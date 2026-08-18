package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// IngestHandler exposes the HTTP endpoint fridges POST events to.
type IngestHandler struct {
	Store      Store
	Dispatcher *Dispatcher
}

func NewIngestHandler(store Store, dispatcher *Dispatcher) *IngestHandler {
	return &IngestHandler{Store: store, Dispatcher: dispatcher}
}

type ingestRequest struct {
	FridgeID  string         `json:"fridgeId"`
	SlotID    string         `json:"slotId"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload"`
}

type ingestResponse struct {
	Event   Event  `json:"event"`
	AlertID *int64 `json:"alertId,omitempty"`
}

// ServeHTTP handles POST /events: validate, persist, update fridge state,
// and triage into an alert if warranted.
func (h *IngestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.FridgeID == "" {
		http.Error(w, "fridgeId is required", http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		http.Error(w, "type is required", http.StatusBadRequest)
		return
	}
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now().UTC()
	}

	ctx := r.Context()
	event, err := h.Store.InsertEvent(ctx, Event{
		FridgeID:  req.FridgeID,
		SlotID:    req.SlotID,
		Type:      EventType(req.Type),
		Timestamp: req.Timestamp,
		Payload:   req.Payload,
	})
	if err != nil {
		http.Error(w, "failed to record event: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.updateFridgeState(ctx, event); err != nil {
		http.Error(w, "failed to update fridge state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ingestResponse{Event: event}
	if h.Dispatcher != nil {
		alert, err := h.Dispatcher.TriageEvent(ctx, event)
		if err != nil {
			http.Error(w, "failed to triage event: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if alert != nil {
			resp.AlertID = &alert.ID
		}
	}

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// updateFridgeState upserts the fridge's current record, deriving its
// health status from the event. Status derivation is deliberately simple:
// a hardware fault or a charged-but-no-item vend always marks the fridge
// faulted; a fault or low-stock state is not silently cleared by later
// events (that requires an explicit restock/repair, which this demo does
// not model) except a subsequent clean vend success.
func (h *IngestHandler) updateFridgeState(ctx context.Context, e Event) error {
	current, err := h.Store.GetFridge(ctx, e.FridgeID)
	if err != nil && err != ErrNotFound {
		return err
	}
	status := computeStatus(current.Status, e)

	return h.Store.UpsertFridge(ctx, Fridge{
		ID:          e.FridgeID,
		Status:      status,
		LastEventAt: e.Timestamp,
	})
}

func computeStatus(current FridgeStatus, e Event) FridgeStatus {
	switch e.Type {
	case EventHardwareFault:
		return StatusFaulted
	case EventRestockAlert:
		if current == StatusFaulted {
			return current
		}
		return StatusLowStock
	case EventVendCompleted:
		outcome, _ := e.Payload["outcome"].(string)
		switch outcome {
		case "success":
			if current == "" {
				return StatusHealthy
			}
			return current
		case "refund_pending", "refunded":
			return StatusFaulted
		default:
			return current
		}
	default:
		if current == "" {
			return StatusHealthy
		}
		return current
	}
}
