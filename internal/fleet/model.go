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

// Location is a fridge's real-world placement. It's not used for anything
// beyond display — the fleet-wide/per-fridge health logic never depends on
// it — but a national fleet is easier to make sense of on a map than in a
// flat list.
type Location struct {
	Address  string  `json:"address,omitempty"` // full street address, e.g. "309 Cedar Ln, Florence, NJ 08518"
	City     string  `json:"city"`
	State    string  `json:"state"`
	Zip      string  `json:"zip,omitempty"`
	Vertical string  `json:"vertical,omitempty"` // e.g. "Airport", "Healthcare", "B&I", "Office"
	Name     string  `json:"name,omitempty"`     // e.g. "DFW Airport - Gate B2"
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

// Fridge is the current known state of one fridge.
type Fridge struct {
	ID           string       `json:"id"`
	Status       FridgeStatus `json:"status"`
	LastEventAt  time.Time    `json:"lastEventAt"`
	OpenAlertIDs []int64      `json:"openAlertIds,omitempty"`
	Location     *Location    `json:"location,omitempty"`
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
	ID            int64         `json:"id"`
	FridgeID      string        `json:"fridgeId"`
	SlotID        string        `json:"slotId,omitempty"`
	SourceEvent   EventType     `json:"sourceEvent"`
	Severity      AlertSeverity `json:"severity"`
	Status        AlertStatus   `json:"status"`
	AssignedTo    string        `json:"assignedTo,omitempty"`
	BlockedReason string        `json:"blockedReason,omitempty"` // set when Assign found no eligible tech; stays "open" otherwise
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`

	// AssignmentScore, AccessState, and EscortTech record the dispatch
	// decision made at assignment time (see dispatch.go's scoreCandidates
	// and resolveAccess) -- they're stored, not recomputed at read time,
	// because they describe why THAT assignment was made; recomputing them
	// later against a tech's now-different position/workload would produce
	// a number that no longer matches the actual decision.
	AssignmentScore *float64    `json:"assignmentScore,omitempty"`
	AccessState     AccessState `json:"accessState,omitempty"` // only set for access-constrained venues (see venue.go)
	EscortTech      string      `json:"escortTech,omitempty"`  // set when AccessState == escort_required
}

// TechRole is a field technician's specialty. Farmer's Fridge job postings
// publicly describe two distinct field roles -- a route-based Delivery
// Driver and a separate Service Technician for installs/repairs/audits --
// this mirrors that real distinction. See README for the sourcing note.
type TechRole string

const (
	RoleDriver      TechRole = "driver"       // route-based restocking
	RoleServiceTech TechRole = "service_tech" // installs, repairs, PM audits, inspections
)

// AccessState is how an access-constrained-venue alert (see
// VenueProfile.AccessConstrained) was able to be staffed, modeled on how
// airport SIDA badge access actually works: badges are venue-specific and
// individually earned, but a badged person can escort an unbadged person
// into a secured area, staying with them continuously. See README for what
// in this model is grounded in that public fact versus illustrative.
type AccessState string

const (
	// AccessAssigned: a role-matched tech individually cleared for this
	// venue is available -- dispatch as normal.
	AccessAssigned AccessState = "assigned"
	// AccessEscortRequired: no individually-cleared, role-matched tech is
	// available, but a role-matched (uncleared) tech AND a cleared tech of
	// any role (the escort) both exist -- the cleared tech can badge the
	// uncleared one in.
	AccessEscortRequired AccessState = "escort_required"
	// AccessBlocked: neither of the above is possible right now.
	AccessBlocked AccessState = "blocked"
)

// Tech is one field technician: static identity/role/home-depot fields plus
// live dispatch state (current position, in-flight job). Live-state fields
// are unexported -- they're internal to Dispatcher's bookkeeping, never
// marshaled directly; TechView (below) is the read-time-computed public
// shape GET /fleet/techs actually returns. Dispatch state is intentionally
// NOT persisted to the Store -- see Dispatcher's doc comment for why an
// in-memory roster is the right scope for this demo.
type Tech struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Role    TechRole `json:"role"`
	HomeLat float64  `json:"homeLat"`
	HomeLng float64  `json:"homeLng"`

	// Live state -- zero values mean "idle, parked at home".
	originLat, originLng float64
	destLat, destLng     float64
	assignedAt           time.Time
	eta                  time.Time
	currentAlertID       int64
}

// TechView is a Tech plus its read-time-computed live position and
// workload -- what GET /fleet/techs actually returns (see dispatch.go's
// currentPosition).
type TechView struct {
	Tech
	Lat            float64    `json:"lat"`
	Lng            float64    `json:"lng"`
	Idle           bool       `json:"idle"`
	Workload       int        `json:"workload"`
	CurrentAlertID int64      `json:"currentAlertId,omitempty"`
	ETA            *time.Time `json:"eta,omitempty"`
}

// ReassignmentLogEntry records a manual override of an auto-assignment: who
// changed it, when, which alert, and what the auto-assignment (FromTech)
// had been -- so the override is comparable against what the system would
// have done on its own. In-memory only, same scope decision as Tech state.
type ReassignmentLogEntry struct {
	ID       int       `json:"id"`
	AlertID  int64     `json:"alertId"`
	FromTech string    `json:"fromTech"`
	ToTech   string    `json:"toTech"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}
