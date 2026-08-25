package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestComputeStatus_HealsOnSuccessOnlyWithoutOpenAlert(t *testing.T) {
	successEvent := Event{Type: EventVendCompleted, Payload: map[string]any{"outcome": "success"}}

	tests := []struct {
		name         string
		current      FridgeStatus
		event        Event
		hasOpenAlert bool
		want         FridgeStatus
	}{
		{
			name:         "faulted heals to healthy on success with no open alert",
			current:      StatusFaulted,
			event:        successEvent,
			hasOpenAlert: false,
			want:         StatusHealthy,
		},
		{
			name:         "faulted stays faulted on success while an alert is still open",
			current:      StatusFaulted,
			event:        successEvent,
			hasOpenAlert: true,
			want:         StatusFaulted,
		},
		{
			name:         "low_stock heals to healthy on success with no open alert",
			current:      StatusLowStock,
			event:        successEvent,
			hasOpenAlert: false,
			want:         StatusHealthy,
		},
		{
			name:         "low_stock stays low_stock on success while an alert is still open",
			current:      StatusLowStock,
			event:        successEvent,
			hasOpenAlert: true,
			want:         StatusLowStock,
		},
		{
			name:         "already healthy stays healthy on success regardless of hasOpenAlert",
			current:      StatusHealthy,
			event:        successEvent,
			hasOpenAlert: false,
			want:         StatusHealthy,
		},
		{
			name:         "no prior status becomes healthy on first success",
			current:      "",
			event:        successEvent,
			hasOpenAlert: false,
			want:         StatusHealthy,
		},
		{
			name:         "hardware fault always faults, regardless of hasOpenAlert",
			current:      StatusHealthy,
			event:        Event{Type: EventHardwareFault},
			hasOpenAlert: false,
			want:         StatusFaulted,
		},
		{
			name:         "refund_pending faults even with hasOpenAlert=false",
			current:      StatusHealthy,
			event:        Event{Type: EventVendCompleted, Payload: map[string]any{"outcome": "refund_pending"}},
			hasOpenAlert: false,
			want:         StatusFaulted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeStatus(tt.current, tt.event, tt.hasOpenAlert)
			if got != tt.want {
				t.Fatalf("computeStatus(%q, %+v, hasOpenAlert=%v) = %v, want %v", tt.current, tt.event, tt.hasOpenAlert, got, tt.want)
			}
		})
	}
}

// TestIngestHandler_HealsOnlyAfterOpenAlertResolved exercises the real
// wiring end-to-end (not just computeStatus in isolation): a hardware
// fault opens an alert and faults the fridge; a subsequent successful
// vend must NOT heal it while that alert is still open; resolving the
// alert and then reporting another successful vend must heal it.
func TestIngestHandler_HealsOnlyAfterOpenAlertResolved(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []Tech{testTech("tech-1", RoleServiceTech, 0, 0)})
	h := NewIngestHandler(s, d)
	ctx := context.Background()

	post := func(t *testing.T, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/fleet/events", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /fleet/events status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
		}
	}

	// Hardware fault: fridge faults, an alert opens.
	post(t, `{"fridgeId":"f1","slotId":"A1","type":"hardware_fault","payload":{"error":"jam"}}`)

	fridge, err := s.GetFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("GetFridge() error: %v", err)
	}
	if fridge.Status != StatusFaulted {
		t.Fatalf("status after hardware_fault = %v, want faulted", fridge.Status)
	}
	openAlerts, err := s.ListOpenAlertsForFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("ListOpenAlertsForFridge() error: %v", err)
	}
	if len(openAlerts) != 1 {
		t.Fatalf("open alerts after hardware_fault = %d, want 1", len(openAlerts))
	}
	alertID := openAlerts[0].ID

	// A successful vend while the alert is still open must NOT heal it.
	post(t, `{"fridgeId":"f1","slotId":"A2","type":"vend_completed","payload":{"outcome":"success"}}`)
	fridge, err = s.GetFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("GetFridge() error: %v", err)
	}
	if fridge.Status != StatusFaulted {
		t.Fatalf("status after success vend WITH open alert = %v, want still faulted", fridge.Status)
	}

	// Resolve the alert, then report another successful vend -- now it
	// should heal.
	if _, err := d.Resolve(ctx, alertID); err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	post(t, `{"fridgeId":"f1","slotId":"A2","type":"vend_completed","payload":{"outcome":"success"}}`)
	fridge, err = s.GetFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("GetFridge() error: %v", err)
	}
	if fridge.Status != StatusHealthy {
		t.Fatalf("status after success vend WITHOUT open alert = %v, want healthy", fridge.Status)
	}
}

