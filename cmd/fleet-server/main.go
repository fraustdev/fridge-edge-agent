// Command fleet-server runs the fleet backend HTTP API: event ingestion,
// fleet/per-fridge status reads, alert dispatch, and the ops copilot
// endpoint. See SPEC.md for the problem this replaces.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/frida/fridge-edge-agent/internal/copilot"
	"github.com/frida/fridge-edge-agent/internal/fleet"
)

func main() {
	addr := os.Getenv("FLEET_SERVER_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	dbPath := os.Getenv("FLEET_DB_PATH")
	if dbPath == "" {
		dbPath = "fleet.db"
	}

	store, err := fleet.OpenSQLiteStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	dispatcher := fleet.NewDispatcher(store, fleet.DefaultTechRoster())
	summarizer := copilot.NewSummarizer()

	mux := http.NewServeMux()
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/static/leaflet.js", leafletJSHandler)
	mux.HandleFunc("/static/leaflet.css", leafletCSSHandler)
	mux.HandleFunc("/static/fonts/", fontHandler)
	mux.Handle("/fleet/events", fleet.NewIngestHandler(store, dispatcher))
	mux.Handle("/fleet/status", fleet.NewStatusHandler(store))
	mux.Handle("/fleet/fridges/", fleet.NewFridgeDetailHandler(store))
	mux.Handle("/fleet/alerts", fleet.NewAlertsHandler(store, dispatcher))
	mux.Handle("/fleet/alerts/", fleet.NewAlertsHandler(store, dispatcher))
	mux.Handle("/fleet/techs", fleet.NewTechsHandler(dispatcher))
	mux.HandleFunc("/fleet/copilot/summary", copilotSummaryHandler(store, summarizer))

	log.Printf("fleet-server listening on %s (db: %s)", addr, dbPath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// copilotSummaryHandler summarizes the most recent fleet-wide events. It's
// a reporting layer only — it never influences dispatch or vend decisions.
func copilotSummaryHandler(store fleet.Store, summarizer *copilot.Summarizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		events, err := store.ListRecentEvents(ctx, 200)
		if err != nil {
			http.Error(w, "failed to list events: "+err.Error(), http.StatusInternalServerError)
			return
		}

		inputs := make([]copilot.EventSummaryInput, len(events))
		for i, e := range events {
			inputs[i] = copilot.EventSummaryInput{
				FridgeID:  e.FridgeID,
				SlotID:    e.SlotID,
				Type:      string(e.Type),
				Timestamp: e.Timestamp,
				Payload:   e.Payload,
			}
		}

		report := summarizer.Summarize(ctx, inputs)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(report)
	}
}
