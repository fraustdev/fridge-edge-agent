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

func TestDispatcher_Assign_AccessConstrained_NoEligibleTech_MarksBlocked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Chicago", State: "IL", Vertical: "Airport", Lat: 41.97, Lng: -87.9},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	// tech-bob is not cleared for Airport per defaultClearances.
	d := NewDispatcher(s, []string{"tech-bob"})
	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f-airport", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}

	result, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if result.Status != AlertOpen {
		t.Fatalf("Status = %v, want still open (blocked, not assigned)", result.Status)
	}
	if result.AssignedTo != "" {
		t.Fatalf("AssignedTo = %q, want empty", result.AssignedTo)
	}
	if result.BlockedReason == "" {
		t.Fatal("BlockedReason is empty, want a reason explaining no eligible tech")
	}
}

func TestDispatcher_Assign_AccessConstrained_EligibleTechFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Chicago", State: "IL", Vertical: "Airport", Lat: 41.97, Lng: -87.9},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	// tech-alice IS cleared for Airport per defaultClearances.
	d := NewDispatcher(s, []string{"tech-alice"})
	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f-airport", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}

	result, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if result.Status != AlertAssigned {
		t.Fatalf("Status = %v, want assigned", result.Status)
	}
	if result.AssignedTo != "tech-alice" {
		t.Fatalf("AssignedTo = %q, want tech-alice", result.AssignedTo)
	}
	if result.BlockedReason != "" {
		t.Fatalf("BlockedReason = %q, want empty once assigned", result.BlockedReason)
	}
}

func TestDispatcher_AssignNext_PrioritizesHighTierVenueOverFIFO(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC) // off-peak, so peak boost doesn't complicate the comparison

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-office", Status: StatusHealthy, LastEventAt: base,
		Location: &Location{City: "Chicago", State: "IL", Vertical: "Office", Lat: 41.88, Lng: -87.63},
	}); err != nil {
		t.Fatalf("UpsertFridge(f-office) error: %v", err)
	}
	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: base,
		Location: &Location{City: "Dallas", State: "TX", Vertical: "Airport", Lat: 32.9, Lng: -97.04},
	}); err != nil {
		t.Fatalf("UpsertFridge(f-airport) error: %v", err)
	}

	d := NewDispatcher(s, []string{"tech-carol"}) // cleared for everything
	d.Now = func() time.Time { return base }

	// Office alert opened first (older); airport alert opened second
	// (younger) -- plain FIFO/round-robin would grab the office one first.
	officeAlert, err := d.TriageEvent(ctx, Event{FridgeID: "f-office", Type: EventHardwareFault, Timestamp: base})
	if err != nil || officeAlert == nil {
		t.Fatalf("TriageEvent(office) error: %v, alert: %v", err, officeAlert)
	}
	d.Now = func() time.Time { return base.Add(time.Minute) }
	airportAlert, err := d.TriageEvent(ctx, Event{FridgeID: "f-airport", Type: EventHardwareFault, Timestamp: base.Add(time.Minute)})
	if err != nil || airportAlert == nil {
		t.Fatalf("TriageEvent(airport) error: %v, alert: %v", err, airportAlert)
	}

	d.Now = func() time.Time { return base.Add(2 * time.Minute) }
	assigned, err := d.AssignNext(ctx)
	if err != nil {
		t.Fatalf("AssignNext() error: %v", err)
	}
	if assigned == nil {
		t.Fatal("AssignNext() = nil, want the airport alert")
	}
	if assigned.ID != airportAlert.ID {
		t.Fatalf("AssignNext() assigned alert %d (fridge %s), want the airport alert %d despite being opened after the office one",
			assigned.ID, assigned.FridgeID, airportAlert.ID)
	}
	if assigned.Status != AlertAssigned {
		t.Fatalf("Status = %v, want assigned", assigned.Status)
	}

	// The office alert should still be open, untouched, for a later pass.
	stillOpen, err := s.GetAlert(ctx, officeAlert.ID)
	if err != nil {
		t.Fatalf("GetAlert(office) error: %v", err)
	}
	if stillOpen.Status != AlertOpen {
		t.Fatalf("office alert Status = %v, want still open", stillOpen.Status)
	}
}

func TestDispatcher_AssignNext_SkipsBlockedCandidateForNextBest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: now,
		Location: &Location{City: "Dallas", State: "TX", Vertical: "Airport", Lat: 32.9, Lng: -97.04},
	}); err != nil {
		t.Fatalf("UpsertFridge(f-airport) error: %v", err)
	}
	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-office", Status: StatusHealthy, LastEventAt: now,
		Location: &Location{City: "Chicago", State: "IL", Vertical: "Office", Lat: 41.88, Lng: -87.63},
	}); err != nil {
		t.Fatalf("UpsertFridge(f-office) error: %v", err)
	}

	// tech-bob is cleared for Office but not Airport -- the higher-priority
	// airport alert can't be assigned to anyone, so AssignNext should fall
	// through to the office alert instead of giving up.
	d := NewDispatcher(s, []string{"tech-bob"})
	d.Now = func() time.Time { return now }

	airportAlert, err := d.TriageEvent(ctx, Event{FridgeID: "f-airport", Type: EventHardwareFault, Timestamp: now})
	if err != nil || airportAlert == nil {
		t.Fatalf("TriageEvent(airport) error: %v, alert: %v", err, airportAlert)
	}
	officeAlert, err := d.TriageEvent(ctx, Event{FridgeID: "f-office", Type: EventHardwareFault, Timestamp: now})
	if err != nil || officeAlert == nil {
		t.Fatalf("TriageEvent(office) error: %v, alert: %v", err, officeAlert)
	}

	assigned, err := d.AssignNext(ctx)
	if err != nil {
		t.Fatalf("AssignNext() error: %v", err)
	}
	if assigned == nil {
		t.Fatal("AssignNext() = nil, want the office alert")
	}
	if assigned.ID != officeAlert.ID {
		t.Fatalf("AssignNext() assigned alert %d, want the office alert %d", assigned.ID, officeAlert.ID)
	}

	blocked, err := s.GetAlert(ctx, airportAlert.ID)
	if err != nil {
		t.Fatalf("GetAlert(airport) error: %v", err)
	}
	if blocked.Status != AlertOpen || blocked.BlockedReason == "" {
		t.Fatalf("airport alert = %+v, want still open with a blocked reason", blocked)
	}
}
