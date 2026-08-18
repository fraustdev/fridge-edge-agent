# Fridge Fleet Ops Backend — Project Spec

## Why this project exists

I'm applying to Farmer's Fridge's Lead Back-End Engineer (SE IV) role. Rather
than build a generic vending-machine demo, I wanted to anchor this in a
problem the company has actually named — not something I'm guessing at.

## The problem, and the evidence for it

**Core problem shape:** a fast-growing fleet of edge devices (smart fridges),
one central ops team, and no single reliable source of truth for fleet state
— which shows up as downtime, missed restocks, and manual, ad hoc
coordination between HQ and the field.

Evidence this is real and current, not just an old anecdote:

- **Scale grew roughly 10-15x in five years.** Farmer's Fridge went from
  ~200 IoT-connected fridges (2018) to ~400 (2020) to 2,200+ (2025) to
  3,000+ locations across 20 states (Jan 2026), with 1,000+ more planned
  in 2026. Whatever coordination problems existed at 400 fridges don't get
  easier at 3,000+ — they get structurally worse.
- **A 2020 engineering account (Built In Chicago) described the failure
  mode directly:** as the fleet grew, HQ lost real-time visibility into
  per-fridge inventory/health. The workaround was manually Slacking
  drivers and service techs to tell them where to go, which the team's
  Senior IoT Engineering Manager described as significant overhead. The
  stated goal was giving everyone real-time access to stock levels in one
  place.
- **This same problem class is still named as a live priority in 2026
  job postings**, which is stronger evidence than the 2020 article alone:
  - A Feb 2026 Sr Regional Manager, Fulfillment posting lists "reduce
    missed stockings and minimize fridge downtime" and "partner on fleet
    reliability... and inventory accuracy" as core responsibilities —
    active 2026 KPIs, not history.
  - A March 2026 Data Engineer, Lead posting frames the mandate as
    reducing data silos, improving data reliability, and turning "blind
    spots" into visibility — i.e., the company is on record in 2026 saying
    it doesn't yet have full visibility into its own fleet/data.
- **No public evidence the 2020 fix fully resolved this.** I found no
  follow-up post claiming victory. I'm treating the 2020 article as
  historical color and the 2026 postings as the actual current signal.

**Honest framing for the README/interview:** this project is inspired by a
problem pattern Farmer's Fridge has publicly described across 2020-2026,
not a claim that their current system has this exact gap. I don't know
their current architecture. What I do know is that "many edge devices, one
ops team, no fleet-wide source of truth" is a real, named, current problem
in their world, and it's the kind of thing a Lead Back-End Engineer on
Fridge Tech would plausibly be asked to help fix.

## What's being carried over from v1

The v1 project (`fridge-edge-agent`) already modeled the fridge-side
critical path in Go:
- `internal/vend` — a state machine for one vend transaction: payment
  authorization, dispensing, and every recovery path when payment and
  dispensing disagree (refunded, refund_pending, failed_no_charge). Table-
  driven tests cover all branches.
- `internal/dispenser` — the hardware boundary as an interface, with an
  in-memory simulator (no physical hardware required).
- `internal/copilot` — an LLM-assisted incident summarizer, deliberately
  kept outside the transaction path (used for post-hoc analysis, not
  in-line decisioning), with a heuristic fallback when no API key is set.

These stay mostly as-is. They become "the fridge side" — one simulated
fridge's local logic. What's new in v2 is everything on the *fleet* side.

## v2 scope: what we're actually building

A backend service that many simulated fridges report events into, giving
ops the fleet-wide visibility and automated routing that the manual-Slack
workflow lacked.

### 1. Event ingestion
- HTTP endpoint(s) that a fridge (running the v1 vend/dispenser logic)
  POSTs to instead of calling an in-process callback:
  - vend completions (success/failure, which failure mode)
  - restock alerts (slot low/empty)
  - hardware faults (jam, timeout, sensor error)
  - door-sensor anomalies
- Each event carries a fridge ID, slot ID (where applicable), event type,
  timestamp, and payload.

### 2. Persistence
- Durable store of: per-fridge current state, full event history, open
  alerts and their status.
- Start with SQLite (real persistence, no infra overhead); Postgres is a
  documented upgrade path, not a requirement for the demo.

### 3. Fleet status API (read side)
- Fleet-wide view: which fridges are healthy / low-stock / faulted / offline.
- Per-fridge drill-down: recent events, current slot states, open alerts.
- This is the direct replacement for "no single place to see fleet state."

### 4. Dispatch / assignment logic
- The direct replacement for "manually Slack a driver": incoming alerts
  are triaged (type + severity), assigned to a tech/route, and tracked
  through a status lifecycle: `open → assigned → resolved`.
- Deliberately simple assignment logic (e.g. nearest available tech, or
  round-robin) — the point is the state machine and audit trail existing
  at all, not a sophisticated routing algorithm.

### 5. LLM ops copilot (carried over, repointed)
- Same idea as v1, but now summarizing fleet-wide patterns across many
  fridges and many alerts, not one fridge's transaction batch — a more
  honest use of "deliberate LLM-assisted development" at fleet scale.
- Stays outside any decision path that affects money or dispatch
  correctness — it's a reporting/summarization layer, not a controller.

### 6. Fridge simulator harness
- A small runner that spins up N simulated fridges (reusing v1's
  `dispenser.Simulator` + `vend.Machine`), drives semi-random transactions
  and occasional injected failures, and POSTs the resulting events to the
  fleet backend. This replaces the v1 CLI demo.

## Explicitly out of scope

- Real hardware drivers (still simulated — no hardware, no time to order any).
- Auth/production security hardening.
- A frontend/dashboard UI — API-only is the deliverable. A thin dashboard
  is a nice-to-have stretch goal, not a requirement.
- Sophisticated dispatch optimization (routing algorithms, geo-based
  assignment) — the assignment logic should be simple and clearly labeled
  as a stand-in for something more sophisticated in a real system.
- Postgres / cloud deployment — documented as a "next steps" upgrade path,
  not built for the demo, given the 11-day timeline.

## Suggested package layout

```
fridge-edge-agent/
  internal/
    dispenser/        (v1, unchanged)
    vend/              (v1, unchanged — but Machine now takes an
                        EventPublisher instead of a local callback)
    copilot/           (v1, repointed at fleet-wide event batches)
    fleet/
      model.go         fridge/alert/event types shared across fleet package
      store.go         persistence interface + SQLite implementation
      ingest.go        HTTP handlers for event ingestion
      status.go        HTTP handlers for fleet/per-fridge status reads
      dispatch.go       alert triage + assignment state machine
  cmd/
    fleet-server/      HTTP server entrypoint (fleet backend)
    fridge-sim/        harness spinning up N simulated fridges, posting
                       events to fleet-server
  README.md
  SPEC.md              (this file)
```

## Definition of done for the v2 demo

- `go run ./cmd/fleet-server` starts the backend on a local port.
- `go run ./cmd/fridge-sim` spins up N (e.g. 10) simulated fridges, runs a
  batch of transactions with a realistic mix of successes and injected
  failures, and POSTs events to the running fleet-server.
- Hitting the fleet status endpoint shows accurate, real-time fleet state
  reflecting what the simulator did.
- At least one alert flows all the way through open → assigned → resolved.
- The ops copilot endpoint returns a fleet-wide summary that correctly
  calls out any transaction where a customer may have been charged
  without receiving an item (carried over from v1's most important
  correctness property).
- All new logic has table-driven tests, matching the standard v1 set.
- README ties each piece back to the problem statement above.
