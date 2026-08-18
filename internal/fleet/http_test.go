package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestIngestHandler_ServeHTTP(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1"})
	h := NewIngestHandler(s, d)

	tests := []struct {
		name           string
		body           string
		wantStatus     int
		wantAlertID    bool
		wantFridgeStat FridgeStatus
	}{
		{
			name:           "vend success records event, no alert",
			body:           `{"fridgeId":"f1","slotId":"A1","type":"vend_completed","payload":{"outcome":"success"}}`,
			wantStatus:     http.StatusCreated,
			wantAlertID:    false,
			wantFridgeStat: StatusHealthy,
		},
		{
			name:           "hardware fault records event and raises an alert",
			body:           `{"fridgeId":"f2","slotId":"A2","type":"hardware_fault","payload":{"error":"jam"}}`,
			wantStatus:     http.StatusCreated,
			wantAlertID:    true,
			wantFridgeStat: StatusFaulted,
		},
		{
			name:       "missing fridgeId is rejected",
			body:       `{"type":"hardware_fault"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing type is rejected",
			body:       `{"fridgeId":"f1"}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/fleet/events", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusCreated {
				return
			}

			var resp ingestResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if (resp.AlertID != nil) != tt.wantAlertID {
				t.Fatalf("AlertID present = %v, want %v", resp.AlertID != nil, tt.wantAlertID)
			}

			fridge, err := s.GetFridge(context.Background(), resp.Event.FridgeID)
			if err != nil {
				t.Fatalf("GetFridge() error: %v", err)
			}
			if fridge.Status != tt.wantFridgeStat {
				t.Fatalf("fridge status = %v, want %v", fridge.Status, tt.wantFridgeStat)
			}
		})
	}
}

func TestIngestHandler_WrongMethod(t *testing.T) {
	s := newTestStore(t)
	h := NewIngestHandler(s, NewDispatcher(s, []string{"tech-1"}))

	req := httptest.NewRequest(http.MethodGet, "/fleet/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestStatusHandler_ServeHTTP(t *testing.T) {
	s := newTestStore(t)
	fixedNow := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	s.UpsertFridge(context.Background(), Fridge{ID: "f1", Status: StatusHealthy, LastEventAt: fixedNow.Add(-time.Minute)})
	s.UpsertFridge(context.Background(), Fridge{ID: "f2", Status: StatusFaulted, LastEventAt: fixedNow.Add(-time.Hour)}) // stale -> offline

	h := &StatusHandler{Store: s, Now: func() time.Time { return fixedNow }}

	req := httptest.NewRequest(http.MethodGet, "/fleet/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp fleetStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Fridge) != 2 {
		t.Fatalf("len(fridges) = %d, want 2", len(resp.Fridge))
	}

	byID := map[string]fridgeView{}
	for _, f := range resp.Fridge {
		byID[f.ID] = f
	}
	if byID["f1"].Status != StatusHealthy {
		t.Fatalf("f1 status = %v, want healthy", byID["f1"].Status)
	}
	if byID["f2"].Status != StatusOffline {
		t.Fatalf("f2 status = %v, want offline (stale last_event_at overrides stored status)", byID["f2"].Status)
	}
}

func TestFridgeDetailHandler_ServeHTTP(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1"})
	ctx := context.Background()
	now := time.Now().UTC()

	s.UpsertFridge(ctx, Fridge{ID: "f1", Status: StatusFaulted, LastEventAt: now})
	s.InsertEvent(ctx, Event{FridgeID: "f1", SlotID: "A1", Type: EventHardwareFault, Timestamp: now})
	d.TriageEvent(ctx, Event{FridgeID: "f1", SlotID: "A1", Type: EventHardwareFault, Timestamp: now})

	h := &FridgeDetailHandler{Store: s, Now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodGet, "/fleet/fridges/f1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp fridgeDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Fridge.ID != "f1" {
		t.Fatalf("Fridge.ID = %q, want f1", resp.Fridge.ID)
	}
	if len(resp.RecentEvents) != 1 {
		t.Fatalf("len(RecentEvents) = %d, want 1", len(resp.RecentEvents))
	}
	if len(resp.OpenAlerts) != 1 {
		t.Fatalf("len(OpenAlerts) = %d, want 1", len(resp.OpenAlerts))
	}
}

func TestFridgeDetailHandler_NotFound(t *testing.T) {
	s := newTestStore(t)
	h := NewFridgeDetailHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/fleet/fridges/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAlertsHandler_ListAssignResolve(t *testing.T) {
	s := newTestStore(t)
	d := NewDispatcher(s, []string{"tech-1"})
	h := NewAlertsHandler(s, d)
	ctx := context.Background()

	alert, err := d.TriageEvent(ctx, Event{FridgeID: "f1", Type: EventHardwareFault, Timestamp: time.Now()})
	if err != nil || alert == nil {
		t.Fatalf("TriageEvent() error: %v, alert: %v", err, alert)
	}

	// List
	req := httptest.NewRequest(http.MethodGet, "/fleet/alerts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var listed []Alert
	json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}

	// Assign
	assignPath := "/fleet/alerts/" + strconv.FormatInt(alert.ID, 10) + "/assign"
	req = httptest.NewRequest(http.MethodPost, assignPath, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var assigned Alert
	json.Unmarshal(rec.Body.Bytes(), &assigned)
	if assigned.Status != AlertAssigned {
		t.Fatalf("Status = %v, want assigned", assigned.Status)
	}

	// Resolve
	resolvePath := "/fleet/alerts/" + strconv.FormatInt(alert.ID, 10) + "/resolve"
	req = httptest.NewRequest(http.MethodPost, resolvePath, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resolved Alert
	json.Unmarshal(rec.Body.Bytes(), &resolved)
	if resolved.Status != AlertResolved {
		t.Fatalf("Status = %v, want resolved", resolved.Status)
	}
}
