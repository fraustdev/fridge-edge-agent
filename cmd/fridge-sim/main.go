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
		publisher.locations[fridgeID] = usAirports[i%len(usAirports)]
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
	City    string  `json:"city"`
	State   string  `json:"state"`
	Airport string  `json:"airport,omitempty"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
}

// usAirports is Farmer's Fridge's own publicly reported airport footprint,
// not an invented city list — sourced from farmersfridge.com/blog/airport-locations/
// and corroborating reporting (Fast Company, Fast Casual), checked 2026-08-18.
// It's a snapshot of public reporting, not a live feed of their real fleet,
// and airport terminal coordinates are approximate. Each simulated fridge is
// pinned to one of these round-robin — multiple fridges landing on the same
// airport is realistic (Farmer's Fridge runs several units per terminal).
var usAirports = []location{
	{"Chicago", "IL", "ORD", 41.9742, -87.9073},
	{"Chicago", "IL", "MDW", 41.7868, -87.7522},
	{"Minneapolis", "MN", "MSP", 44.8848, -93.2223},
	{"Milwaukee", "WI", "MKE", 42.9472, -87.8966},
	{"Cincinnati", "KY", "CVG", 39.0533, -84.6630},
	{"Columbus", "OH", "CMH", 39.9980, -82.8919},
	{"Indianapolis", "IN", "IND", 39.7173, -86.2944},
	{"St. Louis", "MO", "STL", 38.7487, -90.3700},
	{"Newark", "NJ", "EWR", 40.6895, -74.1745},
	{"New York", "NY", "JFK", 40.6413, -73.7781},
	{"New York", "NY", "LGA", 40.7769, -73.8740},
	{"Philadelphia", "PA", "PHL", 39.8744, -75.2424},
	{"Boston", "MA", "BOS", 42.3656, -71.0096},
	{"Washington", "DC", "DCA", 38.8512, -77.0402},
	{"Washington", "VA", "IAD", 38.9531, -77.4565},
	{"Baltimore", "MD", "BWI", 39.1774, -76.6684},
	{"Nashville", "TN", "BNA", 36.1263, -86.6774},
	{"Houston", "TX", "IAH", 29.9902, -95.3368},
	{"Dallas", "TX", "DFW", 32.8998, -97.0403},
	{"Dallas", "TX", "DAL", 32.8471, -96.8518},
	{"Austin", "TX", "AUS", 30.1975, -97.6664},
	{"Los Angeles", "CA", "LAX", 33.9416, -118.4085},
	{"Ontario", "CA", "ONT", 34.0559, -117.6011},
	{"Atlanta", "GA", "ATL", 33.6407, -84.4277},
	{"Las Vegas", "NV", "LAS", 36.0840, -115.1537},
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
