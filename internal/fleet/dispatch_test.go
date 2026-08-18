package fleet

import (
	"context"
	"testing"
	"time"
)

func TestClassifySeverity(t *testing.T) {
	tests := []struct {
		name      string
		event     Event
		wantSev   AlertSeverity
		wantAlert bool
	}{
		{
			name:      "hardware fault is high severity",
			event:     Event{Type: EventHardwareFault},
			wantSev:   SeverityHigh,
			wantAlert: true,
		},
		{
			name:      "door anomaly is medium severity",
			event:     Event{Type: EventDoorAnomaly},
			wantSev:   SeverityMedium,
			wantAlert: true,
		},
		{
			name:      "restock alert is low severity",
			event:     Event{Type: EventRestockAlert},
			wantSev:   SeverityLow,
			wantAlert: true,
		},
		{
			name:      "vend completed success does not alert",
			event:     Event{Type: EventVendCompleted, Payload: map[string]any{"outcome": "success"}},
			wantAlert: false,
		},
		{
			name:      "vend completed refund_pending is high severity",
			event:     Event{Type: EventVendCompleted, Payload: map[string]any{"outcome": "refund_pending"}},
			wantSev:   SeverityHigh,
			wantAlert: true,
		},
		{
			name:      "vend completed refunded does not alert",
			event:     Event{Type: EventVendCompleted, Payload: map[string]any{"outcome": "refunded"}},
			wantAlert: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sev, warrants := classifySeverity(tt.event)
			if warrants != tt.wantAlert {
				t.Fatalf("warrants alert = %v, want %v", warrants, tt.wantAlert)
			}
			if warrants && sev != tt.wantSev {
				t.Fatalf("severity = %v, want %v", sev, tt.wantSev)
			}
		})
	}
}

func TestDispatcher_TriageEvent_NoAlertForOrdinaryEvent(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1"})

	alert, err := d.TriageEvent(context.Background(), Event{
		FridgeID: "f1", Type: EventVendCompleted, Payload: map[string]any{"outcome": "success"},
	})
	if err != nil {
		t.Fatalf("TriageEvent() error: %v", err)
	}
	if alert != nil {
		t.Fatalf("TriageEvent() = %+v, want nil (no alert warranted)", alert)
	}
}

func TestDispatcher_FullLifecycle_OpenAssignResolve(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1", "tech-2"})
	ctx := context.Background()

	alert, err := d.TriageEvent(ctx, Event{
		FridgeID: "f1", SlotID: "A1", Type: EventHardwareFault, Timestamp: time.Now(),
		Payload: map[string]any{"error": "jam"},
	})
	if err != nil {
		t.Fatalf("TriageEvent() error: %v", err)
	}
	if alert == nil {
		t.Fatal("TriageEvent() = nil, want an alert for a hardware fault")
	}
	if alert.Status != AlertOpen {
		t.Fatalf("initial Status = %v, want open", alert.Status)
	}

	assigned, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if assigned.Status != AlertAssigned {
		t.Fatalf("Status = %v, want assigned", assigned.Status)
	}
	if assigned.AssignedTo == "" {
		t.Fatal("AssignedTo is empty after Assign()")
	}

	resolved, err := d.Resolve(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if resolved.Status != AlertResolved {
		t.Fatalf("Status = %v, want resolved", resolved.Status)
	}
	if resolved.AssignedTo != assigned.AssignedTo {
		t.Fatalf("AssignedTo changed on resolve: %v -> %v", assigned.AssignedTo, resolved.AssignedTo)
	}
}

func TestDispatcher_Assign_RoundRobin(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1", "tech-2"})
	ctx := context.Background()

	var assignees []string
	for i := 0; i < 4; i++ {
		alert, err := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
		if err != nil || alert == nil {
			t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
		}
		assigned, err := d.Assign(ctx, alert.ID)
		if err != nil {
			t.Fatalf("Assign() error: %v", err)
		}
		assignees = append(assignees, assigned.AssignedTo)
	}

	want := []string{"tech-1", "tech-2", "tech-1", "tech-2"}
	for i, w := range want {
		if assignees[i] != w {
			t.Fatalf("assignees = %v, want round-robin %v", assignees, want)
		}
	}
}

func TestDispatcher_Assign_NoTechsConfigured(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, nil)
	ctx := context.Background()

	alert, _ := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
	if _, err := d.Assign(ctx, alert.ID); err == nil {
		t.Fatal("Assign() with no techs configured should return an error")
	}
}
