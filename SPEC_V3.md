# Addendum: Venue-Aware Dispatch (v3)

This is an addition to the existing Fridge Fleet Ops Backend
(`fraustdev/fridge-edge-agent`), not a rewrite. It targets one specific gap
in the current `internal/fleet` dispatch logic: alerts are triaged and
assigned with plain round-robin, with no awareness of *where* the fridge
actually is. That's no longer a reasonable simplification given what's
public about the company's current trajectory.

## Why now, specifically

Farmer's Fridge is in the middle of an aggressive, federally-backed airport
expansion, not a hypothetical one:

- They've expanded into more than 20 major U.S. airports, and CEO Luke
  Saunders spoke at a press briefing at Reagan National alongside the HHS
  Secretary and the U.S. Transportation Secretary, who called fresh-food
  kiosks the preferred model for the future of airport dining — this is
  tied to federal infrastructure-law funding for "enhanced passenger
  experiences" (Fast Casual, Jan 2026).
- New airport locations have kept opening steadily through 2026: Boston
  Logan (May, ahead of a FIFA-related travel surge), George Bush
  Houston (Aug 2026), among others.

This matters operationally because an airport fridge is not the same
serviceability problem as an office-lobby fridge:

- **Access is gated.** Post-security airport locations require a
  badged/cleared technician — you can't send "whoever's nearest."
- **Traffic isn't flat.** Airports have sharp peak windows (holidays, big
  travel days) where a stockout is far costlier than the same stockout at
  3am.
- **Visibility is higher.** A broken fridge in a federally-spotlighted
  expansion is a worse look than the same failure in a hospital break
  room.

The current dispatch logic (round-robin, no venue field consulted) doesn't
model any of this, even though the fridge-sim harness already tags every
simulated fridge with a real venue type (Airport/Healthcare/B&I/Office/
Education) from real location data. The data's already there; the
dispatch logic just isn't using it yet.

## What to build

### 1. Venue-aware SLA tiers
- Add a criticality tier per venue type (e.g. Airport/Healthcare = high,
  Office/B&I = medium, Education = lower — this ordering is a judgment
  call to state plainly in the README, not a claim about Farmer's Fridge's
  actual internal SLAs, which aren't public).
- Each open alert gets an effective priority score combining tier +
  time-open, so a high-tier alert doesn't get starved behind a long
  round-robin queue of low-tier ones.
- Dispatch picks the highest-priority open alert first, not FIFO.

### 2. Access-constrained assignment
- Add an `access_constrained` flag on venue type (true for
  Airport-post-security, at minimum) meaning: only techs flagged as
  "cleared" for that specific venue can be assigned.
- Simple model: a small static roster of techs, each with a set of venues
  (or venue types) they're cleared for. Assignment logic filters to
  eligible techs first, then applies whatever base assignment rule
  (round-robin within the eligible subset).
- If no cleared tech is available, the alert stays `open` with a visible
  "blocked: no eligible tech" reason — this is itself a useful signal, not
  a bug to hide.

### 3. Peak-window weighting
- A simple time-of-day heuristic (not a real traffic model): airport
  venues get a priority boost during defined peak windows (e.g. early
  morning and evening commute-analogous hours, configurable).
- This is deliberately simple — the point is demonstrating that the
  dispatch model treats "when" as well as "where," not building real
  travel-demand forecasting.

### 4. Dashboard updates
- Alert table: show priority score and, when applicable, the "blocked: no
  eligible tech" state distinctly (not just "open").
- Per-fridge card view: show venue type and its criticality tier.
- No new pages needed — this extends the existing dashboard, doesn't
  replace it.

## Explicitly out of scope for this addendum

- Real technician roster/certification data (still a small static mock
  roster — no real Farmer's Fridge staffing data exists to model this on).
- Real travel-demand forecasting (peak windows are a static heuristic).
- Any claim that this matches Farmer's Fridge's actual internal SLA or
  staffing policy — the README must be explicit that criticality tiers and
  access-clearance rules here are a plausible model built from public
  information about *what kind of constraints airport locations impose in
  general*, not insider knowledge of their system.

## Suggested implementation touch points

```
internal/fleet/
  venue.go        NEW - venue type -> criticality tier, access-constrained
                  flag, peak-window definition
  dispatch.go     MODIFIED - priority scoring replaces round-robin as the
                  primary ordering; add eligible-tech filtering
  techs.go        NEW - small static tech roster + clearance model
cmd/fleet-server/
  (dashboard templates/static assets) MODIFIED - surface priority + blocked
  state in the alert table, venue tier on fridge cards
```

## Definition of done

- A high-tier (airport) alert opened after a low-tier (office) alert still
  gets assigned first, provable in a test.
- An alert for an access-constrained venue with no eligible tech available
  stays `open` with a distinct "blocked" reason, not silently stuck looking
  like a normal open alert.
- Peak-window weighting is covered by at least one test asserting the
  priority score changes based on simulated time.
- Dashboard visibly reflects priority/blocked state without a page reload
  redesign — extend existing table/cards, don't replace them.
- README section explicitly states the sourcing (Fast Casual airport
  expansion piece + general knowledge of airport access constraints) and
  is explicit that criticality tiers/clearance rules are an illustrative
  model, not insider knowledge of Farmer's Fridge's real policies.
