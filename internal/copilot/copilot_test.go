package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestComputeStats(t *testing.T) {
	events := sampleEvents()
	report := computeStats(events, fixedNow())

	if report.TotalEvents != 3 {
		t.Fatalf("TotalEvents = %d, want 3", report.TotalEvents)
	}
	if report.FridgesAffected != 2 {
		t.Fatalf("FridgesAffected = %d, want 2", report.FridgesAffected)
	}
	if report.HardwareFaults != 1 {
		t.Fatalf("HardwareFaults = %d, want 1", report.HardwareFaults)
	}
	if report.ChargedNoItemCount != 1 {
		t.Fatalf("ChargedNoItemCount = %d, want 1", report.ChargedNoItemCount)
	}
	if len(report.ChargedNoItemDetails) != 1 || !strings.Contains(report.ChargedNoItemDetails[0], "f2") {
		t.Fatalf("ChargedNoItemDetails = %v, want a detail mentioning f2", report.ChargedNoItemDetails)
	}
}

func TestSummarize_NoAPIKey_UsesHeuristic(t *testing.T) {
	s := &Summarizer{APIKey: "", Now: fixedNow}
	report := s.Summarize(context.Background(), sampleEvents())

	if report.Source != "heuristic" {
		t.Fatalf("Source = %q, want heuristic", report.Source)
	}
	if !strings.Contains(report.Narrative, "f2") {
		t.Fatalf("heuristic narrative must mention the charged-no-item fridge, got: %s", report.Narrative)
	}
	if report.ChargedNoItemCount != 1 {
		t.Fatalf("ChargedNoItemCount = %d, want 1", report.ChargedNoItemCount)
	}
}

func TestSummarize_LLM(t *testing.T) {
	tests := []struct {
		name          string
		serverHandler http.HandlerFunc
		wantSource    string
		wantNarrative string
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
					Content:    []contentBlock{{Type: "text", Text: "LLM narrative mentioning fridge f2."}},
				}
				json.NewEncoder(w).Encode(resp)
			},
			wantSource:    "llm",
			wantNarrative: "LLM narrative mentioning fridge f2.",
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
			narrative, err := callLLMAt(s, server.URL, sampleEvents())
			if tt.wantSource == "heuristic" {
				if err == nil {
					t.Fatalf("expected callLLM to fail so Summarize would fall back, got narrative %q", narrative)
				}
				return
			}
			if err != nil {
				t.Fatalf("callLLM() unexpected error: %v", err)
			}
			if narrative != tt.wantNarrative {
				t.Fatalf("narrative = %q, want %q", narrative, tt.wantNarrative)
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
	return s.callLLM(context.Background(), events, computeStats(events, s.now()))
}
