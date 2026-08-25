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
	rng            *rand.Rand
	vendTime       time.Time
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

	fs := &fridgeState{
		id:             fridgeID,
		loc:            loc,
		sim:            dispenser.NewSimulator(initialQty),
		slotPriceCents: slotPriceCents,
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

func injectRandomFault(sim *dispenser.Simulator, slot string, rng *rand.Rand) {
	faults := []error{dispenser.ErrJam, dispenser.ErrTimeout, dispenser.ErrSensor}
	sim.InjectFault(slot, faults[rng.Intn(len(faults))])
}
