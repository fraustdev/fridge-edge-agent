// Package dispenser models the hardware boundary of a single smart fridge:
// the physical mechanism that dispenses an item from a slot. Dispenser is
// an interface so the rest of the system never depends on real hardware;
// Simulator is the only implementation for this project.
package dispenser

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrUnknownSlot = errors.New("unknown slot")
	ErrSlotEmpty   = errors.New("slot empty")
	ErrJam         = errors.New("dispenser jam")
	ErrTimeout     = errors.New("dispenser timeout")
	ErrSensor      = errors.New("sensor error")
)

// Dispenser is the hardware boundary: given a slot ID, physically dispense
// one item from that slot.
type Dispenser interface {
	Dispense(slotID string) error
	Inventory() map[string]int
}

// Simulator is an in-memory Dispenser used in place of real hardware.
// It supports injecting one-shot faults so tests and the fridge simulator
// can exercise every failure path a real dispenser could hit.
type Simulator struct {
	mu     sync.Mutex
	slots  map[string]int
	faults map[string]error
}

// NewSimulator creates a Simulator seeded with the given slot quantities.
func NewSimulator(initial map[string]int) *Simulator {
	slots := make(map[string]int, len(initial))
	for id, qty := range initial {
		slots[id] = qty
	}
	return &Simulator{
		slots:  slots,
		faults: make(map[string]error),
	}
}

// InjectFault forces the next Dispense call on slotID to return err instead
// of performing its normal logic. The fault is consumed after one call.
func (s *Simulator) InjectFault(slotID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[slotID] = err
}

func (s *Simulator) Dispense(slotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err, ok := s.faults[slotID]; ok {
		delete(s.faults, slotID)
		return err
	}

	qty, ok := s.slots[slotID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSlot, slotID)
	}
	if qty <= 0 {
		return ErrSlotEmpty
	}
	s.slots[slotID] = qty - 1
	return nil
}

// Inventory returns a snapshot of remaining quantity per slot.
func (s *Simulator) Inventory() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]int, len(s.slots))
	for id, qty := range s.slots {
		out[id] = qty
	}
	return out
}

// Restock sets slotID's remaining quantity back to qty, as if a technician
// had just refilled it. Restocking a slot ID this Simulator wasn't created
// with is a no-op -- Restock can only refill an existing slot, never add a
// new one.
func (s *Simulator) Restock(slotID string, qty int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.slots[slotID]; !ok {
		return
	}
	s.slots[slotID] = qty
}

// LowStockSlots returns the slot IDs whose remaining quantity is at or
// below threshold, sorted is not guaranteed — callers needing stable order
// should sort the result themselves.
func (s *Simulator) LowStockSlots(threshold int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var low []string
	for id, qty := range s.slots {
		if qty <= threshold {
			low = append(low, id)
		}
	}
	return low
}
