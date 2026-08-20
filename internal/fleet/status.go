package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OfflineThreshold is how long a fridge can go without reporting an event
// before the fleet status view considers it offline, regardless of its
// last known health status.
const OfflineThreshold = 15 * time.Minute

// StatusHandler exposes the fleet-wide and per-fridge read-side HTTP
// endpoints described in SPEC.md — the direct replacement for "no single
// place to see fleet state."
type StatusHandler struct {
	Store Store
	Now   func() time.Time
}

func NewStatusHandler(store Store) *StatusHandler {
	return &StatusHandler{Store: store}
}

func (h *StatusHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// effectiveStatus overrides the stored status with "offline" when the
// fridge hasn't reported anything recently.
func (h *StatusHandler) effectiveStatus(f Fridge) FridgeStatus {
	if h.now().Sub(f.LastEventAt) > OfflineThreshold {
		return StatusOffline
	}
	return f.Status
}

type fleetStatusResponse struct {
	GeneratedAt time.Time      `json:"generatedAt"`
	Counts      map[string]int `json:"counts"`
	Fridge      []fridgeView   `json:"fridges"`
}

type fridgeView struct {
	ID           string          `json:"id"`
	Status       FridgeStatus    `json:"status"`
	LastEventAt  time.Time       `json:"lastEventAt"`
	OpenAlertIDs []int64         `json:"openAlertIds,omitempty"`
	Location     *Location       `json:"location,omitempty"`
	Tier         CriticalityTier `json:"tier,omitempty"` // venue criticality tier; see venue.go
}

func tierFor(loc *Location) CriticalityTier {
	if loc == nil {
		return ""
	}
	return venueProfileFor(loc.Vertical).Tier
}

// ServeHTTP handles GET /fleet/status: fleet-wide health view.
func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fridges, err := h.Store.ListFridges(r.Context())
	if err != nil {
		http.Error(w, "failed to list fridges: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := fleetStatusResponse{
		GeneratedAt: h.now(),
		Counts:      map[string]int{},
	}
	for _, f := range fridges {
		status := h.effectiveStatus(f)
		resp.Counts[string(status)]++
		resp.Fridge = append(resp.Fridge, fridgeView{
			ID:           f.ID,
			Status:       status,
			LastEventAt:  f.LastEventAt,
			OpenAlertIDs: f.OpenAlertIDs,
			Location:     f.Location,
			Tier:         tierFor(f.Location),
		})
	}

	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type fridgeDetailResponse struct {
	Fridge       fridgeView `json:"fridge"`
	RecentEvents []Event    `json:"recentEvents"`
	OpenAlerts   []Alert    `json:"openAlerts"`
}

// FridgeDetailHandler handles GET /fleet/fridges/{id}: per-fridge drill-down.
type FridgeDetailHandler struct {
	Store Store
	Now   func() time.Time
}

func NewFridgeDetailHandler(store Store) *FridgeDetailHandler {
	return &FridgeDetailHandler{Store: store}
}

func (h *FridgeDetailHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *FridgeDetailHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/fleet/fridges/")
	if id == "" {
		http.Error(w, "fridge id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	fridge, err := h.Store.GetFridge(ctx, id)
	if err != nil {
		if err == ErrNotFound {
			http.Error(w, "fridge not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to get fridge: "+err.Error(), http.StatusInternalServerError)
		return
	}

	events, err := h.Store.ListEventsForFridge(ctx, id, 50)
	if err != nil {
		http.Error(w, "failed to list events: "+err.Error(), http.StatusInternalServerError)
		return
	}

	openAlerts, err := h.Store.ListOpenAlertsForFridge(ctx, id)
	if err != nil {
		http.Error(w, "failed to list alerts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	status := fridge.Status
	if h.now().Sub(fridge.LastEventAt) > OfflineThreshold {
		status = StatusOffline
	}

	resp := fridgeDetailResponse{
		Fridge: fridgeView{
			ID:           fridge.ID,
			Status:       status,
			LastEventAt:  fridge.LastEventAt,
			OpenAlertIDs: fridge.OpenAlertIDs,
			Location:     fridge.Location,
			Tier:         tierFor(fridge.Location),
		},
		RecentEvents: events,
		OpenAlerts:   openAlerts,
	}

	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// AlertsHandler handles the dispatch lifecycle over HTTP:
//
//	GET  /fleet/alerts             list alerts (optional ?status= filter)
//	POST /fleet/alerts/{id}/assign assign the alert to the next eligible tech
//	POST /fleet/alerts/{id}/resolve mark the alert resolved
//	POST /fleet/alerts/assign-next assign the single highest-priority open alert
type AlertsHandler struct {
	Store      Store
	Dispatcher *Dispatcher
}

func NewAlertsHandler(store Store, dispatcher *Dispatcher) *AlertsHandler {
	return &AlertsHandler{Store: store, Dispatcher: dispatcher}
}

func (h *AlertsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/fleet/alerts")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		h.list(w, r)
	case path == "assign-next" && r.Method == http.MethodPost:
		h.assignNext(w, r)
	case strings.HasSuffix(path, "/assign") && r.Method == http.MethodPost:
		h.transition(w, r, strings.TrimSuffix(path, "/assign"), h.Dispatcher.Assign)
	case strings.HasSuffix(path, "/resolve") && r.Method == http.MethodPost:
		h.transition(w, r, strings.TrimSuffix(path, "/resolve"), h.Dispatcher.Resolve)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// alertView adds the current, read-time-computed priority score to an
// Alert for display -- priority isn't stored, since it depends on "now"
// (age, peak windows) as well as the alert's static fields.
type alertView struct {
	Alert
	Priority float64 `json:"priority"`
}

func (h *AlertsHandler) list(w http.ResponseWriter, r *http.Request) {
	status := AlertStatus(r.URL.Query().Get("status"))
	alerts, err := h.Store.ListAlerts(r.Context(), status)
	if err != nil {
		http.Error(w, "failed to list alerts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]alertView, len(alerts))
	for i, a := range alerts {
		views[i] = alertView{Alert: a, Priority: h.Dispatcher.PriorityOf(r.Context(), a)}
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(views)
}

func (h *AlertsHandler) assignNext(w http.ResponseWriter, r *http.Request) {
	assigned, err := h.Dispatcher.AssignNext(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"assigned": assigned})
}

func (h *AlertsHandler) transition(w http.ResponseWriter, r *http.Request, idStr string, fn func(ctx context.Context, id int64) (Alert, error)) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid alert id", http.StatusBadRequest)
		return
	}
	alert, err := fn(r.Context(), id)
	if err != nil {
		if err == ErrNotFound {
			http.Error(w, "alert not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(alert)
}
