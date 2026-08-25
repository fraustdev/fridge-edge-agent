package fleet

import (
	"context"
	"testing"
	"time"
)

func testTech(id string, role TechRole, lat, lng float64) Tech {
	return Tech{ID: id, Name: id, Role: role, HomeLat: lat, HomeLng: lng}
}

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
	d := NewDispatcher(s, []Tech{testTech("tech-1", RoleServiceTech, 0, 0)})

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
	d := NewDispatcher(s, []Tech{testTech("tech-1", RoleServiceTech, 0, 0), testTech("tech-2", RoleServiceTech, 0, 0)})
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

// Round-robin is no longer a distinct algorithm -- but with two otherwise
// identical ServiceTechs (same position, same tier) and no travel-time
// difference (the fridge has no location), pure workload-balancing scoring
// reproduces the same alternation a dedicated round-robin would have
// produced, tie-broken by roster order.
func TestDispatcher_Assign_WorkloadBalancingAlternatesEvenlyMatchedTechs(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []Tech{testTech("tech-1", RoleServiceTech, 0, 0), testTech("tech-2", RoleServiceTech, 0, 0)})
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
			t.Fatalf("assignees = %v, want alternating %v", assignees, want)
		}
	}
}

func TestDispatcher_Assign_NoRoleMatchedTech_MarksBlocked(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, nil)
	ctx := context.Background()

	alert, _ := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
	result, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if result.Status != AlertOpen {
		t.Fatalf("Status = %v, want still open (blocked, not assigned)", result.Status)
	}
	if result.BlockedReason == "" {
		t.Fatal("BlockedReason is empty, want a reason explaining no tech is configured")
	}
}

func TestDispatcher_Assign_RoutesByRole(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := NewDispatcher(s, []Tech{
		testTech("tech-driver", RoleDriver, 0, 0),
		testTech("tech-svc", RoleServiceTech, 0, 0),
	})

	cases := []struct {
		name     string
		event    Event
		wantTech string
	}{
		{"restock routes to Driver", Event{FridgeID: "f1", Type: EventRestockAlert, Timestamp: time.Now()}, "tech-driver"},
		{"hardware fault routes to ServiceTech", Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()}, "tech-svc"},
		{"door anomaly routes to ServiceTech", Event{FridgeID: "f1", Type: EventDoorAnomaly, Timestamp: time.Now()}, "tech-svc"},
		{"charged-no-item routes to ServiceTech", Event{FridgeID: "f1", Type: EventVendCompleted, Payload: map[string]any{"outcome": "refund_pending"}, Timestamp: time.Now()}, "tech-svc"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			alert, err := d.TriageEvent(ctx, c.event)
			if err != nil || alert == nil {
				t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
			}
			assigned, err := d.Assign(ctx, alert.ID)
			if err != nil {
				t.Fatalf("Assign() error: %v", err)
			}
			if assigned.AssignedTo != c.wantTech {
				t.Fatalf("AssignedTo = %q, want %q", assigned.AssignedTo, c.wantTech)
			}
		})
	}
}

func TestDispatcher_Assign_AccessConstrained_Assigned(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Chicago", State: "IL", Vertical: "Airport", Lat: 41.97, Lng: -87.9},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	// tech-alice IS cleared for Airport per defaultClearances.
	d := NewDispatcher(s, []Tech{testTech("tech-alice", RoleServiceTech, 41.97, -87.9)})
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
	if result.AccessState != AccessAssigned {
		t.Fatalf("AccessState = %v, want assigned", result.AccessState)
	}
	if result.BlockedReason != "" {
		t.Fatalf("BlockedReason = %q, want empty once assigned", result.BlockedReason)
	}
}

func TestDispatcher_Assign_AccessConstrained_EscortRequired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Dallas", State: "TX", Vertical: "Airport", Lat: 32.9, Lng: -97.04},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	// tech-svc is role-matched (ServiceTech) but not individually cleared
	// for Airport; tech-driver IS cleared for Airport but can't do
	// ServiceTech work themself -- they can only escort tech-svc in.
	d := NewDispatcher(s, []Tech{
		testTech("tech-svc", RoleServiceTech, 32.9, -97.0),
		testTech("tech-driver", RoleDriver, 32.9, -97.0),
	})
	d.Clearances = map[string]map[string]bool{
		"tech-svc":    {},
		"tech-driver": {"Airport": true},
	}

	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f-airport", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}

	result, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if result.Status != AlertAssigned {
		t.Fatalf("Status = %v, want assigned (with an escort)", result.Status)
	}
	if result.AccessState != AccessEscortRequired {
		t.Fatalf("AccessState = %v, want escort_required", result.AccessState)
	}
	if result.AssignedTo != "tech-svc" {
		t.Fatalf("AssignedTo = %q, want the role-matched tech-svc", result.AssignedTo)
	}
	if result.EscortTech != "tech-driver" {
		t.Fatalf("EscortTech = %q, want tech-driver", result.EscortTech)
	}
}

func TestDispatcher_Assign_AccessConstrained_Blocked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f-airport", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Dallas", State: "TX", Vertical: "Airport", Lat: 32.9, Lng: -97.04},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	// tech-svc is role-matched but nobody in the roster (any role) is
	// cleared for Airport -- no escort is possible either.
	d := NewDispatcher(s, []Tech{testTech("tech-svc", RoleServiceTech, 32.9, -97.0)})
	d.Clearances = map[string]map[string]bool{"tech-svc": {}}

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
		t.Fatal("BlockedReason is empty, want a reason explaining no tech or escort is available")
	}
	if result.AccessState != "" {
		t.Fatalf("AccessState = %v, want empty (access was never resolved)", result.AccessState)
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

	d := NewDispatcher(s, []Tech{testTech("tech-carol", RoleServiceTech, 32.9, -97.04)}) // cleared for everything per defaultClearances
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

	// tech-bob is cleared for Office but not Airport (nor is anyone else in
	// this roster) -- the higher-priority airport alert can't be assigned
	// or escorted, so AssignNext should fall through to the office alert
	// instead of giving up.
	d := NewDispatcher(s, []Tech{testTech("tech-bob", RoleServiceTech, 41.88, -87.63)})
	d.Clearances = map[string]map[string]bool{"tech-bob": {"Office": true}}
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

func TestDispatcher_Assign_SetsAssignmentScore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := NewDispatcher(s, []Tech{testTech("tech-1", RoleServiceTech, 0, 0)})

	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}
	assigned, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	if assigned.AssignmentScore == nil {
		t.Fatal("AssignmentScore is nil, want a score recorded for the winning candidate")
	}
}

func TestDispatcher_TechPosition_MovesTowardAssignedFridge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const fridgeLat, fridgeLng = 32.7767, -96.7970
	if err := s.UpsertFridge(ctx, Fridge{
		ID: "f1", Status: StatusHealthy, LastEventAt: time.Now(),
		Location: &Location{City: "Dallas", State: "TX", Vertical: "Office", Lat: fridgeLat, Lng: fridgeLng},
	}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}

	tech := testTech("tech-1", RoleServiceTech, 41.8781, -87.6298) // Chicago -- far from the fridge
	d := NewDispatcher(s, []Tech{tech})
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return start }

	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: start})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}
	if _, err := d.Assign(ctx, alert.ID); err != nil {
		t.Fatalf("Assign() error: %v", err)
	}

	before := techViewByID(t, d, "tech-1")
	if before.Idle {
		t.Fatal("tech should be busy immediately after assignment")
	}
	if before.ETA == nil || !before.ETA.After(start) {
		t.Fatal("expected a future ETA")
	}
	if before.Lat != tech.HomeLat || before.Lng != tech.HomeLng {
		t.Fatalf("tech should still be at its origin immediately after assignment, got (%v,%v)", before.Lat, before.Lng)
	}

	// Partway to the ETA, the tech should have visibly moved closer to the
	// fridge -- not teleported and not still sitting at the origin.
	midpoint := start.Add(before.ETA.Sub(start) / 2)
	d.Now = func() time.Time { return midpoint }
	mid := techViewByID(t, d, "tech-1")
	if mid.Lat == tech.HomeLat && mid.Lng == tech.HomeLng {
		t.Fatal("tech position did not move from its origin at the ETA midpoint")
	}
	distAtStart := haversineMiles(tech.HomeLat, tech.HomeLng, fridgeLat, fridgeLng)
	distAtMid := haversineMiles(mid.Lat, mid.Lng, fridgeLat, fridgeLng)
	if distAtMid >= distAtStart {
		t.Fatalf("distance to fridge should have decreased by the ETA midpoint: start=%.1fmi mid=%.1fmi", distAtStart, distAtMid)
	}

	// At (or after) the ETA, the tech should have arrived.
	d.Now = func() time.Time { return *before.ETA }
	arrived := techViewByID(t, d, "tech-1")
	if arrived.Lat != fridgeLat || arrived.Lng != fridgeLng {
		t.Fatalf("expected tech to have arrived at the fridge's coordinates, got (%v,%v)", arrived.Lat, arrived.Lng)
	}
}

func TestDispatcher_Reassign_LogsOverrideWithPriorAutoAssignment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	techA := testTech("tech-a", RoleServiceTech, 0, 0)
	techB := testTech("tech-b", RoleServiceTech, 0, 0)
	d := NewDispatcher(s, []Tech{techA, techB})

	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}
	assigned, err := d.Assign(ctx, alert.ID)
	if err != nil {
		t.Fatalf("Assign() error: %v", err)
	}
	autoAssignee := assigned.AssignedTo
	other := "tech-b"
	if autoAssignee == "tech-b" {
		other = "tech-a"
	}

	reassigned, err := d.Reassign(ctx, alert.ID, other, "ops-lead")
	if err != nil {
		t.Fatalf("Reassign() error: %v", err)
	}
	if reassigned.AssignedTo != other {
		t.Fatalf("AssignedTo after Reassign = %q, want %q", reassigned.AssignedTo, other)
	}
	if reassigned.AssignmentScore == nil {
		t.Fatal("AssignmentScore is nil after Reassign, want a score recorded for the new tech too")
	}

	log := d.Reassignments(alert.ID)
	if len(log) != 1 {
		t.Fatalf("Reassignments() = %d entries, want 1", len(log))
	}
	entry := log[0]
	if entry.FromTech != autoAssignee {
		t.Fatalf("FromTech = %q, want the auto-assignment %q (preserved for comparison)", entry.FromTech, autoAssignee)
	}
	if entry.ToTech != other {
		t.Fatalf("ToTech = %q, want %q", entry.ToTech, other)
	}
	if entry.By != "ops-lead" {
		t.Fatalf("By = %q, want ops-lead", entry.By)
	}

	// Workload should have moved with the reassignment.
	if v := techViewByID(t, d, autoAssignee); v.Workload != 0 {
		t.Fatalf("original auto-assignee %s workload = %d, want 0 after being reassigned away", autoAssignee, v.Workload)
	}
	if v := techViewByID(t, d, other); v.Workload != 1 {
		t.Fatalf("new assignee %s workload = %d, want 1", other, v.Workload)
	}
}

func techViewByID(t *testing.T, d *Dispatcher, id string) TechView {
	t.Helper()
	for _, v := range d.Techs() {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no tech view found for ID %q", id)
	return TechView{}
}
