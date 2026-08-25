package fleet

// defaultTechRoster is a small, illustrative field-tech roster -- NOT real
// Farmer's Fridge staffing data (none of that is public). It exists to
// demonstrate role-based, access-constrained, geo-aware dispatch (see
// README's v6 section). Home coordinates are plausible regional depots
// (Chicago, Dallas-Fort Worth), chosen because those metros have a real
// concentration of Farmer's Fridge locations in the embedded dataset
// (cmd/fridge-sim/locations.json) -- not any claim about actual depots.
var defaultTechRoster = []Tech{
	{ID: "tech-alice", Name: "Alice Chen", Role: RoleServiceTech, HomeLat: 41.9742, HomeLng: -87.9073}, // near O'Hare
	{ID: "tech-bob", Name: "Bob Nguyen", Role: RoleDriver, HomeLat: 41.8781, HomeLng: -87.6298},         // Chicago Loop
	{ID: "tech-carol", Name: "Carol Diaz", Role: RoleServiceTech, HomeLat: 32.8998, HomeLng: -97.0403},  // near DFW
	{ID: "tech-dave", Name: "Dave Okafor", Role: RoleDriver, HomeLat: 32.7767, HomeLng: -96.7970},       // Dallas
	{ID: "tech-erin", Name: "Erin Walsh", Role: RoleServiceTech, HomeLat: 41.8781, HomeLng: -87.6298},   // Chicago Loop
}

// defaultClearances maps a tech ID to the venue "verticals" they're
// individually cleared to service (see AccessState). Same "not real
// certification data" caveat as defaultTechRoster.
//
// A tech ID absent from this map is treated as cleared for everything (see
// Dispatcher.isCleared) -- so a caller that builds a Dispatcher with its own
// ad hoc tech IDs (as many of this package's tests do) keeps working as
// plain unconstrained dispatch, unaffected by this roster.
var defaultClearances = map[string]map[string]bool{
	"tech-alice": {"Airport": true, "Healthcare": true},
	"tech-bob":   {"Office": true, "B&I": true, "Education": true},
	"tech-carol": {"Airport": true, "Office": true, "B&I": true, "Education": true, "Healthcare": true},
	"tech-dave":  {"Airport": true, "Healthcare": true}, // a Driver cleared for Airport -- can badge a ServiceTech in as an escort, but can't do ServiceTech work themself
	"tech-erin":  {"Office": true, "B&I": true, "Education": true, "Healthcare": true},
}
