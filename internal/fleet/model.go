// Package fleet is the fleet-side backend: ingesting events from many
// fridges, tracking per-fridge state, and triaging alerts into a dispatch
// lifecycle. It is the direct replacement for the manual-Slack workflow
// described in SPEC.md.
package fleet

import "time"

// EventType mirrors vend.EventType — duplicated here rather than imported so
// the fleet package's wire format doesn't depend on vend's Go types.
type EventType string

const (
	EventVendCompleted EventType = "vend_completed"
	EventRestockAlert  EventType = "restock_alert"
	EventHardwareFault EventType = "hardware_fault"
	EventDoorAnomaly   EventType = "door_anomaly"
)

// Event is one fact reported by a fridge.
type Event struct {
	ID        int64          `json:"id"`
	FridgeID  string         `json:"fridgeId"`
	SlotID    string         `json:"slotId,omitempty"`
	Type      EventType      `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// FridgeStatus is the fleet-wide health classification for one fridge.
type FridgeStatus string

const (
	StatusHealthy  FridgeStatus = "healthy"
	StatusLowStock FridgeStatus = "low_stock"
	StatusFaulted  FridgeStatus = "faulted"
	StatusOffline  FridgeStatus = "offline"
)

// Fridge is the current known state of one fridge.
type Fridge struct {
	ID           string       `json:"id"`
	Status       FridgeStatus `json:"status"`
	LastEventAt  time.Time    `json:"lastEventAt"`
	OpenAlertIDs []int64      `json:"openAlertIds,omitempty"`
}

// AlertStatus is where an alert sits in the dispatch lifecycle.
type AlertStatus string

const (
	AlertOpen     AlertStatus = "open"
	AlertAssigned AlertStatus = "assigned"
	AlertResolved AlertStatus = "resolved"
)

// AlertSeverity is a coarse triage bucket driving assignment order.
type AlertSeverity string

const (
	SeverityLow    AlertSeverity = "low"
	SeverityMedium AlertSeverity = "medium"
	SeverityHigh   AlertSeverity = "high"
)

// Alert is one open item in the dispatch lifecycle: open -> assigned -> resolved.
type Alert struct {
	ID          int64         `json:"id"`
	FridgeID    string        `json:"fridgeId"`
	SlotID      string        `json:"slotId,omitempty"`
	SourceEvent EventType     `json:"sourceEvent"`
	Severity    AlertSeverity `json:"severity"`
	Status      AlertStatus   `json:"status"`
	AssignedTo  string        `json:"assignedTo,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
	UpdatedAt   time.Time     `json:"updatedAt"`
}
