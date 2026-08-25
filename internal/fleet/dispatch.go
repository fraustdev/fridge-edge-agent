package fleet

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Dispatcher triages incoming events into alerts and carries them through
// the open -> assigned -> resolved lifecycle. Assignment filters candidate
// techs by role (see requiredRole) and, for access-constrained venues, by
// individual clearance or escort eligibility (see AccessState), then picks
// the highest-scoring eligible candidate (see scoreLocked) -- a stand-in
// for a real routing/dispatch algorithm, not a claim that this is
// production-grade logistics (see README's "explicitly out of scope").
//
// The tech roster's live state (position, in-flight job, workload) lives
// entirely in memory on the Dispatcher, not in the Store. Dispatcher is a
// single long-lived object for the life of one fleet-server process (like
// the round-robin counter this replaced), so this needs no persistence
// layer of its own -- it simply doesn't survive a server restart, the same
// limitation fridge-sim's daemon mode already documents for its own
// in-memory state.
type Dispatcher struct {
	store Store

	mu       sync.Mutex
	techs    map[string]*Tech
	techIDs  []string // stable iteration/display order
	workload map[string]int

	reassignments  []ReassignmentLogEntry
	nextReassignID int

	// Clearances maps a tech ID to the venue verticals they're individually
	// eligible for; a tech ID absent from the map is treated as cleared for
	// everything. Defaults to defaultClearances (techs.go); override
	// directly for tests that need different clearance rules.
	Clearances map[string]map[string]bool

	// Now overrides the clock (age/peak-window/travel-ETA calculations).
	// Defaults to time.Now; set directly in tests for determinism.
	Now func() time.Time
}

// NewDispatcher builds a Dispatcher over the given tech roster. Pass
// DefaultTechRoster() for the illustrative default roster, or a small
// custom slice in tests.
func NewDispatcher(store Store, techs []Tech) *Dispatcher {
	d := &Dispatcher{
		store:      store,
		techs:      map[string]*Tech{},
		workload:   map[string]int{},
		Clearances: defaultClearances,
	}
	for _, t := range techs {
		tc := t
		d.techs[t.ID] = &tc
		d.techIDs = append(d.techIDs, t.ID)
	}
	return d
}

// DefaultTechRoster returns a fresh copy of the illustrative default tech
// roster (see techs.go) -- fresh so callers (main.go, tests) each get their
// own Tech values to mutate independently.
func DefaultTechRoster() []Tech {
	out := make([]Tech, len(defaultTechRoster))
	copy(out, defaultTechRoster)
	return out
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// requiredRole maps an alert's source event to the tech role that handles
// it: Drivers own restocking (route-based, higher frequency), Service
// Techs own everything else (hardware faults, door anomalies, and
// charged-no-item vend outcomes all need investigation, not a routine
// restock).
func requiredRole(sourceEvent EventType) TechRole {
	if sourceEvent == EventRestockAlert {
		return RoleDriver
	}
	return RoleServiceTech
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

// Assign moves an alert from open to assigned, filtering candidates by role
// (requiredRole) and, for an access-constrained venue (see venue.go),
// resolving one of the three AccessState outcomes. When no eligible
// candidate exists at all, the alert is NOT assigned -- it stays open,
// marked with a BlockedReason instead of erroring, since "nobody available
// right now" is a real, visible operational state, not a bug to hide
// behind a generic failure.
func (d *Dispatcher) Assign(ctx context.Context, alertID int64) (Alert, error) {
	alert, err := d.store.GetAlert(ctx, alertID)
	if err != nil {
		return Alert{}, fmt.Errorf("get alert: %w", err)
	}

	fridge, vertical, err := d.alertFridgeAndVertical(ctx, alert)
	if err != nil {
		return Alert{}, err
	}
	profile := venueProfileFor(vertical)
	role := requiredRole(alert.SourceEvent)

	d.mu.Lock()
	defer d.mu.Unlock()

	roleMatched := d.techsByRoleLocked(role)

	if !profile.AccessConstrained {
		if len(roleMatched) == 0 {
			return d.blockLocked(ctx, alertID, fmt.Sprintf("no %s techs configured", role))
		}
		best := d.pickBestLocked(roleMatched, fridge, profile.Tier)
		return d.commitAssignmentLocked(ctx, alertID, fridge, best, "", "", alert.AssignedTo)
	}

	// Access-constrained venue: individually-cleared, role-matched
	// candidates get dispatched normally.
	clearedRoleMatched := d.filterClearedLocked(roleMatched, vertical)
	if len(clearedRoleMatched) > 0 {
		best := d.pickBestLocked(clearedRoleMatched, fridge, profile.Tier)
		return d.commitAssignmentLocked(ctx, alertID, fridge, best, AccessAssigned, "", alert.AssignedTo)
	}

	// No individually-cleared role-matched tech. If a role-matched
	// (uncleared) tech exists AND some cleared tech of any role exists to
	// escort them in, that's still workable -- just slower.
	anyCleared := d.filterClearedLocked(d.allTechsLocked(), vertical)
	if len(roleMatched) > 0 && len(anyCleared) > 0 {
		worker := d.pickBestLocked(roleMatched, fridge, profile.Tier)
		escort := d.pickBestLocked(anyCleared, fridge, profile.Tier)
		return d.commitAssignmentLocked(ctx, alertID, fridge, worker, AccessEscortRequired, escort.tech.ID, alert.AssignedTo)
	}

	reason := fmt.Sprintf("no tech cleared for %s (and no escort available)", vertical)
	if len(roleMatched) == 0 {
		reason = fmt.Sprintf("no %s tech available for %s", role, vertical)
	}
	return d.blockLocked(ctx, alertID, reason)
}

// AssignNext scans every open alert, scores each by priorityScore
// (venue criticality tier, alert severity, time open, and an airport
// peak-window boost), and assigns the single highest-priority one that has
// an eligible tech available -- skipping (and marking blocked) any
// higher-priority candidates nobody can currently take, rather than giving
// up entirely. Returns (nil, nil) if there are no open alerts, or if every
// open alert turned out to be blocked.
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
		// else Assign found nobody available and marked it blocked --
		// try the next candidate.
	}
	return nil, nil
}

// Reassign moves alertID's assignment to a specific tech, overriding
// whatever Assign/AssignNext (or a prior Reassign) had chosen -- a
// dispatcher-facing manual override. The prior AssignedTo is preserved in
// the returned log entry (see Reassignments) so the override is comparable
// against what auto-assignment had picked. by identifies who made the
// override; it's free text since this demo has no real user/auth system.
func (d *Dispatcher) Reassign(ctx context.Context, alertID int64, techID, by string) (Alert, error) {
	alert, err := d.store.GetAlert(ctx, alertID)
	if err != nil {
		return Alert{}, fmt.Errorf("get alert: %w", err)
	}
	fridge, vertical, err := d.alertFridgeAndVertical(ctx, alert)
	if err != nil {
		return Alert{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	tech, ok := d.techs[techID]
	if !ok {
		return Alert{}, fmt.Errorf("dispatch: unknown tech %q", techID)
	}

	tier := venueProfileFor(vertical).Tier
	cs := candidateScore{tech: tech, score: d.scoreLocked(tech, fridge, tier)}
	updated, err := d.commitAssignmentLocked(ctx, alertID, fridge, cs, alert.AccessState, alert.EscortTech, alert.AssignedTo)
	if err != nil {
		return Alert{}, fmt.Errorf("reassign alert: %w", err)
	}

	d.nextReassignID++
	d.reassignments = append(d.reassignments, ReassignmentLogEntry{
		ID:       d.nextReassignID,
		AlertID:  alertID,
		FromTech: alert.AssignedTo,
		ToTech:   techID,
		By:       by,
		At:       d.now(),
	})

	return updated, nil
}

// Reassignments returns the manual-override audit log for one alert,
// oldest first.
func (d *Dispatcher) Reassignments(alertID int64) []ReassignmentLogEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []ReassignmentLogEntry
	for _, r := range d.reassignments {
		if r.AlertID == alertID {
			out = append(out, r)
		}
	}
	return out
}

// Techs returns a live, read-time-computed view of every tech in the
// roster: current interpolated position, idle/busy state, and workload.
func (d *Dispatcher) Techs() []TechView {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	views := make([]TechView, 0, len(d.techIDs))
	for _, id := range d.techIDs {
		t := d.techs[id]
		lat, lng := t.currentPosition(now)
		idle := t.assignedAt.IsZero()
		v := TechView{
			Tech:     *t,
			Lat:      lat,
			Lng:      lng,
			Idle:     idle,
			Workload: d.workload[id],
		}
		if !idle {
			eta := t.eta
			v.ETA = &eta
			v.CurrentAlertID = t.currentAlertID
		}
		views = append(views, v)
	}
	return views
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

// alertFridgeAndVertical looks up the fridge (for coordinates) and venue
// vertical (for the dispatch profile) an alert belongs to. A fridge with no
// location data (or a lookup failure) reports "", which venueProfileFor
// treats as the unconstrained default profile.
func (d *Dispatcher) alertFridgeAndVertical(ctx context.Context, a Alert) (Fridge, string, error) {
	fridge, err := d.store.GetFridge(ctx, a.FridgeID)
	if err != nil {
		if err == ErrNotFound {
			return Fridge{}, "", nil
		}
		return Fridge{}, "", fmt.Errorf("get fridge for alert venue: %w", err)
	}
	if fridge.Location == nil {
		return fridge, "", nil
	}
	return fridge, fridge.Location.Vertical, nil
}

func (d *Dispatcher) alertVertical(ctx context.Context, a Alert) (string, error) {
	_, vertical, err := d.alertFridgeAndVertical(ctx, a)
	return vertical, err
}

func (d *Dispatcher) isCleared(techID, vertical string) bool {
	clearance, known := d.Clearances[techID]
	if !known {
		return true // unknown tech: treated as a generalist, always eligible
	}
	if vertical == "" {
		return true // no venue constraint to check against
	}
	return clearance[vertical]
}

func (d *Dispatcher) allTechsLocked() []*Tech {
	out := make([]*Tech, 0, len(d.techIDs))
	for _, id := range d.techIDs {
		out = append(out, d.techs[id])
	}
	return out
}

func (d *Dispatcher) techsByRoleLocked(role TechRole) []*Tech {
	var out []*Tech
	for _, t := range d.allTechsLocked() {
		if t.Role == role {
			out = append(out, t)
		}
	}
	return out
}

func (d *Dispatcher) filterClearedLocked(techs []*Tech, vertical string) []*Tech {
	var out []*Tech
	for _, t := range techs {
		if d.isCleared(t.ID, vertical) {
			out = append(out, t)
		}
	}
	return out
}

// scoreWeights lets a venue's criticality tier shift how heavily travel
// time vs. workload-balancing count toward a candidate's score.
type scoreWeights struct{ travel, workload float64 }

// scoreWeightsByTier: a high-tier (airport/healthcare) alert should get
// there fast even if it means picking a busier tech; a low-tier alert can
// afford to prioritize spreading work out instead. A simple weighted sum,
// not a trained/learned model -- see README's "explicitly out of scope".
var scoreWeightsByTier = map[CriticalityTier]scoreWeights{
	TierHigh:   {travel: 1.5, workload: 0.5},
	TierMedium: {travel: 1.0, workload: 1.0},
	TierLow:    {travel: 0.6, workload: 1.4},
}

// workloadCostMinutes is a fixed per-queued-job cost, expressed in the same
// "minutes" unit as travel time so the two factors are comparable -- a
// rough proxy for "how much longer until this tech is free for a new job,"
// not a real queueing model.
const workloadCostMinutes = 20.0

// avgTravelSpeedMPH converts straight-line (haversine) distance into an
// estimated drive time using a flat average-speed assumption. A real
// routing API (e.g. OSRM) is a documented upgrade path, not required for
// this build -- see README.
const avgTravelSpeedMPH = 32.0

func travelMinutesFor(miles float64) float64 {
	return miles / avgTravelSpeedMPH * 60
}

// haversineMiles is the great-circle distance between two lat/lng points.
func haversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMiles = 3958.8
	toRad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLng := toRad(lng2 - lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMiles * c
}

// candidateScore pairs a candidate tech with its computed score (higher is
// better, i.e. an inverted cost -- see scoreLocked) so the winning score
// can be persisted onto the alert alongside who won.
type candidateScore struct {
	tech  *Tech
	score float64
}

// scoreLocked scores one candidate tech for one alert: higher is better.
// It's expressed as a negated cost (weighted travel time + weighted
// workload penalty) so "highest score wins" stays consistent with
// priorityScore's convention elsewhere in this package. Must be called
// with d.mu held (reads workload and tech position).
func (d *Dispatcher) scoreLocked(tech *Tech, fridge Fridge, tier CriticalityTier) float64 {
	w, ok := scoreWeightsByTier[tier]
	if !ok {
		w = scoreWeightsByTier[TierMedium]
	}

	lat, lng := tech.currentPosition(d.now())
	var travelMinutes float64
	if fridge.Location != nil {
		travelMinutes = travelMinutesFor(haversineMiles(lat, lng, fridge.Location.Lat, fridge.Location.Lng))
	}

	cost := w.travel*travelMinutes + w.workload*float64(d.workload[tech.ID])*workloadCostMinutes
	return -cost
}

// pickBestLocked scores every candidate and returns the highest-scoring
// one. candidates must be non-empty. Must be called with d.mu held.
func (d *Dispatcher) pickBestLocked(candidates []*Tech, fridge Fridge, tier CriticalityTier) candidateScore {
	best := candidateScore{score: math.Inf(-1)}
	for _, t := range candidates {
		s := d.scoreLocked(t, fridge, tier)
		if best.tech == nil || s > best.score {
			best = candidateScore{tech: t, score: s}
		}
	}
	return best
}

// blockLocked records why an alert couldn't be assigned without changing
// its status -- it stays open, just with a visible reason. Must be called
// with d.mu held (for call-site consistency with the rest of Assign; it
// doesn't itself touch in-memory state).
func (d *Dispatcher) blockLocked(ctx context.Context, alertID int64, reason string) (Alert, error) {
	blocked, err := d.store.SetAlertBlocked(ctx, alertID, reason)
	if err != nil {
		return Alert{}, fmt.Errorf("mark alert blocked: %w", err)
	}
	return blocked, nil
}

// commitAssignmentLocked is the single place that mutates tech travel
// state, workload bookkeeping, and the persisted alert for any assignment
// (auto via Assign, or manual via Reassign). prevAssignedTo is the alert's
// AssignedTo before this call -- "" for a fresh assignment, non-empty for
// a reassignment -- so the previous tech's slot is released exactly once.
// Must be called with d.mu held.
func (d *Dispatcher) commitAssignmentLocked(ctx context.Context, alertID int64, fridge Fridge, cs candidateScore, accessState AccessState, escortID, prevAssignedTo string) (Alert, error) {
	tech := cs.tech
	now := d.now()

	if prevAssignedTo != "" && prevAssignedTo != tech.ID {
		if prevTech, ok := d.techs[prevAssignedTo]; ok && prevTech.currentAlertID == alertID {
			d.releaseTechLocked(prevTech)
		}
		if d.workload[prevAssignedTo] > 0 {
			d.workload[prevAssignedTo]--
		}
	}

	lat, lng := tech.currentPosition(now)
	tech.originLat, tech.originLng = lat, lng
	if fridge.Location != nil {
		tech.destLat, tech.destLng = fridge.Location.Lat, fridge.Location.Lng
		miles := haversineMiles(lat, lng, fridge.Location.Lat, fridge.Location.Lng)
		tech.assignedAt = now
		tech.eta = now.Add(time.Duration(travelMinutesFor(miles) * float64(time.Minute)))
	} else {
		// No coordinates to travel toward -- stay put, but still record an
		// active job so workload/idle state reflect the assignment.
		tech.destLat, tech.destLng = lat, lng
		tech.assignedAt = now
		tech.eta = now
	}
	tech.currentAlertID = alertID

	if prevAssignedTo != tech.ID {
		d.workload[tech.ID]++
	}

	updated, err := d.store.SetAssignment(ctx, alertID, tech.ID, cs.score, accessState, escortID)
	if err != nil {
		return Alert{}, fmt.Errorf("assign alert: %w", err)
	}
	return updated, nil
}

// releaseTechLocked returns a tech to idle (parked at home, per
// currentPosition's zero-value handling). Must be called with d.mu held.
func (d *Dispatcher) releaseTechLocked(t *Tech) {
	t.originLat, t.originLng = 0, 0
	t.destLat, t.destLng = 0, 0
	t.assignedAt = time.Time{}
	t.eta = time.Time{}
	t.currentAlertID = 0
}

// currentPosition interpolates a tech's live map position between its last
// travel origin and destination, based on elapsed time toward its ETA.
// Computed at read time (like effectiveStatus and priorityScore elsewhere
// in this package) rather than updated by a background loop -- there's no
// per-tick mutation to keep in sync, a read simply reflects "now".
func (t Tech) currentPosition(now time.Time) (lat, lng float64) {
	if t.assignedAt.IsZero() || t.eta.IsZero() || !now.Before(t.eta) {
		if t.destLat != 0 || t.destLng != 0 {
			return t.destLat, t.destLng
		}
		return t.HomeLat, t.HomeLng
	}
	total := t.eta.Sub(t.assignedAt)
	if total <= 0 {
		return t.destLat, t.destLng
	}
	frac := float64(now.Sub(t.assignedAt)) / float64(total)
	if frac < 0 {
		frac = 0
	}
	return t.originLat + (t.destLat-t.originLat)*frac, t.originLng + (t.destLng-t.originLng)*frac
}

// Resolve marks an assigned alert as resolved and frees its tech (back to
// idle, parked at home -- see releaseTechLocked) if one was assigned.
func (d *Dispatcher) Resolve(ctx context.Context, alertID int64) (Alert, error) {
	existing, err := d.store.GetAlert(ctx, alertID)
	if err != nil {
		return Alert{}, fmt.Errorf("get alert: %w", err)
	}
	alert, err := d.store.UpdateAlertStatus(ctx, alertID, AlertResolved, existing.AssignedTo)
	if err != nil {
		return Alert{}, fmt.Errorf("resolve alert: %w", err)
	}

	if existing.AssignedTo != "" {
		d.mu.Lock()
		if t, ok := d.techs[existing.AssignedTo]; ok && t.currentAlertID == alertID {
			d.releaseTechLocked(t)
		}
		if d.workload[existing.AssignedTo] > 0 {
			d.workload[existing.AssignedTo]--
		}
		d.mu.Unlock()
	}
	return alert, nil
}
