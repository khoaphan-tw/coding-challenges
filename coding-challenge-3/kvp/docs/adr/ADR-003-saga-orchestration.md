# ADR-003: Orchestrated saga for dispatch

* **Status:** Accepted
* **Date:** 2026-11-01
* **Context:** team constraint T1 (microservices + saga); AC-05, AC-06, AC-07, AC-10

## Context

Dispatching a ride changes three aggregates owned by three services: the ride (ride-service), the driver (driver-service) and the offer (dispatch-service). There is no shared database transaction and we will not use 2PC. A saga makes the multi-step change reliable: a sequence of local transactions, each with a compensation or a guaranteed forward retry.

## Options

| | Choreography (services react to each other's events) | **Orchestration (one coordinator sends commands)** |
|---|---|---|
| Rematch loop, 30 s timer, exclusions | Spread across services; nobody owns "the ride's dispatch" | One state machine in `dispatch_sagas` |
| Answering "what happened to R1?" | Rebuild from many event streams | One row + `saga_steps` |
| Coupling | Low between services, but implicit | Participants know nothing of the flow; orchestrator knows all participants |
| Risk | Cyclic event dependencies | Orchestrator becomes a "god service" |

## Decision

**Orchestration**, with dispatch-service as the orchestrator. Participants (ride-service, driver-service) expose **commands** and send **replies**. They contain no dispatch logic, only their own state machines.

Saga design rules:

1. **Compensatable steps first, pivot, then retriable steps.** The pivot is *offer accepted* (a row change in the orchestrator's own DB, so it is atomic). Before it, any failure compensates (release driver, unassign ride). After it, steps are idempotent and retried until done.
2. **Every saga step is one local transaction** that updates `dispatch_sagas` (with `version` check), writes a `saga_steps` audit row and appends commands to the outbox.
3. **Every command is idempotent** and carries `rideId` + `offerId` so participants can detect stale ones.
4. **Semantic locks:** driver `OFFERED`/`BUSY` and ride `DRIVER_ASSIGNED` are the visible "in-progress" markers that stop other sagas from using the same resources.
5. **Ride cancellation is owned by ride-service** (it owns the lifecycle). The saga reacts to `RideCancelled` and compensates. It never blocks a cancel.
6. **Recovery:** on startup, and every 10 s, a resumer re-drives sagas stuck in a non-terminal step longer than 30 s (re-sends the last command; dedup makes this safe).

## Consequences

* ✅ All dispatch rules (radius, exclusions, timeout, retry) live in one service and are unit-testable with fake participants.
* ✅ Clear story for "what happens if processing fails" (see [architecture §8](../architecture.md#8-failure-modes--degradation)).
* ⚠️ Ride and driver state are **eventually consistent** with the offer, for milliseconds. The API documents that accept returns when the offer is accepted.
* ⚠️ The orchestrator is on the critical path. It scales horizontally because Kafka partitions by `rideId` spread sagas across replicas, and the sweeper uses `SKIP LOCKED`.
* 🔁 Revisit with a workflow engine (Temporal) if more flows appear (payments, scheduled rides). Hand-rolled sagas do not scale to many workflows.
