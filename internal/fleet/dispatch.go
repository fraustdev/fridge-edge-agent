package fleet

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Dispatcher triages incoming events into alerts and carries them through
// the open -> assigned -> resolved lifecycle. Assignment is round-robin
// over a fixed tech list, filtered to techs eligible for the alert's venue
// (see venue.go, techs.go) — a stand-in for a real routing/dispatch
// algorithm, not a claim that this is production-grade logistics (see
// SPEC.md's "explicitly out of scope").
type Dispatcher struct {
	store Store

	mu          sync.Mutex
	techs       []string
	nextTechIdx int

	// Clearances maps a tech name to the venue verticals they're eligible
	// for; a tech absent from the map is treated as cleared for
	// everything. Defaults to defaultClearances (techs.go); override
	// directly for tests that need different clearance rules.
	Clearances map[string]map[string]bool

	// Now overrides the clock (age/peak-window calculations). Defaults to
	// time.Now; set directly in tests for deterministic priority scoring.
	Now func() time.Time
}

// NewDispatcher builds a Dispatcher that assigns alerts round-robin across
// techs (filtered by venue eligibility for access-constrained venues).
// techs must be non-empty for Assign to succeed on an unconstrained venue.
func NewDispatcher(store Store, techs []string) *Dispatcher {
	return &Dispatcher{store: store, techs: techs, Clearances: defaultClearances}
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// classifySeverity maps an event to the severity of the alert it should
// raise, and whether it should raise one at all. restock/hardware/door
// events always raise an alert; a vend completion only raises one when the
// customer may have been charged without receiving an item.
func classifySeverity(e Event) (AlertSeverity, bool) {
	switch e.Type {
	case EventHardwareFault:
		return SeverityHigh, true
	case EventDoorAnomaly:
		return SeverityMedium, true
	case EventRestockAlert:
		return SeverityLow, true
	case EventVendCompleted:
		if outcome, _ := e.Payload["outcome"].(string); outcome == "refund_pending" {
			return SeverityHigh, true
		}
		return "", false
	default:
		return "", false
	}
}

// TriageEvent inspects e and, if it warrants ops attention, creates an open
// Alert for it. Returns (nil, nil) when the event doesn't warrant an alert.
func (d *Dispatcher) TriageEvent(ctx context.Context, e Event) (*Alert, error) {
	severity, warrantsAlert := classifySeverity(e)
	if !warrantsAlert {
		return nil, nil
	}

	now := d.now().UTC()
	alert, err := d.store.CreateAlert(ctx, Alert{
		FridgeID:    e.FridgeID,
		SlotID:      e.SlotID,
		SourceEvent: e.Type,
		Severity:    severity,
		Status:      AlertOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		return nil, fmt.Errorf("create alert: %w", err)
	}
	return &alert, nil
}

// Assign moves an alert from open to assigned, picking the next eligible
// tech in round-robin order. For an access-constrained venue (see venue.go)
// with no eligible tech among d.techs, the alert is NOT assigned -- it
// stays open, marked with a BlockedReason instead of erroring, since "no
// cleared tech available right now" is a real, visible operational state,
// not a bug to hide behind a generic failure.
func (d *Dispatcher) Assign(ctx context.Context, alertID int64) (Alert, error) {
	alert, err := d.store.GetAlert(ctx, alertID)
	if err != nil {
		return Alert{}, fmt.Errorf("get alert: %w", err)
	}

	vertical, err := d.alertVertical(ctx, alert)
	if err != nil {
		return Alert{}, err
	}
	profile := venueProfileFor(vertical)

	if profile.AccessConstrained {
		eligible := d.eligibleTechs(vertical)
		if len(eligible) == 0 {
			blocked, err := d.store.SetAlertBlocked(ctx, alertID, fmt.Sprintf("no tech cleared for %s", vertical))
			if err != nil {
				return Alert{}, fmt.Errorf("mark alert blocked: %w", err)
			}
			return blocked, nil
		}
		return d.assignTo(ctx, alertID, d.nextFrom(eligible))
	}

	tech, err := d.nextTech()
	if err != nil {
		return Alert{}, err
	}
	return d.assignTo(ctx, alertID, tech)
}

// AssignNext scans every open alert, scores each by priorityScore
// (venue criticality tier, alert severity, time open, and an airport
// peak-window boost), and assigns the single highest-priority one that has
// an eligible tech available -- skipping (and marking blocked) any
// higher-priority candidates that are access-constrained with nobody
// eligible, rather than giving up entirely. Returns (nil, nil) if there
// are no open alerts, or if every open alert turned out to be blocked.
func (d *Dispatcher) AssignNext(ctx context.Context) (*Alert, error) {
	open, err := d.store.ListAlerts(ctx, AlertOpen)
	if err != nil {
		return nil, fmt.Errorf("list open alerts: %w", err)
	}
	if len(open) == 0 {
		return nil, nil
	}

	type scored struct {
		alert Alert
		score float64
	}
	candidates := make([]scored, len(open))
	for i, a := range open {
		candidates[i] = scored{alert: a, score: d.priorityOf(ctx, a)}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	for _, c := range candidates {
		assigned, err := d.Assign(ctx, c.alert.ID)
		if err != nil {
			return nil, err
		}
		if assigned.Status == AlertAssigned {
			return &assigned, nil
		}
		// else Assign found it access-constrained with no eligible tech
		// and marked it blocked instead -- try the next candidate.
	}
	return nil, nil
}

// PriorityOf reports an alert's current priority score, for display (see
// AlertsHandler) -- it does not affect assignment ordering by itself,
// AssignNext computes its own scores at assignment time.
func (d *Dispatcher) PriorityOf(ctx context.Context, a Alert) float64 {
	return d.priorityOf(ctx, a)
}

func (d *Dispatcher) priorityOf(ctx context.Context, a Alert) float64 {
	vertical, err := d.alertVertical(ctx, a)
	if err != nil {
		vertical = "" // fall back to the default profile rather than fail a read-only score
	}
	profile := venueProfileFor(vertical)
	now := d.now()
	return priorityScore(profile, a.Severity, now.Sub(a.CreatedAt), now)
}

// alertVertical looks up the venue vertical of the fridge an alert belongs
// to. A fridge with no location data (or a lookup failure) reports "",
// which venueProfileFor treats as the unconstrained default profile.
func (d *Dispatcher) alertVertical(ctx context.Context, a Alert) (string, error) {
	fridge, err := d.store.GetFridge(ctx, a.FridgeID)
	if err != nil {
		if err == ErrNotFound {
			return "", nil
		}
		return "", fmt.Errorf("get fridge for alert venue: %w", err)
	}
	if fridge.Location == nil {
		return "", nil
	}
	return fridge.Location.Vertical, nil
}

func (d *Dispatcher) isCleared(tech, vertical string) bool {
	clearance, known := d.Clearances[tech]
	if !known {
		return true // unknown tech: treated as a generalist, always eligible
	}
	if vertical == "" {
		return true // no venue constraint to check against
	}
	return clearance[vertical]
}

func (d *Dispatcher) eligibleTechs(vertical string) []string {
	var eligible []string
	for _, t := range d.techs {
		if d.isCleared(t, vertical) {
			eligible = append(eligible, t)
		}
	}
	return eligible
}

func (d *Dispatcher) assignTo(ctx context.Context, alertID int64, tech string) (Alert, error) {
	alert, err := d.store.UpdateAlertStatus(ctx, alertID, AlertAssigned, tech)
	if err != nil {
		return Alert{}, fmt.Errorf("assign alert: %w", err)
	}
	return alert, nil
}

func (d *Dispatcher) nextTech() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.techs) == 0 {
		return "", fmt.Errorf("dispatch: no techs configured")
	}
	tech := d.techs[d.nextTechIdx%len(d.techs)]
	d.nextTechIdx++
	return tech, nil
}

// nextFrom picks the next tech from an eligible subset, round-robin. It
// shares the same counter as nextTech -- simple, not perfectly fair across
// differently-sized subsets, which is an acceptable simplification given
// this whole assignment scheme is already a deliberate stand-in for a real
// routing algorithm (see the Dispatcher doc comment).
func (d *Dispatcher) nextFrom(pool []string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	tech := pool[d.nextTechIdx%len(pool)]
	d.nextTechIdx++
	return tech
}

// Resolve marks an assigned alert as resolved.
func (d *Dispatcher) Resolve(ctx context.Context, alertID int64) (Alert, error) {
	existing, err := d.store.GetAlert(ctx, alertID)
	if err != nil {
		return Alert{}, fmt.Errorf("get alert: %w", err)
	}
	alert, err := d.store.UpdateAlertStatus(ctx, alertID, AlertResolved, existing.AssignedTo)
	if err != nil {
		return Alert{}, fmt.Errorf("resolve alert: %w", err)
	}
	return alert, nil
}
