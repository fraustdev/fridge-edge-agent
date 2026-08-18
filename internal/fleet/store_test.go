package fleet

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_InsertAndListEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	inserted, err := s.InsertEvent(ctx, Event{
		FridgeID:  "f1",
		SlotID:    "A1",
		Type:      EventVendCompleted,
		Timestamp: ts,
		Payload:   map[string]any{"outcome": "success"},
	})
	if err != nil {
		t.Fatalf("InsertEvent() error: %v", err)
	}
	if inserted.ID == 0 {
		t.Fatal("InsertEvent() did not assign an ID")
	}

	events, err := s.ListEventsForFridge(ctx, "f1", 10)
	if err != nil {
		t.Fatalf("ListEventsForFridge() error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	got := events[0]
	if got.FridgeID != "f1" || got.SlotID != "A1" || got.Type != EventVendCompleted {
		t.Fatalf("event mismatch: %+v", got)
	}
	if !got.Timestamp.Equal(ts) {
		t.Fatalf("Timestamp = %v, want %v", got.Timestamp, ts)
	}
	if got.Payload["outcome"] != "success" {
		t.Fatalf("Payload[outcome] = %v, want success", got.Payload["outcome"])
	}
}

func TestStore_ListEventsForFridge_ScopedToFridge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	s.InsertEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: now})
	s.InsertEvent(ctx, Event{FridgeID: "f2", Type: EventHardwareFault, Timestamp: now})

	events, err := s.ListEventsForFridge(ctx, "f1", 10)
	if err != nil {
		t.Fatalf("ListEventsForFridge() error: %v", err)
	}
	if len(events) != 1 || events[0].FridgeID != "f1" {
		t.Fatalf("expected only f1's event, got %+v", events)
	}
}

func TestStore_FridgeUpsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ts1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(time.Hour)

	if err := s.UpsertFridge(ctx, Fridge{ID: "f1", Status: StatusHealthy, LastEventAt: ts1}); err != nil {
		t.Fatalf("UpsertFridge() error: %v", err)
	}
	if err := s.UpsertFridge(ctx, Fridge{ID: "f1", Status: StatusFaulted, LastEventAt: ts2}); err != nil {
		t.Fatalf("UpsertFridge() (update) error: %v", err)
	}

	got, err := s.GetFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("GetFridge() error: %v", err)
	}
	if got.Status != StatusFaulted {
		t.Fatalf("Status = %v, want %v (upsert should update, not duplicate)", got.Status, StatusFaulted)
	}
	if !got.LastEventAt.Equal(ts2) {
		t.Fatalf("LastEventAt = %v, want %v", got.LastEventAt, ts2)
	}
}

func TestStore_GetFridge_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetFridge(context.Background(), "does-not-exist")
	if err != ErrNotFound {
		t.Fatalf("GetFridge() error = %v, want ErrNotFound", err)
	}
}

func TestStore_AlertLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	alert, err := s.CreateAlert(ctx, Alert{
		FridgeID:    "f1",
		SlotID:      "A1",
		SourceEvent: EventHardwareFault,
		Severity:    SeverityHigh,
		Status:      AlertOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("CreateAlert() error: %v", err)
	}
	if alert.ID == 0 {
		t.Fatal("CreateAlert() did not assign an ID")
	}

	open, err := s.ListOpenAlertsForFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("ListOpenAlertsForFridge() error: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("len(open alerts) = %d, want 1", len(open))
	}

	assigned, err := s.UpdateAlertStatus(ctx, alert.ID, AlertAssigned, "tech-1")
	if err != nil {
		t.Fatalf("UpdateAlertStatus(assigned) error: %v", err)
	}
	if assigned.Status != AlertAssigned || assigned.AssignedTo != "tech-1" {
		t.Fatalf("assigned alert = %+v, want status=assigned assignedTo=tech-1", assigned)
	}

	resolved, err := s.UpdateAlertStatus(ctx, alert.ID, AlertResolved, "tech-1")
	if err != nil {
		t.Fatalf("UpdateAlertStatus(resolved) error: %v", err)
	}
	if resolved.Status != AlertResolved {
		t.Fatalf("Status = %v, want resolved", resolved.Status)
	}

	open, err = s.ListOpenAlertsForFridge(ctx, "f1")
	if err != nil {
		t.Fatalf("ListOpenAlertsForFridge() error: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("len(open alerts) after resolve = %d, want 0", len(open))
	}
}

func TestStore_UpdateAlertStatus_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpdateAlertStatus(context.Background(), 999, AlertAssigned, "tech-1")
	if err != ErrNotFound {
		t.Fatalf("UpdateAlertStatus() error = %v, want ErrNotFound", err)
	}
}

func TestStore_ListAlerts_FilterByStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a1, _ := s.CreateAlert(ctx, Alert{FridgeID: "f1", SourceEvent: EventHardwareFault, Severity: SeverityHigh, Status: AlertOpen, CreatedAt: now, UpdatedAt: now})
	s.CreateAlert(ctx, Alert{FridgeID: "f2", SourceEvent: EventRestockAlert, Severity: SeverityLow, Status: AlertOpen, CreatedAt: now, UpdatedAt: now})
	s.UpdateAlertStatus(ctx, a1.ID, AlertResolved, "")

	open, err := s.ListAlerts(ctx, AlertOpen)
	if err != nil {
		t.Fatalf("ListAlerts(open) error: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("len(open) = %d, want 1", len(open))
	}

	all, err := s.ListAlerts(ctx, "")
	if err != nil {
		t.Fatalf("ListAlerts(all) error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}
}
