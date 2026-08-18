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

## Running it

```bash
# Terminal 1: start the fleet backend (SQLite file created at ./fleet.db)
go run ./cmd/fleet-server

# Terminal 2: spin up 10 simulated fridges running 20 transactions each,
# POSTing events to the server above
go run ./cmd/fridge-sim -fridges 10 -transactions 20
```

Then poke the read side:

```bash
curl http://localhost:8080/fleet/status               # fleet-wide view
curl http://localhost:8080/fleet/fridges/fridge-001    # per-fridge drill-down
curl "http://localhost:8080/fleet/alerts?status=open"  # open alerts
curl -X POST http://localhost:8080/fleet/alerts/1/assign
curl -X POST http://localhost:8080/fleet/alerts/1/resolve
curl http://localhost:8080/fleet/copilot/summary       # fleet-wide ops narrative
```

Config is via environment variables: `FLEET_SERVER_ADDR` (default `:8080`),
`FLEET_DB_PATH` (default `fleet.db`), `ANTHROPIC_API_KEY` (optional — the
copilot endpoint falls back to a deterministic heuristic summary when unset,
carrying over v1's "no API key needed" fallback behavior).

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/fleet/events` | A fridge reports a vend completion, restock alert, hardware fault, or door anomaly |
| `GET` | `/fleet/status` | Fleet-wide health: healthy / low-stock / faulted / offline counts and per-fridge summary |
| `GET` | `/fleet/fridges/{id}` | One fridge's recent events, open alerts, and current status |
| `GET` | `/fleet/alerts?status=` | List alerts, optionally filtered by `open`/`assigned`/`resolved` |
| `POST` | `/fleet/alerts/{id}/assign` | Move an alert to `assigned`, round-robin over a fixed tech list |
| `POST` | `/fleet/alerts/{id}/resolve` | Move an alert to `resolved` |
| `GET` | `/fleet/copilot/summary` | Fleet-wide LLM (or heuristic) ops narrative over recent events |

A fridge is marked **offline** at read time if it hasn't reported an event
in `fleet.OfflineThreshold` (15 minutes) — this isn't a stored status, since
"no events at all" isn't something a fridge can report about itself.

## The one correctness property that matters

Carried over from v1: a customer must never end up charged with no item and
no record of it. `internal/vend` classifies every vend attempt into
`success`, `failed_no_charge` (never charged), `refunded` (charged, no item,
refund confirmed), or `refund_pending` (charged, no item, refund itself
failed — needs a human). The fleet backend never derives this from LLM
output: `internal/copilot`'s `Report.ChargedNoItemCount` /
`ChargedNoItemDetails` are computed deterministically in Go from the event
payloads, so the "did the copilot summary correctly flag every
charged-no-item case" guarantee holds even if the LLM call fails, is
skipped (no API key), or writes a bad narrative — the narrative is framing,
the structured fields are the correctness surface.

## Package layout

```
internal/
  dispenser/   hardware boundary + in-memory simulator (v1, unchanged)
  vend/        vend transaction state machine (v1; Machine now takes an
               EventPublisher instead of an in-process callback)
  copilot/     LLM-assisted ops summarizer, repointed at fleet-wide event
               batches; heuristic fallback when no API key is set
  fleet/       model.go, store.go (SQLite), ingest.go, status.go, dispatch.go
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
- Auth / production security hardening.
- A dashboard UI — this is API-only; a thin dashboard is a stretch goal.
- Sophisticated dispatch optimization — round-robin assignment is a
  deliberate stand-in for a real routing algorithm.
- Postgres / cloud deployment — SQLite is the demo store; Postgres is a
  documented upgrade path (swap the `fleet.Store` implementation), not
  built here given the timeline.
