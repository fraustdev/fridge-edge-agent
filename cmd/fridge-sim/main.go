// Command fridge-sim spins up N simulated fridges, each reusing the v1
// dispenser.Simulator + vend.Machine, and drives realistic vend traffic
// against a running fleet-server. In batch mode (the default) it fires a
// fixed number of transactions per fridge, spread across today so far, then
// exits. In daemon mode (-daemon) it runs indefinitely, continuously
// scheduling new events per fridge's venue-aware traffic rate, so the
// fleet-server's dashboard reflects an actually-live fleet instead of a
// single frozen snapshot.
package main

import (
	"bytes"
	"context"
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
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/frida/fridge-edge-agent/internal/vend"
)

func main() {
	var (
		fridgeCount      = flag.Int("fridges", 10, "number of simulated fridges")
		transactionCount = flag.Int("transactions", 20, "vend transactions per fridge per run (batch mode) or per simulated day (daemon mode)")
		serverURL        = flag.String("server", "http://localhost:8080", "fleet-server base URL")
		seed             = flag.Int64("seed", time.Now().UnixNano(), "random seed")
		daemon           = flag.Bool("daemon", false, "run indefinitely, continuously generating events instead of one fixed batch")
		speed            = flag.Float64("speed", 1.0, "daemon mode: simulated-time speed multiplier (e.g. 60 = 1 simulated hour per real minute)")
		workers          = flag.Int("workers", 16, "daemon mode: max concurrent outbound POSTs to the fleet server")
	)
	flag.Parse()

	if *daemon && *speed <= 0 {
		log.Fatalf("-speed must be > 0")
	}

	var newPaymentGateway func(rng *rand.Rand) vend.PaymentGateway
	if stripeKey := os.Getenv("STRIPE_SECRET_KEY"); stripeKey != "" {
		log.Printf("STRIPE_SECRET_KEY set: using real Stripe test-mode API calls for payment authorize/refund")
		newPaymentGateway = func(rng *rand.Rand) vend.PaymentGateway { return newStripePaymentGateway(stripeKey, rng) }
	} else {
		log.Printf("STRIPE_SECRET_KEY not set: using simulated in-memory payment gateway")
		newPaymentGateway = func(rng *rand.Rand) vend.PaymentGateway { return &simulatedPaymentGateway{rng: rng} }
	}

	publisher := &httpEventPublisher{
		baseURL:   *serverURL,
		client:    &http.Client{Timeout: 5 * time.Second},
		locations: map[string]location{},
	}

	fridges := make([]*fridgeState, *fridgeCount)
	for i := range fridges {
		fridges[i] = newFridgeState(i, *seed, publisher, newPaymentGateway)
	}

	if *daemon {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runDaemon(ctx, fridges, *transactionCount, *speed, *workers)
		if publisher.failures > 0 {
			log.Printf("%d event(s) failed to POST to %s during this run", publisher.failures, *serverURL)
		}
		return
	}

	runBatch(fridges, *transactionCount, publisher)
}

// runBatch fires a fixed number of transactions per fridge, spread across
// today-so-far, then returns. This is fridge-sim's original (pre-daemon)
// behavior, unchanged in substance -- just rebuilt on top of fridgeState so
// batch and daemon mode share one event-generation path (fireVend).
func runBatch(fridges []*fridgeState, transactionCount int, publisher *httpEventPublisher) {
	outcomes := map[vend.Outcome]int{}
	var doorAnomalies int

	now := time.Now()
	currentHour := now.Hour()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	for _, fs := range fridges {
		// Total attempts and their distribution across the day both come
		// from the fridge's venue type -- see traffic.go.
		attempts := venueAdjustedAttemptCount(transactionCount, fs.loc.Vertical, now)
		timestamps := make([]time.Time, attempts)
		for a := 0; a < attempts; a++ {
			h := sampleHour(fs.rng, fs.loc.Vertical, currentHour)
			ts := todayStart.Add(time.Duration(h)*time.Hour + time.Duration(fs.rng.Intn(3600))*time.Second)
			if ts.After(now) {
				ts = now
			}
			timestamps[a] = ts
		}
		sort.Slice(timestamps, func(a, b int) bool { return timestamps[a].Before(timestamps[b]) })

		for _, ts := range timestamps {
			outcomes[fireVend(fs, ts)]++
		}

		// Occasionally simulate a door-sensor anomaly, independent of vends.
		if fs.rng.Float64() < 0.3 {
			publisher.Publish(vend.Event{
				FridgeID:  fs.id,
				Type:      vend.EventDoorAnomaly,
				Timestamp: now,
				Payload:   map[string]any{"reason": "left_open"},
			})
			doorAnomalies++
		}
	}

	log.Printf("simulation complete: %d fridges, %d transactions each", len(fridges), transactionCount)
	for outcome, count := range outcomes {
		log.Printf("  %s: %d", outcome, count)
	}
	log.Printf("  door anomalies: %d", doorAnomalies)

	if publisher.failures > 0 {
		log.Printf("WARNING: %d event(s) failed to POST to %s (is fleet-server running?)", publisher.failures, publisher.baseURL)
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

// simulatedPaymentGateway stands in for a real payment processor: it
// authorizes and refunds most of the time, with a small chance of each
// failing so the simulator exercises every vend.Outcome.
type simulatedPaymentGateway struct {
	rng     *rand.Rand
	nextTxn int
}

func (p *simulatedPaymentGateway) Authorize(slotID string, amountCents int) (string, error) {
	if p.rng.Float64() < paymentDeclineRate {
		return "", errors.New("card declined")
	}
	p.nextTxn++
	return fmt.Sprintf("txn-%d", p.nextTxn), nil
}

func (p *simulatedPaymentGateway) Refund(txnID string) error {
	if p.rng.Float64() < refundFailureRate {
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
