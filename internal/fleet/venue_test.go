package fleet

import (
	"testing"
	"time"
)

func TestPriorityScore_TierDominatesSeverityAgeAndPeak(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // not a peak window

	// Worst-case low-tier score (max severity, max capped age, no peak --
	// low tier isn't PeakEligible anyway) must still lose to a bare-minimum
	// medium-tier score.
	lowMax := priorityScore(venueProfileFor("Education"), SeverityHigh, maxAgeMinutes*time.Minute, now)
	mediumMin := priorityScore(venueProfileFor("Office"), SeverityLow, 0, now)
	if lowMax >= mediumMin {
		t.Fatalf("worst-case low-tier score %v should lose to best-case medium-tier score %v", lowMax, mediumMin)
	}

	// Worst-case medium-tier score (including a hypothetical peak boost)
	// must still lose to a bare-minimum high-tier score.
	mediumMax := priorityScore(venueProfileFor("Office"), SeverityHigh, maxAgeMinutes*time.Minute, now) + peakBoost
	highMin := priorityScore(venueProfileFor("Healthcare"), SeverityLow, 0, now)
	if mediumMax >= highMin {
		t.Fatalf("worst-case medium-tier score %v should lose to best-case high-tier score %v", mediumMax, highMin)
	}
}

func TestPriorityScore_PeakWindowBoostsAirportOnly(t *testing.T) {
	peak := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)   // inside the 11:00-14:00 UTC peak window
	offPeak := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC) // outside any peak window
	airport := venueProfileFor("Airport")
	healthcare := venueProfileFor("Healthcare") // also high tier, but not PeakEligible

	airportPeak := priorityScore(airport, SeverityLow, time.Minute, peak)
	airportOffPeak := priorityScore(airport, SeverityLow, time.Minute, offPeak)
	if airportPeak <= airportOffPeak {
		t.Fatalf("airport peak score %v should exceed off-peak score %v", airportPeak, airportOffPeak)
	}
	if airportPeak-airportOffPeak != peakBoost {
		t.Fatalf("peak boost delta = %v, want exactly %v", airportPeak-airportOffPeak, peakBoost)
	}

	healthcarePeak := priorityScore(healthcare, SeverityLow, time.Minute, peak)
	healthcareOffPeak := priorityScore(healthcare, SeverityLow, time.Minute, offPeak)
	if healthcarePeak != healthcareOffPeak {
		t.Fatalf("healthcare (not peak-eligible) score changed with time: %v vs %v", healthcarePeak, healthcareOffPeak)
	}
}

func TestVenueProfileFor_UnknownVerticalUsesDefault(t *testing.T) {
	got := venueProfileFor("Some Future Vertical")
	if got != defaultVenueProfile {
		t.Fatalf("venueProfileFor(unknown) = %+v, want default %+v", got, defaultVenueProfile)
	}
	if venueProfileFor("").AccessConstrained {
		t.Fatal(`venueProfileFor("") should not be access-constrained`)
	}
}
