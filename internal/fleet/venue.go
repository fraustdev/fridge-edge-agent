package fleet

import "time"

// CriticalityTier is how urgently a venue type's alerts should be handled,
// relative to other venue types.
//
// These tiers, and the access-constrained/peak-window rules below, are an
// ILLUSTRATIVE model built from general public information about what kind
// of constraints airport locations impose (badged/cleared access, sharp
// traffic peaks) -- NOT insider knowledge of Farmer's Fridge's actual
// internal SLAs or staffing policy, which aren't public. See SPEC_V3.md.
type CriticalityTier string

const (
	TierHigh   CriticalityTier = "high"
	TierMedium CriticalityTier = "medium"
	TierLow    CriticalityTier = "low"
)

// tierWeight dominates the priority score (see priorityScore) so that a
// higher-tier venue always outranks a lower-tier one regardless of how the
// smaller severity/age/peak factors land -- see priorityScore's doc comment
// for the worst-case/best-case bound that guarantees this.
var tierWeight = map[CriticalityTier]float64{
	TierHigh:   100,
	TierMedium: 50,
	TierLow:    10,
}

// VenueProfile is one venue type's dispatch-relevant properties.
type VenueProfile struct {
	Tier              CriticalityTier
	AccessConstrained bool // true if only specifically-cleared techs may be assigned
	PeakEligible      bool // true if this venue type gets a priority boost during peak windows
}

// venueProfiles maps the venue "vertical" strings that come from the real
// Farmer's Fridge location data (see cmd/fridge-sim/locations.json) to a
// dispatch profile. A vertical not listed here (including "", for fridges
// with no location data at all) falls back to defaultVenueProfile.
var venueProfiles = map[string]VenueProfile{
	"Airport":    {Tier: TierHigh, AccessConstrained: true, PeakEligible: true},
	"Healthcare": {Tier: TierHigh, AccessConstrained: false},
	"Office":     {Tier: TierMedium, AccessConstrained: false},
	"B&I":        {Tier: TierMedium, AccessConstrained: false},
	"Education":  {Tier: TierLow, AccessConstrained: false},
}

var defaultVenueProfile = VenueProfile{Tier: TierMedium, AccessConstrained: false}

func venueProfileFor(vertical string) VenueProfile {
	if p, ok := venueProfiles[vertical]; ok {
		return p
	}
	return defaultVenueProfile
}

// severityWeight is a smaller secondary factor layered on top of the
// venue tier -- the existing event-type-derived AlertSeverity still
// matters (a hardware fault is worse than a door anomaly at the same
// venue), it just can't override the venue tier itself.
var severityWeight = map[AlertSeverity]float64{
	SeverityHigh:   15,
	SeverityMedium: 8,
	SeverityLow:    0,
}

const (
	maxAgeMinutes = 120 // age contribution is capped so it can't cross a tier boundary
	ageWeight     = 0.1 // max age contribution: maxAgeMinutes * ageWeight = 12
	peakBoost     = 20
)

// isPeakWindow is a deliberately simple, single-timezone (UTC) heuristic --
// not a real per-venue local-time or travel-demand model. It approximates
// the morning/evening commute-analogous windows referenced in SPEC_V3.md.
// Documented as a simplification in README.md.
func isPeakWindow(t time.Time) bool {
	h := t.UTC().Hour()
	return (h >= 11 && h < 14) || (h >= 21 && h < 24)
}

// priorityScore combines a venue's criticality tier (dominant), the
// alert's own severity and age (secondary, tie-breaking factors), and an
// airport peak-window boost (tertiary). tierWeight's gaps (100/50/10) are
// wide enough that severityWeight's max (15) + the capped age contribution
// (12) + peakBoost (20) -- at most 47 -- can never let a lower-tier alert
// outrank a higher-tier one: a maxed-out low-tier score (10+15+12=37) still
// loses to a bare-minimum medium-tier score (50), and a maxed-out medium
// (50+15+12+20=97, though peak only applies to Airport/high-tier in
// practice) still loses to a bare-minimum high-tier score (100).
func priorityScore(profile VenueProfile, severity AlertSeverity, openSince time.Duration, now time.Time) float64 {
	score := tierWeight[profile.Tier]
	score += severityWeight[severity]

	ageMinutes := openSince.Minutes()
	if ageMinutes > maxAgeMinutes {
		ageMinutes = maxAgeMinutes
	}
	if ageMinutes > 0 {
		score += ageMinutes * ageWeight
	}

	if profile.PeakEligible && isPeakWindow(now) {
		score += peakBoost
	}
	return score
}
