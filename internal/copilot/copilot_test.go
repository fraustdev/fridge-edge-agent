package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func sampleEvents() []EventSummaryInput {
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 1, 10, 10, 0, 0, time.UTC)
	return []EventSummaryInput{
		{FridgeID: "f1", SlotID: "A1", Type: "vend_completed", Timestamp: t1, Payload: map[string]any{"outcome": "success"}},
		{FridgeID: "f1", SlotID: "A2", Type: "hardware_fault", Timestamp: t2, Payload: map[string]any{"error": "jam"}},
		{FridgeID: "f2", SlotID: "B1", Type: "vend_completed", Timestamp: t3, Payload: map[string]any{"outcome": "refund_pending"}},
	}
}

func TestComputeSummary(t *testing.T) {
	events := sampleEvents()
	summary := computeSummary(events)

	if summary.TotalEvents != 3 {
		t.Fatalf("TotalEvents = %d, want 3", summary.TotalEvents)
	}
	if summary.FridgeCount != 2 {
		t.Fatalf("FridgeCount = %d, want 2", summary.FridgeCount)
	}
	if summary.EventCounts["hardware_fault"] != 1 {
		t.Fatalf("EventCounts[hardware_fault] = %d, want 1", summary.EventCounts["hardware_fault"])
	}
	if summary.EventCounts["vend_completed"] != 2 {
		t.Fatalf("EventCounts[vend_completed] = %d, want 2", summary.EventCounts["vend_completed"])
	}
	// known event types with zero occurrences must still appear.
	if _, ok := summary.EventCounts["restock_alert"]; !ok {
		t.Fatal("EventCounts missing restock_alert (should be present at 0)")
	}
	if summary.EventCounts["restock_alert"] != 0 {
		t.Fatalf("EventCounts[restock_alert] = %d, want 0", summary.EventCounts["restock_alert"])
	}
	if len(summary.ActionItems) != 1 {
		t.Fatalf("len(ActionItems) = %d, want 1", len(summary.ActionItems))
	}
	item := summary.ActionItems[0]
	if item.FridgeID != "f2" || item.Slot != "B1" || item.Reason == "" {
		t.Fatalf("ActionItems[0] = %+v, want fridge f2 slot B1 with a reason", item)
	}
}

func TestSummarize_NoAPIKey_UsesHeuristic(t *testing.T) {
	s := &Summarizer{APIKey: "", Now: fixedNow}
	summary := s.Summarize(context.Background(), sampleEvents())

	if summary.Source != "heuristic" {
		t.Fatalf("Source = %q, want heuristic", summary.Source)
	}
	if summary.Headline == "" {
		t.Fatal("heuristic Headline must not be empty")
	}
	if len(summary.ActionItems) != 1 || summary.ActionItems[0].FridgeID != "f2" {
		t.Fatalf("ActionItems = %+v, want one entry for fridge f2", summary.ActionItems)
	}
}

func TestSummarize_LLM(t *testing.T) {
	tests := []struct {
		name          string
		serverHandler http.HandlerFunc
		wantSource    string
		wantHeadline  string
	}{
		{
			name: "successful LLM call is used",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("x-api-key"); got != "test-key" {
					t.Errorf("x-api-key header = %q, want test-key", got)
				}
				if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
					t.Errorf("anthropic-version header = %q, want %q", got, anthropicVersion)
				}
				resp := messagesResponse{
					StopReason: "end_turn",
					Content:    []contentBlock{{Type: "text", Text: "Fleet is stable, one case needs manual review."}},
				}
				json.NewEncoder(w).Encode(resp)
			},
			wantSource:   "llm",
			wantHeadline: "Fleet is stable, one case needs manual review.",
		},
		{
			name: "refusal falls back to heuristic",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				resp := messagesResponse{StopReason: "refusal"}
				json.NewEncoder(w).Encode(resp)
			},
			wantSource: "heuristic",
		},
		{
			name: "non-200 falls back to heuristic",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(errorResponse{})
			},
			wantSource: "heuristic",
		},
		{
			name: "empty content falls back to heuristic",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				resp := messagesResponse{StopReason: "end_turn", Content: []contentBlock{}}
				json.NewEncoder(w).Encode(resp)
			},
			wantSource: "heuristic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.serverHandler)
			defer server.Close()

			s := &Summarizer{
				APIKey:     "test-key",
				Model:      "claude-haiku-4-5",
				HTTPClient: server.Client(),
				Now:        fixedNow,
			}
			headline, err := callLLMAt(s, server.URL, sampleEvents())
			if tt.wantSource == "heuristic" {
				if err == nil {
					t.Fatalf("expected callLLM to fail so Summarize would fall back, got headline %q", headline)
				}
				return
			}
			if err != nil {
				t.Fatalf("callLLM() unexpected error: %v", err)
			}
			if headline != tt.wantHeadline {
				t.Fatalf("headline = %q, want %q", headline, tt.wantHeadline)
			}
		})
	}
}

// callLLMAt exercises the same request/response handling as callLLM but
// against an arbitrary URL, temporarily swapping the package-level apiURL.
func callLLMAt(s *Summarizer, url string, events []EventSummaryInput) (string, error) {
	orig := apiURL
	apiURL = url
	defer func() { apiURL = orig }()
	return s.callLLM(context.Background(), computeSummary(events))
}
