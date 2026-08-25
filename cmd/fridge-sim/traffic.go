package main

import (
	"math"
	"math/rand"
	"time"
)

// hourlyMultiplier gives each hour of the day (0-23) a relative
// vend-attempt-frequency weight, by venue type. This is a simple
// illustrative heuristic -- not a real demand model, and not based on any
// of Farmer's Fridge's actual traffic data (not public). Shapes:
//   - Office/B&I: a pronounced late-morning/midday lunch peak, near-zero
//     overnight.
//   - Airport: flatter than Office across the day but a higher floor
//     throughout, with extra peaks around typical early-morning and
//     early-evening travel windows.
//   - Healthcare: closest to round-the-clock, the smallest peak/trough
//     spread of the four.
//   - Education: the same curve as Office -- differentiated only by
//     weekendFactor below, per this addendum's documented simplification.
var hourlyMultiplier = map[string][24]float64{
	"Office": {
		0.05, 0.05, 0.05, 0.05, 0.05, 0.1, 0.3, 0.8,
		1.2, 1.0, 1.1, 2.2, 3.0, 2.0, 1.0, 0.9,
		0.8, 0.7, 0.4, 0.2, 0.1, 0.05, 0.05, 0.05,
	},
	"B&I": {
		0.05, 0.05, 0.05, 0.05, 0.05, 0.1, 0.3, 0.8,
		1.2, 1.0, 1.1, 2.2, 3.0, 2.0, 1.0, 0.9,
		0.8, 0.7, 0.4, 0.2, 0.1, 0.05, 0.05, 0.05,
	},
	"Education": {
		0.05, 0.05, 0.05, 0.05, 0.05, 0.1, 0.3, 0.8,
		1.2, 1.0, 1.1, 2.2, 3.0, 2.0, 1.0, 0.9,
		0.8, 0.7, 0.4, 0.2, 0.1, 0.05, 0.05, 0.05,
	},
	"Airport": {
		1.4, 1.2, 1.0, 0.9, 1.1, 1.8, 2.4, 2.2,
		1.8, 1.5, 1.4, 1.6, 1.8, 1.6, 1.5, 1.6,
		1.9, 2.3, 2.1, 1.7, 1.5, 1.4, 1.4, 1.4,
	},
	"Healthcare": {
		0.8, 0.7, 0.6, 0.6, 0.6, 0.7, 0.9, 1.1,
		1.3, 1.3, 1.3, 1.4, 1.5, 1.4, 1.3, 1.3,
		1.2, 1.2, 1.1, 1.0, 0.9, 0.9, 0.8, 0.8,
	},
}

var defaultHourlyMultiplier = hourlyMultiplier["Office"]

func multiplierFor(vertical string) [24]float64 {
	if m, ok := hourlyMultiplier[vertical]; ok {
		return m
	}
	return defaultHourlyMultiplier
}

// weekendFactor dampens Education-venue traffic on weekends -- a fleet-wide
// simplification (not per-fridge/per-timezone) per this addendum's
// documented scope; every other venue type is unaffected.
func weekendFactor(vertical string, t time.Time) float64 {
	if vertical != "Education" {
		return 1.0
	}
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return 0.2
	default:
		return 1.0
	}
}

// avgMultiplier is a venue's average hourly weight, used to scale total
// daily attempt volume so e.g. Airport fridges see more total traffic than
// Education ones -- not just a differently-shaped day.
func avgMultiplier(vertical string) float64 {
	m := multiplierFor(vertical)
	var sum float64
	for _, v := range m {
		sum += v
	}
	return sum / 24
}

// venueAdjustedAttemptCount scales a base per-fridge transaction count by
// the venue's average traffic level and (for Education) weekend damping,
// always returning at least 1 so every fridge reports something.
func venueAdjustedAttemptCount(base int, vertical string, now time.Time) int {
	n := float64(base) * avgMultiplier(vertical) * weekendFactor(vertical, now)
	count := int(math.Round(n))
	if count < 1 {
		count = 1
	}
	return count
}

// eventRatePerHour returns a fridge's expected vend attempts per simulated
// hour, given a target average attempts-per-day (dailyTarget), distributing
// that daily total across hours proportionally to the venue's
// hourlyMultiplier shape -- the daemon-mode equivalent of
// venueAdjustedAttemptCount's one-shot scaling, so a lunch-hour peak still
// emerges in a continuous stream rather than a fixed batch.
func eventRatePerHour(dailyTarget int, vertical string, t time.Time) float64 {
	avg := avgMultiplier(vertical)
	if avg <= 0 {
		avg = 1
	}
	w := multiplierFor(vertical)[t.Hour()]
	rate := float64(dailyTarget) / 24 * w / avg * weekendFactor(vertical, t)

	// Floor so a dead-zone hour (e.g. 3am at an Office fridge) still
	// produces a finite next-event time instead of an effectively-infinite
	// wait -- matches how real fridges do occasionally see off-hours
	// activity, just rarely.
	const minRatePerHour = 0.01
	if rate < minRatePerHour {
		rate = minRatePerHour
	}
	return rate
}

// sampleNextInterval draws the gap until this fridge's next simulated vend
// attempt from an exponential distribution parameterized by the current
// hour's rate -- an approximation of a non-homogeneous Poisson process (the
// true rate changes continuously through the day; here it's treated as
// constant for the duration of the sampled gap). Good enough for a
// demo-quality traffic pattern, not a claim of statistical rigor.
func sampleNextInterval(rng *rand.Rand, dailyTarget int, vertical string, from time.Time) time.Duration {
	rate := eventRatePerHour(dailyTarget, vertical, from)
	hours := rng.ExpFloat64() / rate
	return time.Duration(hours * float64(time.Hour))
}

// sampleHour picks one hour in [0, maxHour] weighted by the venue's hourly
// multiplier curve, restricted to hours that have already elapsed today so
// every generated timestamp lands in the past relative to "now" -- this is
// a single-clock (not per-location-timezone) simplification, same as
// fleet.isPeakWindow's documented UTC simplification.
func sampleHour(rng *rand.Rand, vertical string, maxHour int) int {
	weights := multiplierFor(vertical)
	var total float64
	for h := 0; h <= maxHour; h++ {
		total += weights[h]
	}
	if total <= 0 {
		return maxHour
	}
	r := rng.Float64() * total
	for h := 0; h <= maxHour; h++ {
		r -= weights[h]
		if r <= 0 {
			return h
		}
	}
	return maxHour
}
