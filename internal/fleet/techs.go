package fleet

// defaultClearances maps a tech name to the venue "verticals" they're
// cleared to service. This is a small static mock roster, NOT real Farmer's
// Fridge staffing/certification data -- none of that is public. It exists
// purely to demonstrate access-constrained dispatch (see SPEC_V3.md).
//
// A tech name absent from this map is treated as cleared for everything
// (see Dispatcher.isCleared) -- so a caller that builds a Dispatcher with
// its own ad hoc tech names (as most of this package's existing tests do)
// keeps working as plain unconstrained round-robin, unaffected by this
// roster.
var defaultClearances = map[string]map[string]bool{
	"tech-alice": {"Airport": true, "Healthcare": true},
	"tech-bob":   {"Office": true, "B&I": true, "Education": true},
	"tech-carol": {"Airport": true, "Office": true, "B&I": true, "Education": true, "Healthcare": true},
}
