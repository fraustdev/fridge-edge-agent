package vend

import (
	"errors"
	"testing"
	"time"

	"github.com/frida/fridge-edge-agent/internal/dispenser"
)

type fakePayment struct {
	authorizeErr   error
	refundErr      error
	nextTxnID      string
	authorizeCalls int
	refundCalls    int
}

func (f *fakePayment) Authorize(slotID string, amountCents int) (string, error) {
	f.authorizeCalls++
	if f.authorizeErr != nil {
		return "", f.authorizeErr
	}
	if f.nextTxnID == "" {
		return "txn-1", nil
	}
	return f.nextTxnID, nil
}

func (f *fakePayment) Refund(txnID string) error {
	f.refundCalls++
	return f.refundErr
}

type recordingPublisher struct {
	events []Event
}

func (r *recordingPublisher) Publish(e Event) {
	r.events = append(r.events, e)
}

func (r *recordingPublisher) types() []EventType {
	var out []EventType
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

func newMachine(disp dispenser.Dispenser, pay PaymentGateway, pub *recordingPublisher) *Machine {
	return &Machine{
		FridgeID:  "fridge-1",
		Dispenser: disp,
		Payment:   pay,
		Publisher: pub,
		Now:       func() time.Time { return time.Unix(0, 0) },
	}
}

func TestMachine_Vend(t *testing.T) {
	tests := []struct {
		name           string
		authorizeErr   error
		dispenseFault  error // injected into the simulator for slot A1
		refundErr      error
		wantOutcome    Outcome
		wantEventTypes []EventType
		wantRefundCall bool
	}{
		{
			name:           "auth fails: no charge, no dispense attempt",
			authorizeErr:   errors.New("card declined"),
			wantOutcome:    OutcomeFailedNoCharge,
			wantEventTypes: []EventType{EventVendCompleted},
			wantRefundCall: false,
		},
		{
			name:           "auth and dispense both succeed",
			wantOutcome:    OutcomeSuccess,
			wantEventTypes: []EventType{EventVendCompleted},
			wantRefundCall: false,
		},
		{
			name:           "dispense fails empty slot, refund succeeds",
			dispenseFault:  dispenser.ErrSlotEmpty,
			wantOutcome:    OutcomeRefunded,
			wantEventTypes: []EventType{EventRestockAlert, EventVendCompleted},
			wantRefundCall: true,
		},
		{
			name:           "dispense fails jam, refund succeeds",
			dispenseFault:  dispenser.ErrJam,
			wantOutcome:    OutcomeRefunded,
			wantEventTypes: []EventType{EventHardwareFault, EventVendCompleted},
			wantRefundCall: true,
		},
		{
			name:           "dispense fails jam, refund also fails: refund_pending",
			dispenseFault:  dispenser.ErrJam,
			refundErr:      errors.New("payment processor unreachable"),
			wantOutcome:    OutcomeRefundPending,
			wantEventTypes: []EventType{EventHardwareFault, EventVendCompleted},
			wantRefundCall: true,
		},
		{
			name:           "dispense fails timeout, refund succeeds",
			dispenseFault:  dispenser.ErrTimeout,
			wantOutcome:    OutcomeRefunded,
			wantEventTypes: []EventType{EventHardwareFault, EventVendCompleted},
			wantRefundCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := dispenser.NewSimulator(map[string]int{"A1": 5})
			if tt.dispenseFault != nil {
				sim.InjectFault("A1", tt.dispenseFault)
			}
			pay := &fakePayment{authorizeErr: tt.authorizeErr, refundErr: tt.refundErr}
			pub := &recordingPublisher{}
			m := newMachine(sim, pay, pub)

			res := m.Vend("A1", 350)

			if res.Outcome != tt.wantOutcome {
				t.Fatalf("Outcome = %v, want %v", res.Outcome, tt.wantOutcome)
			}
			if pay.refundCalls > 0 != tt.wantRefundCall {
				t.Fatalf("refund called = %v, want %v", pay.refundCalls > 0, tt.wantRefundCall)
			}

			gotTypes := pub.types()
			if len(gotTypes) != len(tt.wantEventTypes) {
				t.Fatalf("published events = %v, want types %v", gotTypes, tt.wantEventTypes)
			}
			for i, wantType := range tt.wantEventTypes {
				if gotTypes[i] != wantType {
					t.Fatalf("event[%d].Type = %v, want %v (all: %v)", i, gotTypes[i], wantType, gotTypes)
				}
			}
		})
	}
}

// TestMachine_Vend_RefundPending_NeverLooksLikeAnOrdinaryFailure is the one
// test that guards the whole project's most important correctness property:
// "charged but got nothing" must always be distinguishable in the published
// event, never collapsed into a generic failure outcome.
func TestMachine_Vend_RefundPending_NeverLooksLikeAnOrdinaryFailure(t *testing.T) {
	sim := dispenser.NewSimulator(map[string]int{"A1": 5})
	sim.InjectFault("A1", dispenser.ErrJam)
	pay := &fakePayment{refundErr: errors.New("processor down")}
	pub := &recordingPublisher{}
	m := newMachine(sim, pay, pub)

	res := m.Vend("A1", 500)

	if res.Outcome != OutcomeRefundPending {
		t.Fatalf("Outcome = %v, want %v", res.Outcome, OutcomeRefundPending)
	}
	if res.TxnID == "" {
		t.Fatal("TxnID must be preserved on refund_pending so ops can reconcile the charge")
	}

	var vendEvent *Event
	for i := range pub.events {
		if pub.events[i].Type == EventVendCompleted {
			vendEvent = &pub.events[i]
		}
	}
	if vendEvent == nil {
		t.Fatal("expected a vend_completed event to be published")
	}
	if vendEvent.Payload["outcome"] != string(OutcomeRefundPending) {
		t.Fatalf("vend_completed payload outcome = %v, want %v", vendEvent.Payload["outcome"], OutcomeRefundPending)
	}
	if _, ok := vendEvent.Payload["refundErr"]; !ok {
		t.Fatal("vend_completed payload must record why the refund failed")
	}
}

// TestMachine_Vend_RunningLowFiresOnceOnThresholdCrossing exercises
// dispenser.LowStockSlots' wiring into the vend flow: a restock_alert with
// reason "running_low" must fire the moment a successful dispense brings a
// slot to or below lowStockThreshold, but not again on every subsequent
// dispense while it stays low -- only once, on the crossing.
func TestMachine_Vend_RunningLowFiresOnceOnThresholdCrossing(t *testing.T) {
	// lowStockThreshold is 3; start at 4 so the first dispense (4 -> 3)
	// crosses it and the second (3 -> 2) doesn't cross again.
	sim := dispenser.NewSimulator(map[string]int{"A1": 4})
	pay := &fakePayment{}
	pub := &recordingPublisher{}
	m := newMachine(sim, pay, pub)

	res := m.Vend("A1", 350)
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("first Vend() Outcome = %v, want success", res.Outcome)
	}
	wantFirst := []EventType{EventVendCompleted, EventRestockAlert}
	if got := pub.types(); !equalEventTypes(got, wantFirst) {
		t.Fatalf("events after crossing dispense = %v, want %v", got, wantFirst)
	}
	var runningLow *Event
	for i := range pub.events {
		if pub.events[i].Type == EventRestockAlert {
			runningLow = &pub.events[i]
		}
	}
	if runningLow == nil || runningLow.Payload["reason"] != "running_low" {
		t.Fatalf("expected a restock_alert with reason=running_low, got %+v", runningLow)
	}

	pub.events = nil // reset to isolate the second dispense's events
	res = m.Vend("A1", 350)
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("second Vend() Outcome = %v, want success", res.Outcome)
	}
	wantSecond := []EventType{EventVendCompleted}
	if got := pub.types(); !equalEventTypes(got, wantSecond) {
		t.Fatalf("events after already-low dispense = %v, want %v (no duplicate running_low alert)", got, wantSecond)
	}
}

func equalEventTypes(a, b []EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMachine_Vend_UnknownSlot(t *testing.T) {
	sim := dispenser.NewSimulator(map[string]int{"A1": 5})
	pay := &fakePayment{}
	pub := &recordingPublisher{}
	m := newMachine(sim, pay, pub)

	res := m.Vend("Z9", 100)

	if res.Outcome != OutcomeRefunded && res.Outcome != OutcomeRefundPending {
		t.Fatalf("Outcome = %v, want refunded or refund_pending for a dispense-side error", res.Outcome)
	}
}
