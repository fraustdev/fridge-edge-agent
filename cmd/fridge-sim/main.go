// Command fridge-sim spins up N simulated fridges, each reusing the v1
// dispenser.Simulator + vend.Machine, drives a batch of transactions with a
// realistic mix of successes and injected failures, and POSTs the resulting
// events to a running fleet-server. This replaces the v1 CLI demo.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/frida/fridge-edge-agent/internal/dispenser"
	"github.com/frida/fridge-edge-agent/internal/vend"
)

func main() {
	var (
		fridgeCount      = flag.Int("fridges", 10, "number of simulated fridges")
		transactionCount = flag.Int("transactions", 20, "vend transactions per fridge")
		serverURL        = flag.String("server", "http://localhost:8080", "fleet-server base URL")
		seed             = flag.Int64("seed", time.Now().UnixNano(), "random seed")
	)
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	publisher := &httpEventPublisher{
		baseURL:   *serverURL,
		client:    &http.Client{Timeout: 5 * time.Second},
		locations: map[string]location{},
	}

	var outcomes = map[vend.Outcome]int{}
	var doorAnomalies int

	for i := 0; i < *fridgeCount; i++ {
		fridgeID := fmt.Sprintf("fridge-%03d", i+1)
		publisher.locations[fridgeID] = usCities[i%len(usCities)]
		sim := dispenser.NewSimulator(map[string]int{
			"A1": 20, "A2": 20, "A3": 20, "B1": 20, "B2": 20,
		})
		payment := &simulatedPaymentGateway{rng: rng}
		machine := &vend.Machine{
			FridgeID:  fridgeID,
			Dispenser: sim,
			Payment:   payment,
			Publisher: publisher,
		}

		slots := []string{"A1", "A2", "A3", "B1", "B2"}
		for j := 0; j < *transactionCount; j++ {
			slot := slots[rng.Intn(len(slots))]

			// Occasionally inject a hardware fault before the vend attempt.
			if rng.Float64() < 0.1 {
				injectRandomFault(sim, slot, rng)
			}

			res := machine.Vend(slot, 350)
			outcomes[res.Outcome]++
		}

		// Occasionally simulate a door-sensor anomaly, independent of vends.
		if rng.Float64() < 0.3 {
			publisher.Publish(vend.Event{
				FridgeID:  fridgeID,
				Type:      vend.EventDoorAnomaly,
				Timestamp: time.Now(),
				Payload:   map[string]any{"reason": "left_open"},
			})
			doorAnomalies++
		}
	}

	log.Printf("simulation complete: %d fridges, %d transactions each", *fridgeCount, *transactionCount)
	for outcome, count := range outcomes {
		log.Printf("  %s: %d", outcome, count)
	}
	log.Printf("  door anomalies: %d", doorAnomalies)

	if publisher.failures > 0 {
		log.Printf("WARNING: %d event(s) failed to POST to %s (is fleet-server running?)", publisher.failures, *serverURL)
		os.Exit(1)
	}
}

// location is a simulated fridge's real-world placement, purely for the
// fleet dashboard's map view — it plays no role in vend/dispatch logic.
type location struct {
	City  string  `json:"city"`
	State string  `json:"state"`
	Lat   float64 `json:"lat"`
	Lng   float64 `json:"lng"`
}

// usCities stands in for the 20-state footprint described in SPEC.md.
// Each simulated fridge is pinned to one of these round-robin.
var usCities = []location{
	{"Chicago", "IL", 41.8781, -87.6298},
	{"New York", "NY", 40.7128, -74.0060},
	{"Los Angeles", "CA", 34.0522, -118.2437},
	{"Boston", "MA", 42.3601, -71.0589},
	{"Washington", "DC", 38.9072, -77.0369},
	{"Philadelphia", "PA", 39.9526, -75.1652},
	{"Dallas", "TX", 32.7767, -96.7970},
	{"Houston", "TX", 29.7604, -95.3698},
	{"Atlanta", "GA", 33.7490, -84.3880},
	{"Denver", "CO", 39.7392, -104.9903},
	{"Seattle", "WA", 47.6062, -122.3321},
	{"San Francisco", "CA", 37.7749, -122.4194},
	{"Minneapolis", "MN", 44.9778, -93.2650},
	{"Detroit", "MI", 42.3314, -83.0458},
	{"Charlotte", "NC", 35.2271, -80.8431},
	{"Nashville", "TN", 36.1627, -86.7816},
	{"Phoenix", "AZ", 33.4484, -112.0740},
	{"Columbus", "OH", 39.9612, -82.9988},
	{"Milwaukee", "WI", 43.0389, -87.9065},
	{"Indianapolis", "IN", 39.7684, -86.1581},
}

func injectRandomFault(sim *dispenser.Simulator, slot string, rng *rand.Rand) {
	faults := []error{dispenser.ErrJam, dispenser.ErrTimeout, dispenser.ErrSensor}
	sim.InjectFault(slot, faults[rng.Intn(len(faults))])
}

// simulatedPaymentGateway stands in for a real payment processor: it
// authorizes and refunds most of the time, with a small chance of each
// failing so the simulator exercises every vend.Outcome.
type simulatedPaymentGateway struct {
	rng     *rand.Rand
	nextTxn int
}

func (p *simulatedPaymentGateway) Authorize(slotID string, amountCents int) (string, error) {
	if p.rng.Float64() < 0.05 {
		return "", errors.New("card declined")
	}
	p.nextTxn++
	return fmt.Sprintf("txn-%d", p.nextTxn), nil
}

func (p *simulatedPaymentGateway) Refund(txnID string) error {
	if p.rng.Float64() < 0.15 {
		return errors.New("payment processor unreachable")
	}
	return nil
}

// httpEventPublisher POSTs each vend.Event to the fleet-server ingestion
// endpoint, translating vend's event shape into the wire format
// fleet.IngestHandler expects.
type httpEventPublisher struct {
	baseURL   string
	client    *http.Client
	failures  int
	locations map[string]location
}

func (p *httpEventPublisher) Publish(e vend.Event) {
	body, err := json.Marshal(map[string]any{
		"fridgeId":  e.FridgeID,
		"slotId":    e.SlotID,
		"type":      string(e.Type),
		"timestamp": e.Timestamp,
		"payload":   e.Payload,
		"location":  p.locations[e.FridgeID],
	})
	if err != nil {
		log.Printf("marshal event: %v", err)
		p.failures++
		return
	}

	resp, err := p.client.Post(p.baseURL+"/fleet/events", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("post event: %v", err)
		p.failures++
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		log.Printf("post event: unexpected status %d", resp.StatusCode)
		p.failures++
	}
}
