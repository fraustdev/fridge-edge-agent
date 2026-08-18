// Package copilot generates fleet-wide ops summaries from a batch of events.
// It sits outside the vend/dispatch decision path entirely — it is a
// post-hoc reporting layer, never a controller. When no API key is set (or
// the API call fails), it falls back to a deterministic heuristic summary
// instead of failing the request.
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultModel     = "claude-haiku-4-5"
	anthropicVersion = "2023-06-01"
)

// apiURL is a var (not const) so tests can point it at an httptest.Server.
var apiURL = "https://api.anthropic.com/v1/messages"

// EventSummaryInput is copilot's own view of a fleet event, decoupled from
// the fleet package's types so this package has no dependency on it.
type EventSummaryInput struct {
	FridgeID  string
	SlotID    string
	Type      string
	Timestamp time.Time
	Payload   map[string]any
}

// Report is a fleet-wide ops summary. ChargedNoItemCount/Details are
// computed deterministically in Go, not parsed from the LLM response, so
// the one correctness property that matters (never hide a possible
// charged-but-no-item case) holds even if the LLM call fails or is skipped.
type Report struct {
	GeneratedAt          time.Time
	TotalEvents          int
	FridgesAffected      int
	HardwareFaults       int
	RestockAlerts        int
	DoorAnomalies        int
	ChargedNoItemCount   int
	ChargedNoItemDetails []string
	Narrative            string
	Source               string // "llm" or "heuristic"
}

// Summarizer produces Reports from event batches.
type Summarizer struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Now        func() time.Time
}

// NewSummarizer builds a Summarizer reading ANTHROPIC_API_KEY from the
// environment. An empty key means Summarize always uses the heuristic path.
func NewSummarizer() *Summarizer {
	return &Summarizer{
		APIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		Model:      defaultModel,
		HTTPClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (s *Summarizer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Summarize builds a Report for events. It always attempts the LLM call
// first when an API key is present; any failure (network, non-2xx, refusal,
// empty output) falls back to the heuristic narrative rather than erroring.
func (s *Summarizer) Summarize(ctx context.Context, events []EventSummaryInput) Report {
	report := computeStats(events, s.now())

	if s.APIKey == "" {
		report.Narrative = heuristicNarrative(report)
		report.Source = "heuristic"
		return report
	}

	narrative, err := s.callLLM(ctx, events, report)
	if err != nil {
		report.Narrative = heuristicNarrative(report)
		report.Source = "heuristic"
		return report
	}

	report.Narrative = narrative
	report.Source = "llm"
	return report
}

func computeStats(events []EventSummaryInput, now time.Time) Report {
	report := Report{GeneratedAt: now, TotalEvents: len(events)}

	fridges := make(map[string]struct{})
	for _, e := range events {
		fridges[e.FridgeID] = struct{}{}

		switch e.Type {
		case "hardware_fault":
			report.HardwareFaults++
		case "restock_alert":
			report.RestockAlerts++
		case "door_anomaly":
			report.DoorAnomalies++
		case "vend_completed":
			if outcome, _ := e.Payload["outcome"].(string); outcome == "refund_pending" {
				report.ChargedNoItemCount++
				report.ChargedNoItemDetails = append(report.ChargedNoItemDetails, fmt.Sprintf(
					"fridge %s slot %s at %s", e.FridgeID, e.SlotID, e.Timestamp.Format(time.RFC3339)))
			}
		}
	}
	report.FridgesAffected = len(fridges)

	return report
}

func heuristicNarrative(r Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Fleet summary: %d events across %d fridge(s). ", r.TotalEvents, r.FridgesAffected)
	fmt.Fprintf(&sb, "%d hardware fault(s), %d restock alert(s), %d door anomaly(ies). ",
		r.HardwareFaults, r.RestockAlerts, r.DoorAnomalies)

	if r.ChargedNoItemCount == 0 {
		sb.WriteString("No transactions were left in a charged-but-no-item state.")
		return sb.String()
	}

	fmt.Fprintf(&sb, "%d transaction(s) may have charged a customer without dispensing an item: ",
		r.ChargedNoItemCount)
	sb.WriteString(strings.Join(r.ChargedNoItemDetails, "; "))
	sb.WriteString(". These need manual reconciliation.")
	return sb.String()
}

type messagesRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []requestMessage `json:"messages"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	StopReason string         `json:"stop_reason"`
	Content    []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type errorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *Summarizer) callLLM(ctx context.Context, events []EventSummaryInput, stats Report) (string, error) {
	reqBody, err := json.Marshal(messagesRequest{
		Model:     s.Model,
		MaxTokens: 512,
		Messages: []requestMessage{{
			Role:    "user",
			Content: buildPrompt(events, stats),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", s.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr errorResponse
		_ = json.Unmarshal(data, &apiErr)
		return "", fmt.Errorf("anthropic api error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
	}

	var parsed messagesResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if parsed.StopReason == "refusal" {
		return "", fmt.Errorf("anthropic declined the request")
	}

	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("empty response text")
	}
	return text, nil
}

func buildPrompt(events []EventSummaryInput, stats Report) string {
	var sb strings.Builder
	sb.WriteString("You are an ops copilot summarizing recent activity across a fleet of smart vending fridges. ")
	sb.WriteString("Write a short, plain-language summary (3-5 sentences) an operations team can scan quickly. ")
	sb.WriteString("You MUST explicitly call out every transaction where a customer may have been charged without receiving an item, naming the fridge, slot, and time for each. ")
	sb.WriteString("Do not omit or downplay these even if there are many other events.\n\n")

	fmt.Fprintf(&sb, "Stats: %d total events, %d fridges affected, %d hardware faults, %d restock alerts, %d door anomalies, %d charged-no-item case(s).\n",
		stats.TotalEvents, stats.FridgesAffected, stats.HardwareFaults, stats.RestockAlerts, stats.DoorAnomalies, stats.ChargedNoItemCount)

	if stats.ChargedNoItemCount > 0 {
		sb.WriteString("Charged-no-item cases: ")
		sb.WriteString(strings.Join(stats.ChargedNoItemDetails, "; "))
		sb.WriteString("\n")
	}

	sb.WriteString("\nEvents:\n")
	sorted := make([]EventSummaryInput, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Timestamp.Before(sorted[j].Timestamp) })
	for _, e := range sorted {
		fmt.Fprintf(&sb, "- [%s] fridge=%s slot=%s type=%s payload=%v\n",
			e.Timestamp.Format(time.RFC3339), e.FridgeID, e.SlotID, e.Type, e.Payload)
	}

	return sb.String()
}
