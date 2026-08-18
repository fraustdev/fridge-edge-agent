package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Dispatcher triages incoming events into alerts and carries them through
// the open -> assigned -> resolved lifecycle. Assignment is deliberately
// simple round-robin over a fixed tech list — a stand-in for a real
// routing/dispatch algorithm, not a claim that this is production-grade
// logistics (see SPEC.md's "explicitly out of scope").
type Dispatcher struct {
	store Store

	mu          sync.Mutex
	techs       []string
	nextTechIdx int
}

// NewDispatcher builds a Dispatcher that assigns alerts round-robin across
// techs. techs must be non-empty for Assign to succeed.
func NewDispatcher(store Store, techs []string) *Dispatcher {
	return &Dispatcher{store: store, techs: techs}
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

	now := time.Now().UTC()
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

// Assign moves an alert from open to assigned, picking the next tech in
// round-robin order.
func (d *Dispatcher) Assign(ctx context.Context, alertID int64) (Alert, error) {
	tech, err := d.nextTech()
	if err != nil {
		return Alert{}, err
	}
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
