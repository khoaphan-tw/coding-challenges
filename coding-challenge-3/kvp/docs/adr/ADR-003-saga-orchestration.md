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

We need a way to keep the three services consistent and to answer "what happened to ride R001?".

## Decision drivers

* Consistency without a distributed lock or two-phase commit.
* The loop, timer and compensations must be explicit and testable.
* One place to look when debugging a ride.
* Runs locally in Docker, with nothing heavy just for workflow.
* Fits the 4–6 h timebox (the team accepted going over it as a learning goal).

## Part 1: consistency model

| Option | How | Pros | Cons | Verdict |
|---|---|---|---|---|
| Single database transaction (modular monolith) | One ACID transaction updates ride + driver + offer | Simplest, immediately consistent | Not microservices (team constraint T1) | ❌ (see [ADR-006](ADR-006-service-architecture.md)) |
| Two-phase commit (XA / Postgres `PREPARE TRANSACTION`) | Coordinator locks all participants, then commits | Atomic across databases | Blocking protocol; coordinator failure leaves locks held; Kafka does not take part; poor fit for microservices | ❌ |
| **Saga** (local transactions + compensations) | Each step commits locally; failures are undone by compensating steps | Non-blocking, each service autonomous, works with a broker | Eventually consistent; compensations must be designed | ✅ **Chosen** |
| Event sourcing + process manager | State = event log; a process manager reacts | Full audit, replay | Big paradigm shift; projections, snapshots, schema evolution | ❌ Too much for the scope |

## Part 2: saga style

| | Choreography (services react to each other's events) | **Orchestration (one coordinator sends commands)** |
|---|---|---|
| Where the flow lives | Spread over every service | One state machine (`dispatch_sagas`) |
| Rematch loop / 30 s timer / exclusions | Someone must own them, so they leak into services | Natural in the orchestrator |
| "What happened to R1?" | Reconstruct from many streams | One row + `saga_steps` + one trace |
| Coupling | Implicit (everyone knows event semantics) | Explicit (orchestrator knows participants; participants know nothing of the flow) |
| Risk | Cyclic event chains, hard-to-see flows | Orchestrator becomes a "god service" |
| Testing | Needs all services | Orchestrator testable with fake participants |
| Verdict | ❌ | ✅ **Chosen** |

## Part 3: implementation technology

| Option | Language / runtime | What you get | Cost for this project | Verdict |
|---|---|---|---|---|
| **Hand-rolled orchestrator** (Postgres state machine + outbox + Kafka commands) | Go, our own code | Full control, no extra infra, every step visible in SQL and logs | We write retries, timers and resumption ourselves (~600 lines) | ✅ **Chosen** |
| Temporal (Go SDK) | Temporal server + Postgres/Cassandra | Durable execution: workflows as plain code, built-in timers, retries, history UI | Extra server cluster (frontend, history, matching, worker) and UI; determinism rules for workflow code; hides the saga mechanics the challenge wants explained | Strong runner-up; the choice at product scale |
| Cadence (Uber) | Go/Java SDK | Temporal's predecessor, same model | Same as Temporal, smaller community today | ❌ |
| Camunda 8 / Zeebe | BPMN engine (Java), any-language workers | Visual BPMN models, business-friendly | JVM cluster + Elasticsearch; BPMN modelling overhead | ❌ Heavy for one flow |
| Netflix Conductor / Orkes | JSON-defined workflows, Java server | Polyglot workers, UI | JVM server, Redis/ES dependencies | ❌ |
| AWS Step Functions | Managed state machines (ASL JSON) | Serverless, managed retries/timers | Cloud-only; cannot run offline in Docker (LocalStack is a partial emulation) | ❌ Must run locally |
| Dapr Workflows | Dapr sidecar, Go SDK | Durable workflows + pub/sub abstraction | Sidecar per service; younger feature set | ❌ |
| Eventuate Tram Sagas | Java/Spring framework | Orchestrated sagas + transactional messaging | Java only | ❌ Go project |
| Axon Framework | Java, event sourcing | Sagas + CQRS + event store | Java only; event-sourcing mindset | ❌ |
| MassTransit / NServiceBus sagas | .NET | Mature saga state machines | .NET only | ❌ |

**Why hand-rolled:** one workflow, a 4–6 h timebox, and a challenge that scores *explaining* concurrency, idempotency and failure handling. A small, visible orchestrator on infrastructure we already run (Postgres + Redpanda) shows every mechanism explicitly. Temporal would remove most of this code; it is the first thing we would adopt if more workflows appear (payments, scheduled rides, driver onboarding).

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
* ⚠️ Ride and driver state are **eventually consistent** with the offer (milliseconds). `accept` returns `200` when the offer is accepted (the pivot), and the ride shows `DRIVER_ACCEPTED` a few ms later.
* ⚠️ The orchestrator is on the critical path. It scales horizontally by Kafka partition (`rideId`), and its workers use `SKIP LOCKED`.
* ⚠️ We own retry, timer and resume code that a workflow engine would provide.

## When to revisit

* A second or third workflow appears: move to **Temporal** (Go SDK). Keep the participants' idempotent commands; replace `dispatch_sagas` + sweeper + retrier with workflow code.
* Orchestrator throughput limits: partition orchestrators by city or area.
