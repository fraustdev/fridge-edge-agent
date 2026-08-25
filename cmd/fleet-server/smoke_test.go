package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frida/fridge-edge-agent/internal/copilot"
	"github.com/frida/fridge-edge-agent/internal/fleet"
)

// TestSmoke_EventsThroughStatusAndCopilot is a small end-to-end check, not
// a full e2e suite: start a real fleet-server (in-memory SQLite, real HTTP
// round-trips via httptest.Server), POST one event of each type, and
// assert /fleet/status and /fleet/copilot/summary both come back sane and
// correctly reflect what was just POSTed. Unit tests already cover each
// package's internals in isolation; this exists to catch the thing they
// can't -- a wiring mistake in how ingest/status/copilot actually compose
// through newMux, the same composition the real binary uses.
func TestSmoke_EventsThroughStatusAndCopilot(t *testing.T) {
	store, err := fleet.OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	dispatcher := fleet.NewDispatcher(store, fleet.DefaultTechRoster())
	// Empty APIKey (not copilot.NewSummarizer(), which would read
	// ANTHROPIC_API_KEY from the environment) forces the deterministic
	// heuristic path -- this test must not depend on network access or
	// real credentials being present in whatever environment runs it.
	summarizer := &copilot.Summarizer{}

	server := httptest.NewServer(newMux(store, dispatcher, summarizer))
	defer server.Close()

	post := func(t *testing.T, body string) {
		t.Helper()
		resp, err := http.Post(server.URL+"/fleet/events", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST /fleet/events: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST /fleet/events status = %d, want 201 (body: %s)", resp.StatusCode, body)
		}
	}

	// One event of each type the fridge side can report, each on its own
	// fridge so /fleet/status's per-fridge counts are unambiguous.
	post(t, `{"fridgeId":"f-success","slotId":"A1","type":"vend_completed","payload":{"outcome":"success"}}`)
	post(t, `{"fridgeId":"f-refund-pending","slotId":"A1","type":"vend_completed","payload":{"outcome":"refund_pending"}}`)
	post(t, `{"fridgeId":"f-fault","slotId":"A1","type":"hardware_fault","payload":{"error":"jam"}}`)
	post(t, `{"fridgeId":"f-restock","slotId":"A1","type":"restock_alert","payload":{"reason":"slot_empty"}}`)
	post(t, `{"fridgeId":"f-door","slotId":"A1","type":"door_anomaly","payload":{"reason":"left_open"}}`)

	// --- /fleet/status ---
	statusResp, err := http.Get(server.URL + "/fleet/status")
	if err != nil {
		t.Fatalf("GET /fleet/status: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fleet/status status = %d, want 200", statusResp.StatusCode)
	}

	var status struct {
		Counts  map[string]int `json:"counts"`
		Fridges []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"fridges"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode /fleet/status: %v", err)
	}

	if len(status.Fridges) != 5 {
		t.Fatalf("len(fridges) = %d, want 5", len(status.Fridges))
	}
	byID := map[string]string{}
	for _, f := range status.Fridges {
		byID[f.ID] = f.Status
	}
	wantStatus := map[string]string{
		"f-success":        "healthy",
		"f-refund-pending": "faulted",
		"f-fault":          "faulted",
		"f-restock":        "low_stock",
		"f-door":           "healthy", // door_anomaly alone doesn't change status
	}
	for id, want := range wantStatus {
		if got := byID[id]; got != want {
			t.Errorf("fridge %s status = %q, want %q", id, got, want)
		}
	}
	if status.Counts["faulted"] != 2 {
		t.Errorf("counts[faulted] = %d, want 2", status.Counts["faulted"])
	}
	if status.Counts["healthy"] != 2 {
		t.Errorf("counts[healthy] = %d, want 2", status.Counts["healthy"])
	}
	if status.Counts["low_stock"] != 1 {
		t.Errorf("counts[low_stock] = %d, want 1", status.Counts["low_stock"])
	}

	// --- /fleet/copilot/summary ---
	summaryResp, err := http.Get(server.URL + "/fleet/copilot/summary")
	if err != nil {
		t.Fatalf("GET /fleet/copilot/summary: %v", err)
	}
	defer summaryResp.Body.Close()
	if summaryResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fleet/copilot/summary status = %d, want 200", summaryResp.StatusCode)
	}

	// copilot.Summary has no json tags, so the wire format uses Go's
	// exported field names as-is (Headline, EventCounts, ...) -- matched
	// here, not "fixed", since correcting that is out of scope for this
	// pass.
	var summary struct {
		Headline    string
		EventCounts map[string]int
		TotalEvents int
		FridgeCount int
		ActionItems []struct {
			FridgeID string
			Reason   string
		}
		Source string
	}
	if err := json.NewDecoder(summaryResp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode /fleet/copilot/summary: %v", err)
	}

	if summary.Headline == "" {
		t.Error("Headline is empty, want at least the heuristic fallback headline")
	}
	if summary.Source != "heuristic" {
		t.Errorf("Source = %q, want heuristic (no API key configured for this test)", summary.Source)
	}
	if summary.TotalEvents != 5 {
		t.Errorf("TotalEvents = %d, want 5", summary.TotalEvents)
	}
	if summary.FridgeCount != 5 {
		t.Errorf("FridgeCount = %d, want 5", summary.FridgeCount)
	}
	if summary.EventCounts["vend_completed"] != 2 {
		t.Errorf("EventCounts[vend_completed] = %d, want 2", summary.EventCounts["vend_completed"])
	}
	if summary.EventCounts["hardware_fault"] != 1 {
		t.Errorf("EventCounts[hardware_fault] = %d, want 1", summary.EventCounts["hardware_fault"])
	}
	if summary.EventCounts["restock_alert"] != 1 {
		t.Errorf("EventCounts[restock_alert] = %d, want 1", summary.EventCounts["restock_alert"])
	}
	if summary.EventCounts["door_anomaly"] != 1 {
		t.Errorf("EventCounts[door_anomaly] = %d, want 1", summary.EventCounts["door_anomaly"])
	}
	if len(summary.ActionItems) != 1 {
		t.Fatalf("len(ActionItems) = %d, want 1 (the refund_pending case)", len(summary.ActionItems))
	}
	if summary.ActionItems[0].FridgeID != "f-refund-pending" {
		t.Errorf("ActionItems[0].FridgeID = %q, want f-refund-pending", summary.ActionItems[0].FridgeID)
	}
}
