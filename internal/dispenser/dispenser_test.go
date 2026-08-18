package dispenser

import (
	"errors"
	"testing"
)

func TestSimulator_Dispense(t *testing.T) {
	tests := []struct {
		name        string
		initial     map[string]int
		injectFault error
		slotID      string
		wantErr     error
		wantQty     int // expected remaining qty after the call, ignored if wantErr wraps ErrUnknownSlot
	}{
		{
			name:    "successful dispense decrements quantity",
			initial: map[string]int{"A1": 3},
			slotID:  "A1",
			wantErr: nil,
			wantQty: 2,
		},
		{
			name:    "empty slot returns ErrSlotEmpty",
			initial: map[string]int{"A1": 0},
			slotID:  "A1",
			wantErr: ErrSlotEmpty,
			wantQty: 0,
		},
		{
			name:    "unknown slot returns ErrUnknownSlot",
			initial: map[string]int{"A1": 3},
			slotID:  "Z9",
			wantErr: ErrUnknownSlot,
		},
		{
			name:        "injected jam fault is returned instead of normal dispense",
			initial:     map[string]int{"A1": 3},
			injectFault: ErrJam,
			slotID:      "A1",
			wantErr:     ErrJam,
			wantQty:     3,
		},
		{
			name:        "injected timeout fault is returned instead of normal dispense",
			initial:     map[string]int{"A1": 3},
			injectFault: ErrTimeout,
			slotID:      "A1",
			wantErr:     ErrTimeout,
			wantQty:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := NewSimulator(tt.initial)
			if tt.injectFault != nil {
				sim.InjectFault(tt.slotID, tt.injectFault)
			}

			err := sim.Dispense(tt.slotID)

			if tt.wantErr == nil && err != nil {
				t.Fatalf("Dispense() unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Dispense() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil || errors.Is(tt.wantErr, ErrSlotEmpty) || errors.Is(tt.wantErr, ErrJam) || errors.Is(tt.wantErr, ErrTimeout) {
				if got := sim.Inventory()[tt.slotID]; got != tt.wantQty {
					t.Fatalf("Inventory()[%s] = %d, want %d", tt.slotID, got, tt.wantQty)
				}
			}
		})
	}
}

func TestSimulator_InjectFault_IsOneShot(t *testing.T) {
	sim := NewSimulator(map[string]int{"A1": 3})
	sim.InjectFault("A1", ErrSensor)

	if err := sim.Dispense("A1"); !errors.Is(err, ErrSensor) {
		t.Fatalf("first Dispense() error = %v, want ErrSensor", err)
	}
	if err := sim.Dispense("A1"); err != nil {
		t.Fatalf("second Dispense() should succeed after one-shot fault consumed, got %v", err)
	}
	if got := sim.Inventory()["A1"]; got != 2 {
		t.Fatalf("Inventory()[A1] = %d, want 2", got)
	}
}

func TestSimulator_LowStockSlots(t *testing.T) {
	sim := NewSimulator(map[string]int{"A1": 0, "A2": 1, "A3": 5})

	low := sim.LowStockSlots(1)

	want := map[string]bool{"A1": true, "A2": true}
	if len(low) != len(want) {
		t.Fatalf("LowStockSlots(1) = %v, want slots matching %v", low, want)
	}
	for _, id := range low {
		if !want[id] {
			t.Fatalf("LowStockSlots(1) unexpectedly included %s", id)
		}
	}
}

func TestSimulator_Inventory_IsSnapshotCopy(t *testing.T) {
	sim := NewSimulator(map[string]int{"A1": 3})
	inv := sim.Inventory()
	inv["A1"] = 999

	if got := sim.Inventory()["A1"]; got != 3 {
		t.Fatalf("mutating returned Inventory() map affected simulator state: got %d, want 3", got)
	}
}
