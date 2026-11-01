# ADR-003: Distributed transaction: an orchestrated saga, hand-rolled in Go

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** team constraint T1 (microservices + saga); AC-05, AC-06, AC-07, AC-10
* **Related:** [ADR-004 messaging](ADR-004-messaging-outbox-inbox.md), [ADR-006 service architecture](ADR-006-service-architecture.md)

## Context

Dispatching a ride changes three aggregates owned by three services: the **ride** (ride-service), the **driver** (driver-service) and the **offer** (dispatch-service). There is no shared database transaction. The flow is not a straight line. It has:

* a **loop:** reject or timeout leads to excluding the driver and matching again;
* a **timer:** a 30 s offer timeout;
* **compensations:** release the driver, unassign the ride;
* **races:** cancel at any time, accept vs. timeout.

## Part 1: consistency model

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Saga** (local transactions + compensations) | Non-blocking; each service autonomous; works with a broker | Eventually consistent; compensations must be designed | ✅ **Chosen** |
| Two-phase commit (XA) | Atomic across databases | Blocking; a coordinator failure leaves locks held; Kafka cannot take part | ❌ |

## Part 2: saga style

| | Choreography (services react to each other's events) | **Orchestration (one coordinator sends commands)** |
|---|---|---|
| Rematch loop, 30 s timer, exclusions | No natural owner; logic leaks into services | Natural in the orchestrator |
| "What happened to R1?" | Reconstruct from many streams | One row + `saga_steps` + one trace |
| Verdict | ❌ | ✅ **Chosen** |

## Part 3: implementation

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Hand-rolled orchestrator** (Postgres state machine + outbox + commands on Redpanda) | No extra infrastructure; every step visible in SQL and logs | We write retries, timers and resume ourselves | ✅ **Chosen** |
| Temporal (Go SDK) | Durable workflows as code; built-in timers, retries and history | Another server cluster to run; hides the mechanics the challenge asks us to explain | The choice when more workflows appear |

## Decision

**Orchestration**, with **dispatch-service** as the orchestrator. Participants (ride-service, driver-service) expose **commands** and send **replies**. They hold no dispatch logic, only their own state machines.

Saga design rules:

1. **Compensatable steps → pivot → retriable steps.** The pivot is *offer accepted*: a row change in the orchestrator's own database, so it is atomic. Before it, failures compensate (release driver, unassign ride). After it, steps are idempotent and retried until done.
2. **Each step is one local transaction**: lock the saga row, update it, write a `saga_steps` audit row, append commands to the outbox.
3. **Every command is idempotent** and carries `rideId` + `offerId`, so participants can detect stale ones.
4. **Semantic locks:** driver `OFFERED`/`BUSY` and ride `DRIVER_ASSIGNED` are the visible "in progress" markers that stop other sagas from using the same resources.
5. **Ride cancellation is owned by ride-service** (it owns the lifecycle). The saga reacts to `RideCancelled` and compensates; it never blocks a cancel.
6. **Timers and recovery:** the timeout sweeper and the retry/resume worker poll Postgres every second (`FOR UPDATE SKIP LOCKED`). A saga with no reply for 30 s re-sends its last command, which is safe because commands are idempotent.

| Step | Participant | Type | Compensation |
|---|---|---|---|
| `StartMatching` | ride | compensatable | cancel |
| Match in Redis | dispatch | read-only | — |
| `ReserveDriver` | driver | compensatable | `ReleaseDriver` |
| Offer `PENDING` + `AssignDriver` | dispatch + ride | compensatable | offer `REJECTED/EXPIRED/CANCELLED` + `UnassignDriver` |
| **Accept** | dispatch | **pivot** | — |
| `MarkDriverBusy` + `MarkAccepted` | driver + ride | retriable | — |
| Complete / cancel → `ReleaseDriver` | driver | retriable | — |

## Consequences

* ✅ All dispatch rules live in one service and are tested with simulated participants ([orchestrator_integration_test.go](../../services/dispatch/internal/usecase/orchestrator_integration_test.go)).
* ✅ One row, one audit table and one trace per ride ([observability](../observability.md)).
* ⚠️ Ride and driver state are **eventually consistent** with the offer: `accept` returns `200` at the pivot, and the ride shows `DRIVER_ACCEPTED` a few milliseconds later.

## When to revisit

* A second or third workflow appears (payments, scheduled rides): move to **Temporal**, keeping the participants' idempotent commands.
