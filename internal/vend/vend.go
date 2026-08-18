// Package vend implements the state machine for a single vend transaction:
// authorizing payment, dispensing an item, and every recovery path when
// payment and dispensing disagree. The one invariant that matters more than
// any other: a customer must never end up charged with no item and no
// record of it — every such case surfaces as Refunded or RefundPending,
// never silently as a plain failure.
package vend

import (
	"time"

	"github.com/frida/fridge-edge-agent/internal/dispenser"
)

// Outcome is the terminal state of a vend transaction.
type Outcome string

const (
	// OutcomeSuccess: payment authorized, item dispensed.
	OutcomeSuccess Outcome = "success"
	// OutcomeFailedNoCharge: payment authorization itself failed, so the
	// customer was never charged and nothing was dispensed.
	OutcomeFailedNoCharge Outcome = "failed_no_charge"
	// OutcomeRefunded: payment succeeded, dispensing failed, and the
	// refund was confirmed — customer is made whole.
	OutcomeRefunded Outcome = "refunded"
	// OutcomeRefundPending: payment succeeded, dispensing failed, and the
	// refund attempt itself failed or could not be confirmed. This is the
	// case that needs a human: the customer may be charged with no item.
	OutcomeRefundPending Outcome = "refund_pending"
)

// PaymentGateway is the payment boundary: authorize a charge for a vend
// attempt, and refund a previously authorized charge if dispensing fails.
type PaymentGateway interface {
	Authorize(slotID string, amountCents int) (txnID string, err error)
	Refund(txnID string) error
}

// EventType enumerates the kinds of events a fridge reports to the fleet
// backend.
type EventType string

const (
	EventVendCompleted EventType = "vend_completed"
	EventRestockAlert  EventType = "restock_alert"
	EventHardwareFault EventType = "hardware_fault"
	EventDoorAnomaly   EventType = "door_anomaly"
)

// Event is a single fact a fridge reports to the fleet backend. Payload
// carries event-type-specific detail (e.g. outcome, fault kind).
type Event struct {
	FridgeID  string
	SlotID    string
	Type      EventType
	Timestamp time.Time
	Payload   map[string]any
}

// EventPublisher is how a Machine reports events. In v1 this was an
// in-process callback; in v2 the fleet-side implementation POSTs to the
// fleet backend's ingestion endpoint.
type EventPublisher interface {
	Publish(Event)
}

// Result is the outcome of a single vend attempt.
type Result struct {
	Outcome Outcome
	TxnID   string
	// DispenseErr is set when dispensing failed, regardless of how the
	// resulting payment situation was resolved.
	DispenseErr error
}

// Machine runs vend transactions for one fridge.
type Machine struct {
	FridgeID  string
	Dispenser dispenser.Dispenser
	Payment   PaymentGateway
	Publisher EventPublisher
	// Now returns the current time; defaults to time.Now if nil. Tests
	// override it for deterministic timestamps.
	Now func() time.Time
}

func (m *Machine) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Machine) publish(evt Event) {
	if m.Publisher == nil {
		return
	}
	evt.FridgeID = m.FridgeID
	evt.Timestamp = m.now()
	m.Publisher.Publish(evt)
}

// Vend runs one vend transaction for slotID at amountCents.
func (m *Machine) Vend(slotID string, amountCents int) Result {
	txnID, err := m.Payment.Authorize(slotID, amountCents)
	if err != nil {
		res := Result{Outcome: OutcomeFailedNoCharge}
		m.publish(Event{
			SlotID: slotID,
			Type:   EventVendCompleted,
			Payload: map[string]any{
				"outcome":     string(res.Outcome),
				"auth_error":  err.Error(),
				"amountCents": amountCents,
			},
		})
		return res
	}

	dispenseErr := m.Dispenser.Dispense(slotID)
	if dispenseErr == nil {
		res := Result{Outcome: OutcomeSuccess, TxnID: txnID}
		m.publish(Event{
			SlotID: slotID,
			Type:   EventVendCompleted,
			Payload: map[string]any{
				"outcome":     string(res.Outcome),
				"txnId":       txnID,
				"amountCents": amountCents,
			},
		})
		return res
	}

	m.reportDispenseFault(slotID, dispenseErr)

	refundErr := m.Payment.Refund(txnID)
	res := Result{TxnID: txnID, DispenseErr: dispenseErr}
	if refundErr == nil {
		res.Outcome = OutcomeRefunded
	} else {
		res.Outcome = OutcomeRefundPending
	}

	payload := map[string]any{
		"outcome":     string(res.Outcome),
		"txnId":       txnID,
		"amountCents": amountCents,
		"dispenseErr": dispenseErr.Error(),
	}
	if refundErr != nil {
		payload["refundErr"] = refundErr.Error()
	}
	m.publish(Event{
		SlotID:  slotID,
		Type:    EventVendCompleted,
		Payload: payload,
	})

	return res
}

// reportDispenseFault classifies a dispense error into the event type ops
// needs to see it as: an empty slot is a restock alert, anything else is a
// hardware fault.
func (m *Machine) reportDispenseFault(slotID string, err error) {
	switch {
	case err == dispenser.ErrSlotEmpty:
		m.publish(Event{
			SlotID: slotID,
			Type:   EventRestockAlert,
			Payload: map[string]any{
				"reason": "slot_empty",
			},
		})
	default:
		m.publish(Event{
			SlotID: slotID,
			Type:   EventHardwareFault,
			Payload: map[string]any{
				"error": err.Error(),
			},
		})
	}
}
