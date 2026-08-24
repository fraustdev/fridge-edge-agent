// Package copilot generates fleet-wide ops summaries from a batch of events.
// It sits outside the vend/dispatch decision path entirely — it is a
// post-hoc reporting layer, never a controller. When no API key is set (or
// the API call fails), it falls back to a generic heuristic headline
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

// knownEventTypes always appear in Summary.EventCounts, even at zero, so
// the dashboard's bar chart has a stable, complete set of bars rather than
// only whatever event types happened to occur in this particular batch.
var knownEventTypes = []string{"vend_completed", "hardware_fault", "door_anomaly", "restock_alert"}

// EventSummaryInput is copilot's own view of a fleet event, decoupled from
// the fleet package's types so this package has no dependency on it.
type EventSummaryInput struct {
	FridgeID  string
	SlotID    string
	Type      string
	Timestamp time.Time
	Payload   map[string]any
}

// ActionItem is one charged-but-not-dispensed case that needs a human to
// reconcile it.
type ActionItem struct {
	FridgeID  string
	Slot      string
	Timestamp time.Time
	Reason    string
}

// Summary is a fleet-wide ops summary. EventCounts/TotalEvents/FridgeCount/
// ActionItems are computed deterministically in Go, never parsed from the
// LLM response, so the one correctness property that matters (never hide a
// possible charged-but-no-item case) holds even if the LLM call fails, is
// skipped, or writes something misleading. The LLM's only job is Headline
// — a short interpretive sentence — which is display framing, not a
// correctness surface.
type Summary struct {
	Headline    string
	EventCounts map[string]int
	TotalEvents int
	FridgeCount int
	ActionItems []ActionItem
	Source      string // "llm" or "heuristic"
}

// Summarizer produces Summaries from event batches.
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

// Summarize builds a Summary for events. EventCounts/TotalEvents/
// FridgeCount/ActionItems are always computed the same way, LLM or not.
// Summarize always attempts the LLM call first when an API key is present
// (to write Headline); any failure (network, non-2xx, refusal, empty
// output) falls back to a generic heuristic headline rather than erroring.
func (s *Summarizer) Summarize(ctx context.Context, events []EventSummaryInput) Summary {
	summary := computeSummary(events)

	if s.APIKey == "" {
		summary.Headline = "Fleet summary"
		summary.Source = "heuristic"
		return summary
	}

	headline, err := s.callLLM(ctx, summary)
	if err != nil {
		summary.Headline = "Fleet summary"
		summary.Source = "heuristic"
		return summary
	}

	summary.Headline = headline
	summary.Source = "llm"
	return summary
}

func computeSummary(events []EventSummaryInput) Summary {
	summary := Summary{EventCounts: map[string]int{}, TotalEvents: len(events)}
	for _, t := range knownEventTypes {
		summary.EventCounts[t] = 0
	}

	fridges := make(map[string]struct{})
	for _, e := range events {
		fridges[e.FridgeID] = struct{}{}
		summary.EventCounts[e.Type]++

		if e.Type == "vend_completed" {
			if outcome, _ := e.Payload["outcome"].(string); outcome == "refund_pending" {
				summary.ActionItems = append(summary.ActionItems, ActionItem{
					FridgeID:  e.FridgeID,
					Slot:      e.SlotID,
					Timestamp: e.Timestamp,
					Reason:    "charged, not dispensed",
				})
			}
		}
	}
	summary.FridgeCount = len(fridges)

	return summary
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

// callLLM asks Claude for a single headline sentence, given the already-
// computed summary stats -- not the raw event list. The LLM never sees
// (and can't influence) anything but the wording of that one sentence.
func (s *Summarizer) callLLM(ctx context.Context, summary Summary) (string, error) {
	reqBody, err := json.Marshal(messagesRequest{
		Model:     s.Model,
		MaxTokens: 100,
		Messages: []requestMessage{{
			Role:    "user",
			Content: buildHeadlinePrompt(summary),
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

func buildHeadlinePrompt(s Summary) string {
	var sb strings.Builder
	sb.WriteString("You are an ops copilot for a fleet of smart vending fridges. ")
	sb.WriteString("Write exactly one short, plain-English sentence (no more than ~20 words) summarizing the current state of the fleet, suitable as a dashboard headline. ")
	sb.WriteString("Interpret the numbers rather than reciting them verbatim -- e.g. \"Fault rate is higher than usual this hour\" or \"Fleet is stable, one case needs manual review.\" ")
	sb.WriteString("If there are any charged-but-not-dispensed cases, your sentence must acknowledge that manual review is needed, but do not enumerate them individually -- they're shown separately as a structured list.\n\n")

	fmt.Fprintf(&sb, "Total events: %d across %d fridge(s).\n", s.TotalEvents, s.FridgeCount)

	sb.WriteString("Event counts by type:\n")
	types := make([]string, 0, len(s.EventCounts))
	for t := range s.EventCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Fprintf(&sb, "- %s: %d\n", t, s.EventCounts[t])
	}

	fmt.Fprintf(&sb, "\nCharged-but-not-dispensed cases needing manual review: %d\n", len(s.ActionItems))

	return sb.String()
}
