// Command fridge-sim spins up N simulated fridges, each reusing the v1
// dispenser.Simulator + vend.Machine, drives a batch of transactions with a
// realistic mix of successes and injected failures, and POSTs the resulting
// events to a running fleet-server. This replaces the v1 CLI demo.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
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

	var newPaymentGateway func() vend.PaymentGateway
	if stripeKey := os.Getenv("STRIPE_SECRET_KEY"); stripeKey != "" {
		log.Printf("STRIPE_SECRET_KEY set: using real Stripe test-mode API calls for payment authorize/refund")
		gw := newStripePaymentGateway(stripeKey, rng)
		newPaymentGateway = func() vend.PaymentGateway { return gw }
	} else {
		log.Printf("STRIPE_SECRET_KEY not set: using simulated in-memory payment gateway")
		newPaymentGateway = func() vend.PaymentGateway { return &simulatedPaymentGateway{rng: rng} }
	}

	publisher := &httpEventPublisher{
		baseURL:   *serverURL,
		client:    &http.Client{Timeout: 5 * time.Second},
		locations: map[string]location{},
	}

	var outcomes = map[vend.Outcome]int{}
	var doorAnomalies int

	for i := 0; i < *fridgeCount; i++ {
		fridgeID := fmt.Sprintf("fridge-%03d", i+1)
		publisher.locations[fridgeID] = locationForFridge(fridgeID)
		sim := dispenser.NewSimulator(map[string]int{
			"A1": 20, "A2": 20, "A3": 20, "B1": 20, "B2": 20,
		})
		payment := newPaymentGateway()
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
	FridgeID string  `json:"fridgeId"`
	Name     string  `json:"name"`
	Vertical string  `json:"vertical"`
	Access   string  `json:"access"`
	Address  string  `json:"address"`
	City     string  `json:"city"`
	State    string  `json:"state"`
	Zip      string  `json:"zip"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

//go:embed locations.json
var locationsJSON []byte

// realLocations is Farmer's Fridge's own real, individual fridge locations
// (address, coordinates, and venue type for each) -- not invented. Pulled
// directly from the JSON payload that powers farmersfridge.com/locations-map/
// (their Gatsby build's public page-data endpoint, the same data any visitor's
// browser downloads to render that page), captured 2026-08-24. 2,329 usable
// records across 22 states/DC. This is a snapshot of public reporting, not a
// live feed of their current fleet. Each simulated fridge is pinned to one of
// these; running with -fridges >= len(realLocations) covers every one of them.
var realLocations = func() []location {
	var locs []location
	if err := json.Unmarshal(locationsJSON, &locs); err != nil {
		panic("parse embedded locations.json: " + err.Error())
	}
	return locs
}()

// locationForFridge deterministically maps a fridge ID to exactly one real
// location, stable across every run regardless of -seed, -fridges count, or
// how many times the simulator has been re-run against the same
// fleet-server: fridge-001's address today is fridge-001's address next
// week too. Hashing the ID (rather than shuffling a pool by loop position)
// is what gives that stability -- the assignment depends only on the ID
// string, never on iteration order.
func locationForFridge(fridgeID string) location {
	h := fnv.New32a()
	h.Write([]byte(fridgeID))
	idx := int(h.Sum32() % uint32(len(realLocations)))
	return realLocations[idx]
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
