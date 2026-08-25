# Fridge Fleet Ops Backend

A demo project built for Farmer's Fridge's Lead Back-End Engineer (SE IV)
role. Full problem framing, evidence, and scope decisions are in
[SPEC.md](./SPEC.md) — this README is the "how it works and how to run it"
companion.

## The problem this solves

Farmer's Fridge's fleet grew roughly 10-15x in five years (~200 fridges in
2018 to 3,000+ across 20 states by Jan 2026). A 2020 engineering account
described the failure mode that comes with that growth directly: HQ losing
real-time visibility into per-fridge inventory/health, worked around by
manually Slacking drivers and techs. 2026 job postings still name the same
problem class — missed stockings, fridge downtime, data blind spots — as
live priorities. This project is a small, honest model of the fix: **one
place to see fleet state, and an automated (if simple) way to route the
alerts that used to be manual Slack messages.** See SPEC.md for the full
evidence trail and the honest caveat about what this project does and
doesn't claim about Farmer's Fridge's actual current systems.

## What's here

| Piece | Replaces | Package |
|---|---|---|
| Event ingestion | The missing "report state somewhere" step | `internal/fleet` (`ingest.go`) |
| Fleet + per-fridge status API | "No single place to see fleet state" | `internal/fleet` (`status.go`) |
| Alert dispatch (open → assigned → resolved) | "Manually Slack a driver" | `internal/fleet` (`dispatch.go`) |
| Persistence | The lost visibility itself | `internal/fleet` (`store.go`, SQLite) |
| Ops copilot summary | A fleet-wide, not per-fridge, view of what's going on | `internal/copilot` |
| Fridge-side vend logic (carried over from v1) | The critical property that a customer is never silently charged with no item | `internal/vend`, `internal/dispenser` |
| Fridge simulator harness | A stand-in for 3,000+ real fridges | `cmd/fridge-sim` |
| Payment gateway | Real card authorize/refund calls, not a fake success/fail coin flip | `cmd/fridge-sim/stripe_gateway.go` |

## Running it

```bash
# Terminal 1: start the fleet backend (SQLite file created at ./fleet.db)
go run ./cmd/fleet-server

# Terminal 2: spin up 10 simulated fridges running 20 transactions each,
# POSTing events to the server above
go run ./cmd/fridge-sim -fridges 10 -transactions 20
```

Then open **http://localhost:8080** for a live read/write dashboard. It's a
single static page (`cmd/fleet-server/dashboard.html`, embedded via
`go:embed`, no build step, no separate frontend project) laid out as a
sidebar-nav app — four separate pages instead of one long scroll, since at
real fleet scale a single page of everything stops being usable:

- **Dashboard** — fleet health stat cards (with a live rolling sparkline
  each), a structured ops copilot panel (one-line LLM headline, a total-
  events/fridges hero stat row, a bar chart of event counts by type, and a
  distinctly-bordered callout per charged-but-not-dispensed case), and a
  "Needs attention" panel showing the highest-priority open/blocked alerts
  with one-click Assign.
- **Fleet Map** — a real zoomable/pannable US map (Leaflet + CARTO dark
  tiles) of every simulated fridge at its real location, color-coded by
  status and pulsing for faulted.
- **Fridges** — at 2,329 real locations, a flat list is unusable, so this
  groups by state (collapsible, small states auto-expanded) with a filter
  bar (state, status, criticality-tier dropdowns + free-text search over
  id/city/venue name, all combining with AND) that narrows the grid and the
  map together — selecting a state zooms the map to fit it. Verified
  end-to-end against the full dataset (e.g. selecting "CT" shows exactly
  its 5 real fridges).
- **Alerts** — the full triage table (assign/resolve by clicking), sorted
  by priority, with a fridge-id search and a "show more" cap (150 at a
  time) since an unpaginated table hits the same at-scale problem as the
  fridge list did (1,000+ rows otherwise).

Visual design: a cool-graphite palette (not pure black, not a cream
default) with LED-style status dots as the signature element — small,
color-coded, glowing, pulsing for faulted, dim for offline — echoing how a
real vending machine actually signals state, used consistently on stat
cards, fridge cards, and the map. Typography splits by role: Space Grotesk
for headers, Inter for body copy, IBM Plex Mono for every data value
(fridge IDs, timestamps, priority scores, counts) — monospacing the data
specifically is what makes the Alerts table's columns actually align.

Leaflet (`cmd/fleet-server/leaflet.js`/`.css`, BSD-2-Clause) and all three
font families (`cmd/fleet-server/fonts/*.woff2`, SIL Open Font License) are
vendored locally rather than pulled from a CDN/Google Fonts at page-load
time, so the dashboard doesn't depend on a third party being up. The map
*tiles* still come from CARTO/OpenStreetMap over the network, since
there's no reasonable way to vendor world imagery.

`cmd/fridge-sim` pins each simulated fridge to one of Farmer's Fridge's own
**real, individual fridge locations** — 2,329 of them, with exact street
address, coordinates, and venue type (Airport/Healthcare/B&I/Office/
Education/etc.) — pulled directly from the JSON payload that powers
`farmersfridge.com/locations-map/` (their own Gatsby build's public
page-data endpoint; the same data any visitor's browser downloads to render
that page). Captured 2026-08-24, embedded at `cmd/fridge-sim/locations.json`.
This is a snapshot of public reporting, not a live feed of their current
fleet, and it plays no role in any vend/dispatch/alerting logic — it's
cosmetic context for the map view and the fridge detail modal. The dataset
spans 22 states/DC, close to the "20 states" figure in SPEC.md's evidence
section.

Each fridge ID maps to **exactly one** real location, deterministically
(a hash of the fridge ID, not a shuffle keyed by `-seed` or loop position)
— `fridge-001` gets the same real address every run, regardless of
`-seed`, `-fridges` count, or how many times the simulator's been re-run
against the same `fleet-server`. Run `-fridges 2329` to place one
simulated fridge at every real location in the dataset (confirmed working:
the fleet-status endpoint correctly reports all of them, across all 22
states, in well under a minute).

Every fridge is clickable — in the Fridges grid, the Needs Attention list,
the Alerts table, and map popups — opening a detail modal with its real
address/venue, current status/tier, open alerts, and recent event history
(`GET /fleet/fridges/{id}`, already existing from v2).

Or poke the read side directly:

```bash
curl http://localhost:8080/fleet/status               # fleet-wide view
curl http://localhost:8080/fleet/fridges/fridge-001    # per-fridge drill-down
curl "http://localhost:8080/fleet/alerts?status=open"  # open alerts
curl -X POST http://localhost:8080/fleet/alerts/1/assign
curl -X POST http://localhost:8080/fleet/alerts/1/resolve
curl http://localhost:8080/fleet/copilot/summary       # fleet-wide ops summary (structured)
```

Config is via environment variables: `FLEET_SERVER_ADDR` (default `:8080`),
`FLEET_DB_PATH` (default `fleet.db`), `ANTHROPIC_API_KEY` (optional — the
copilot endpoint falls back to a deterministic heuristic summary when unset,
carrying over v1's "no API key needed" fallback behavior).

`cmd/fridge-sim` reads its own `STRIPE_SECRET_KEY` (optional). When set, each
simulated vend's `Authorize`/`Refund` calls hit the real Stripe API in test
mode — a genuine `PaymentIntent` create+confirm and, on a successful vend
that later needs reversing, a genuine `Refund` — instead of an in-memory
coin flip. Card outcomes come from Stripe's own test payment method tokens
(`pm_card_visa` succeeds, `pm_card_chargeDeclined` reliably declines, picked
at the calibrated decline rate below), so success/failure is whatever
Stripe's test-mode processing actually returns, not something the simulator
asserts on its own. Unset, it falls back to the original in-memory
`simulatedPaymentGateway`, so the whole project still runs with zero
external accounts. Get a test key from the
[Stripe Dashboard](https://dashboard.stripe.com/test/apikeys) (test mode
toggle, top-right) — never commit it; export it in your shell instead.

### Realistic simulation (v4)

The simulator used to produce flat, uniform noise — a fixed $3.50 on every
vend, identical traffic at every hour, and arbitrary failure rates. That's
what made an earlier "Needs Review" list look suspiciously clean the moment
anyone looked closely. This addendum ties real menu/pricing data, venue-aware
traffic shape, and calibrated failure rates together so a run reads like a
plausible real fleet instead of synthetic noise:

- **Real menu, plausible pricing.** Item names and categories
  (`cmd/fridge-sim/menu.go`) are pulled directly from Farmer's Fridge's
  public menu page (farmersfridge.com/menu, captured 2026-08-25) — real
  names, not invented. Farmer's Fridge doesn't publish per-item pricing; a
  third-party menu-price tracker reports their overall range as roughly
  $2.00–$8.00, so each item is priced within a plausible band by category
  (Snack/Sides $2.00–$4.00; Salad/Bowl/Wrap/Breakfast/HighProtein
  $6.00–$8.00). **This is a plausible reconstruction, not their actual
  current pricing.** Each fridge's five slots get a random menu item at
  creation time, priced once and reused for every vend of that slot, so
  vend amounts vary realistically instead of a flat placeholder.
- **Venue-aware traffic shape.** `cmd/fridge-sim/traffic.go` applies an
  hour-of-day multiplier to vend-attempt frequency, keyed by each fridge's
  real venue type: Office/B&I get a pronounced late-morning/midday lunch
  peak and near-zero overnight traffic; Airport is flatter across the day
  but has a higher overall baseline (plus small bumps around typical
  early-morning/early-evening travel windows); Healthcare is closest to
  round-the-clock; Education follows the Office shape with an added
  weekday/weekend damping factor. This is a simple illustrative heuristic —
  **not a real demand model and not based on any of Farmer's Fridge's
  actual traffic data**, which isn't public. Verified: an Office-tagged
  fridge showed 8 of 14 events (57%) landing in the 11am–noon hour alone;
  an Airport-tagged fridge showed a flatter 1–5-events-per-hour spread
  across the day at more than double the total volume.
- **Calibrated failure rates** (`cmd/fridge-sim/rates.go`): payment decline
  ~6% (realistic card-present/card-not-present range, not a Farmer's
  Fridge-specific figure), refund-call failure ~0.8% (the rare
  double-failure that produces `refund_pending` — kept well below the
  decline rate so it reads as a genuine edge case), hardware-fault
  injection ~0.5% per vend attempt (tuned down from an earlier, higher
  rate that put ~88% of a full 2,329-fridge run into "faulted" — implausible
  at fleet scale, since any single fault permanently marks a fridge faulted
  for this demo's snapshot).

### Daemon mode (v5)

`fridge-sim` was originally a one-shot batch tool: it generated a fixed set
of "today so far" events, POSTed them, and exited. That meant a dashboard
left open would look correct right after a run and then decay into
**everything offline** a few minutes later, once every fridge aged past the
15-minute silence threshold — the exact failure mode this addendum fixes.

`-daemon` makes `fridge-sim` run indefinitely, continuously generating
events instead of one fixed batch:

```bash
go run ./cmd/fridge-sim -daemon -fridges 500 -transactions 40 -speed 60
```

- **Scheduler.** A single goroutine owns a min-heap (`cmd/fridge-sim/daemon.go`)
  keyed by each fridge's next scheduled vend time. It pops the earliest due
  fridge, dispatches that vend attempt to a bounded worker pool (`-workers`,
  default 16), and — once that worker reports back — draws the fridge's next
  interval from an exponential distribution parameterized by its
  venue-aware hourly rate (`eventRatePerHour` in `traffic.go`), so the same
  lunch-hour-peak/flatter-Airport-baseline shape from the batch-mode
  addendum above still emerges in a continuous stream. One goroutine owning
  the heap avoids any locking; the worker pool exists specifically so a
  burst of due events at a high `-speed` can't fire unbounded concurrent
  POSTs at the fleet server — SQLite serializes writes, so unbounded
  concurrency there produces lock-contention errors, not more realistic
  data. Verified with a 500-fridge run at `-speed 500`: no lock errors, no
  dropped events.
- **`-speed`** scales simulated time relative to wall-clock time (default
  `1.0`). At `-speed 60`, one simulated hour elapses in one real minute —
  useful for watching a whole day's traffic shape (an office fridge's lunch
  spike, an airport's travel-window bumps) play out in a few minutes instead
  of requiring the demo to run for a full real day. All traffic/failure-rate
  logic is defined in simulated time, so `-speed` doesn't require separately
  retuning anything from the batch-mode addenda above.
- **Shutdown.** Ctrl+C (SIGINT/SIGTERM) stops the scheduler from picking up
  new work, then waits for any already-dispatched vend attempts to finish
  before exiting — no partial/mid-write events.
- **Restart resets inventory.** On restart, every simulated fridge's
  in-memory slot inventory starts full again — the daemon doesn't reload
  prior state from the fleet server. This is a deliberate simplification
  (making the simulator's own state durable/resumable is real added
  complexity a demo doesn't need), not an oversight.
- Batch mode (no `-daemon`) is unchanged and remains the default; both modes
  share the same per-fridge setup and `fireVend` event-generation path.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Static ops dashboard (HTML/JS, calls the endpoints below) |
| `POST` | `/fleet/events` | A fridge reports a vend completion, restock alert, hardware fault, or door anomaly |
| `GET` | `/fleet/status` | Fleet-wide health: healthy / low-stock / faulted / offline counts and per-fridge summary |
| `GET` | `/fleet/fridges/{id}` | One fridge's recent events, open alerts, and current status |
| `GET` | `/fleet/alerts?status=` | List alerts, optionally filtered by `open`/`assigned`/`resolved` |
| `POST` | `/fleet/alerts/{id}/assign` | Assign one alert to the next tech eligible for its venue (round-robin within that eligible subset) |
| `POST` | `/fleet/alerts/{id}/resolve` | Move an alert to `resolved` |
| `POST` | `/fleet/alerts/assign-next` | Assign the single highest-priority open alert fleet-wide (see "Venue-aware dispatch" below) |
| `GET` | `/fleet/copilot/summary` | Structured fleet-wide summary (headline, event counts, action items) over recent events |

A fridge is marked **offline** at read time if it hasn't reported an event
in `fleet.OfflineThreshold` (15 minutes) — this isn't a stored status, since
"no events at all" isn't something a fridge can report about itself.

## The one correctness property that matters

Carried over from v1: a customer must never end up charged with no item and
no record of it. `internal/vend` classifies every vend attempt into
`success`, `failed_no_charge` (never charged), `refunded` (charged, no item,
refund confirmed), or `refund_pending` (charged, no item, refund itself
failed — needs a human). The fleet backend never derives this from LLM
output: `internal/copilot`'s `Summary.ActionItems` (every charged-but-not-
dispensed case) and `EventCounts`/`TotalEvents`/`FridgeCount` are computed
deterministically in Go from the event payloads. The LLM's only job is
`Summary.Headline` — one interpretive sentence — so the "did the copilot
correctly flag every charged-no-item case" guarantee holds even if the LLM
call fails, is skipped (no API key), or writes a bad headline; the headline
is framing, the structured fields are the correctness surface.

## Venue-aware dispatch (v3, `internal/fleet/venue.go` + `techs.go`)

The original v2 dispatch logic (`internal/fleet/dispatch.go`) assigned every
alert round-robin, with no awareness of *where* the fridge actually was.
That stopped being a reasonable simplification once you look at where
Farmer's Fridge has actually been expanding: they've grown into 20+ major
U.S. airports, and a Fast Casual piece (Jan 2026) describes CEO Luke
Saunders appearing at a press briefing at Reagan National alongside the HHS
Secretary and the U.S. Transportation Secretary — tied to federal
infrastructure-law funding specifically for airport concessions. New
airport locations kept opening through 2026 (Boston Logan in May, Houston
Bush in August). An airport fridge is a different operational problem than
an office-lobby one: **access is gated** (post-security locations need a
badged/cleared tech, not "whoever's nearest"), **traffic isn't flat**
(sharp peak windows where a stockout is far costlier), and **visibility is
higher** (a broken fridge in a federally-spotlighted expansion is a worse
look than the same failure in a hospital break room).

**Full evidence and design rationale: see [SPEC_V3.md](./SPEC_V3.md).**

**Honest caveat, stated plainly:** the criticality tiers below (which venue
types count as "high priority") and the access-clearance/roster model are
an **illustrative model**, built from general public knowledge of what kind
of constraints airport locations impose — not insider knowledge of Farmer's
Fridge's actual internal SLAs or technician staffing/certification policy.
None of that is public, and this project doesn't claim otherwise.

What's here:

- **Criticality tiers per venue type** (`internal/fleet/venue.go`):
  Airport/Healthcare = high, Office/B&I = medium, Education = lower. Each
  open alert's priority score is dominated by its venue's tier — the
  weight gaps (100/50/10) are wide enough that no combination of the
  smaller severity/age/peak factors can let a lower-tier alert outrank a
  higher-tier one (see the worked bound in `priorityScore`'s doc comment).
  A fridge's tier is shown on its dashboard card; every alert's live
  priority score is shown in the alerts table.
- **Access-constrained assignment** (`internal/fleet/techs.go`): a small
  static mock tech roster (`tech-alice`/`tech-bob`/`tech-carol` — not real
  staffing data) with per-venue clearances. Airport alerts can only go to a
  cleared tech. If none is available, the alert stays `open` with a
  visible `blockedReason` instead of silently sitting there indistinguishable
  from a normal open alert (dashboard shows a distinct "blocked" badge).
- **Peak-window weighting**: Airport-vertical alerts get a priority boost
  during two UTC hour windows approximating morning/evening travel peaks.
  This is a deliberately simple, single-timezone heuristic, not a real
  travel-demand model — the point is demonstrating the dispatch model
  treats *when* as well as *where*.
- **`POST /fleet/alerts/assign-next`**: scans all open alerts, and assigns
  the single highest-priority one that has an eligible tech, skipping (and
  marking blocked) any higher-priority candidates that don't. The existing
  per-alert `POST /fleet/alerts/{id}/assign` still exists for manual,
  human-driven assignment of a specific alert; `assign-next` is the new
  "let the priority model pick" path, and the dashboard's "Auto-assign
  highest priority" button drives it.

## Package layout

```
internal/
  dispenser/   hardware boundary + in-memory simulator (v1, unchanged)
  vend/        vend transaction state machine (v1; Machine now takes an
               EventPublisher instead of an in-process callback)
  copilot/     LLM-assisted ops summarizer, repointed at fleet-wide event
               batches; heuristic fallback when no API key is set
  fleet/       model.go, store.go (SQLite), ingest.go, status.go, dispatch.go,
               venue.go (v3: criticality tiers, priority scoring), techs.go
               (v3: mock tech roster + venue clearances)
cmd/
  fleet-server/  HTTP server entrypoint
  fridge-sim/    harness spinning up N simulated fridges
```

## Tests

Every package has table-driven tests, matching v1's standard:

```bash
go test ./...
```

`internal/vend` includes a dedicated test guarding the project's most
important invariant: a `refund_pending` outcome is never indistinguishable
from an ordinary failure in the published event.

## Explicitly out of scope (see SPEC.md for the full list and why)

- Real hardware drivers — still simulated.
- Auth / production security hardening — the dashboard and API are both
  unauthenticated, fine for a local demo, not for a real deployment.
- Sophisticated dispatch optimization — round-robin assignment is a
  deliberate stand-in for a real routing algorithm.
- Postgres / cloud deployment — SQLite is the demo store; Postgres is a
  documented upgrade path (swap the `fleet.Store` implementation), not
  built here given the timeline.
