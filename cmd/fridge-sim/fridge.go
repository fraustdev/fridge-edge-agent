package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/frida/fridge-edge-agent/internal/dispenser"
	"github.com/frida/fridge-edge-agent/internal/vend"
)

var slots = []string{"A1", "A2", "A3", "B1", "B2"}

// fridgeState bundles everything one simulated fridge needs to generate vend
// events, whether driven by batch mode's fixed loop or daemon mode's
// scheduler. Each fridge owns an independent *rand.Rand (seeded from the
// run's -seed plus the fridge's index) rather than sharing one global
// source: daemon mode dispatches different fridges' events to a worker pool
// concurrently, and math/rand.Rand is not safe for concurrent use. Callers
// must never invoke fireVend concurrently for the *same* fridgeState --
// rng and vendTime are only safe for concurrent use *across* fridges, not
// within one.
type fridgeState struct {
	id             string
	loc            location
	machine        *vend.Machine
	sim            *dispenser.Simulator
	slotPriceCents map[string]int
	initialQty     map[string]int
	totalCapacity  int
	rng            *rand.Rand
	vendTime       time.Time

	// restockPending is daemon-mode scheduling state: true from the moment
	// a restock is scheduled for this fridge until it actually runs, so a
	// fridge lingering just under the threshold doesn't get a restock
	// queued twice. Only ever touched from the single daemon scheduler
	// goroutine -- see runDaemon in daemon.go.
	restockPending bool
}

func newFridgeState(idx int, seed int64, publisher *httpEventPublisher, newPaymentGateway func(*rand.Rand) vend.PaymentGateway) *fridgeState {
	fridgeID := fmt.Sprintf("fridge-%03d", idx+1)
	loc := locationForFridge(fridgeID)
	publisher.locations[fridgeID] = loc

	rng := rand.New(rand.NewSource(seed + int64(idx)))

	// Each slot gets a real menu item, priced once at fridge-creation time
	// and reused for every vend of that slot -- see menu.go.
	slotPriceCents := make(map[string]int, len(slots))
	initialQty := make(map[string]int, len(slots))
	for _, slot := range slots {
		slotPriceCents[slot] = randomMenuItem(rng).randomPriceCents(rng)
		initialQty[slot] = 20
	}

	totalCapacity := 0
	for _, qty := range initialQty {
		totalCapacity += qty
	}

	fs := &fridgeState{
		id:             fridgeID,
		loc:            loc,
		sim:            dispenser.NewSimulator(initialQty),
		slotPriceCents: slotPriceCents,
		initialQty:     initialQty,
		totalCapacity:  totalCapacity,
		rng:            rng,
	}
	fs.machine = &vend.Machine{
		FridgeID:  fridgeID,
		Dispenser: fs.sim,
		Payment:   newPaymentGateway(rng),
		Publisher: publisher,
		Now:       func() time.Time { return fs.vendTime },
	}
	return fs
}

// fireVend sets this fridge's simulated event timestamp, occasionally
// injects a hardware fault first, then drives one vend attempt through the
// dispenser.Simulator + vend.Machine both simulation modes share.
func fireVend(fs *fridgeState, ts time.Time) vend.Outcome {
	slot := slots[fs.rng.Intn(len(slots))]
	fs.vendTime = ts
	if fs.rng.Float64() < hardwareFaultInjectRate {
		injectRandomFault(fs.sim, slot, fs.rng)
	}
	res := fs.machine.Vend(slot, fs.slotPriceCents[slot])
	return res.Outcome
}

// restockThresholdFraction triggers a restock once a fridge's total
// remaining inventory (summed across all slots) drops to this fraction of
// its starting capacity -- roughly "down to one slot's worth remaining."
// This is what makes restock cadence traffic-driven rather than a fixed
// schedule: a busy fridge burns through capacity faster and so crosses
// this threshold (and gets restocked) more often, with no explicit rate
// calculation needed -- the cadence falls out of actual depletion speed.
const restockThresholdFraction = 0.2

// needsRestock reports whether fs has run low enough to schedule a restock,
// and hasn't already got one scheduled.
func needsRestock(fs *fridgeState) bool {
	if fs.restockPending {
		return false
	}
	total := 0
	for _, qty := range fs.sim.Inventory() {
		total += qty
	}
	return float64(total) <= float64(fs.totalCapacity)*restockThresholdFraction
}

// restockFridge refills every slot back to its starting quantity, as if a
// route technician had just visited. It's a cheap, purely in-memory state
// change (dispenser.Simulator guards it with its own mutex) rather than a
// network call, so daemon mode runs it directly from the scheduler
// goroutine instead of routing it through the worker pool.
//
// This does not touch fleet-server's low-stock status: that's derived from
// the restock_alert event already published when a slot first hit empty,
// and (deliberately, per internal/fleet/ingest.go's computeStatus) it
// latches until a human resolves the alert -- restocking fixes the
// simulator's inventory so vends can succeed again, it doesn't retroactively
// clear an alert that already fired.
func restockFridge(fs *fridgeState) {
	for slot, qty := range fs.initialQty {
		fs.sim.Restock(slot, qty)
	}
}

func injectRandomFault(sim *dispenser.Simulator, slot string, rng *rand.Rand) {
	faults := []error{dispenser.ErrJam, dispenser.ErrTimeout, dispenser.ErrSensor}
	sim.InjectFault(slot, faults[rng.Intn(len(faults))])
}
