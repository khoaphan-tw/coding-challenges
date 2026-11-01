# kvp - Coding Challenge #3: Ride-Hailing Dispatch Service

A ride-hailing **dispatch service**: passengers request rides, the system offers each ride to the **nearest available driver**, drivers accept or reject, and rejected or expired offers are **rematched**. A driver can never be given two rides at once, duplicates are harmless, and every ride can be traced end to end.

Built for [Coding Challenge #3](../coding-challenge-3.md) as **three Go microservices** coordinated by an **orchestrated saga** over **Redpanda**, with **PostgreSQL** as the source of truth, **Redis GEO** for nearby drivers, and **OpenTelemetry → Grafana** for logs, traces and metrics. Everything runs in Docker (Colima) with one command.

| | |
|---|---|
| **Requirements** | All 10 acceptance criteria and every §5 evaluation item are met and traced to code and tests → [requirements traceability](docs/requirements-traceability.md) |
| **Tests** | 32 unit + architecture, 23 integration (real Postgres/Redis), 8 end-to-end, 119 Postman requests / 247 assertions, all green → [testing](docs/testing.md) |
| **Design** | C4 architecture, 8 sequence flows, 13 ADRs that each compare alternatives → [docs](docs/README.md) |
| **Run it** | `make up && make demo` |

## Contents

1. [Team Members](#team-members)
2. [How to Run](#how-to-run)
3. [How to Test](#how-to-test)
4. [Approach](#approach): [architecture](#architecture) · [dispatch flow](#how-a-ride-is-dispatched) · [code structure](#code-structure) · [requirements coverage](#requirements-coverage) · [matching](#matching-strategy) · [concurrency](#concurrency-strategy) · [idempotency](#idempotency-strategy) · [event-driven](#event-driven-dispatch) · [observability](#observability) · [scalability](#scalability)
5. [Design Decisions](#design-decisions)
6. [Known Limitations](#known-limitations)
7. [Challenges Faced](#challenges-faced)
8. [What We Learned](#what-we-learned)
9. [With More Time, We Would...](#with-more-time-we-would)
10. [AI Tools Used](#ai-tools-used)
11. [Documentation](#documentation)

---

## Team Members

1. Khoa Phan (_Developer_): khoa.phan@thoughtworks.com
2. Duy Phuong Nguyen (_Developer_): phuong.nguyen@thoughtworks.com
3. Victor Nguyen (_Developer_): victor.nguyen@thoughtworks.com

---

## How to Run

### 1. Prerequisites (macOS, with Docker)

```bash
brew install colima docker docker-compose docker-buildx lazydocker go jq
colima start --cpu 4 --memory 8 --vm-type vz --vz-rosetta
```

Add `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]` to `~/.docker/config.json` so `docker compose` works. Step-by-step guide, Intel Macs and troubleshooting: [docs/local-development.md](docs/local-development.md).

### 2. Start

```bash
cd coding-challenge-3/kvp
make up          # build + start all 10 containers, waits until healthy (~30 s after the first build)
```

| Open | What |
|---|---|
| http://localhost:8080 | **API** (gateway) |
| http://localhost:3000 | Grafana → Dashboards → *Dispatch overview* (logs, traces, metrics) |
| http://localhost:8088 | Redpanda Console (topics, messages, consumer groups) |
| `lazydocker` | Containers, logs, restarts in the terminal |

Seeded data: passengers `P001–P010`; drivers `D001–D010` `AVAILABLE` ~220 m apart north of Ben Thanh (HCMC); `D011–D020` `OFFLINE`.

### 3. Try it

```bash
make demo        # walkthrough: request → reject → rematch → accept → complete, duplicates, a race
                 # every request and response is printed
```

Or by hand:

```bash
curl -s -X POST localhost:8080/rides -H 'Idempotency-Key: k1' \
  -d '{"passengerId":"P001","pickup":{"latitude":10.7769,"longitude":106.7009},"destination":{"latitude":10.8231,"longitude":106.6297}}'
curl -s localhost:8080/rides/<rideId>                         # → DRIVER_ASSIGNED to D001 (nearest)
curl -s -X POST localhost:8080/rides/<rideId>/offers/D001/accept
curl -s -X POST localhost:8080/rides/<rideId>/complete
make trace-ride RIDE=<rideId>                                  # what happened, from the logs
```

**Postman:** import [postman/ridehail.postman_collection.json](postman/ridehail.postman_collection.json): 12 scenario folders, one per requirement area ([how to use](docs/local-development.md#postman)).

Full API (12 endpoints with error codes): [architecture §6](docs/architecture.md#6-public-api).

### 4. Stop

```bash
make down        # stop, keep data      ·  make clean   stop and delete all data
colima stop      # free the VM's CPU and RAM
```

---

## How to Test

| Command | What runs | Needs | Time |
|---|---|---|---|
| `make test` | Unit tests (state machines, matching, validation, ride use cases with fakes) + **architecture rules** | Go | < 2 s |
| `make test-int` | + **integration** on real Postgres 18 / Redis 8 via testcontainers (concurrency race, idempotency, saga flows) | Colima | ~15 s |
| `make test-e2e` | **End-to-end** through the gateway, incl. the real 30 s offer timeout | `make up` | ~40 s |
| `make postman` | Postman collection via newman | `make up`, Node | ~45 s |

Highlights:

* **Concurrency (AC-09):** 50 rounds of two goroutines reserving the same driver against real Postgres; exactly one wins every time.
* **Idempotency (AC-08):** the same `Idempotency-Key` sent 10× in parallel creates one ride; 5 parallel accepts change state once.
* **Saga:** rematch, permanent exclusion, timeout, cancel/complete compensation, accept racing timeout.

Every test and the invariants they protect: [docs/testing.md](docs/testing.md). Which test proves which requirement: [docs/requirements-traceability.md](docs/requirements-traceability.md).

---

## Approach

The system is split along its three aggregates (**ride**, **driver**, **dispatch/offer**) into three services that never share a database. They cooperate through an **orchestrated saga**: dispatch-service sends commands and reacts to replies and events on Redpanda. Every write is a local Postgres transaction that also stores its outgoing messages (**transactional outbox**), and every consumer skips duplicates (**inbox**). Every race is decided by **one atomic SQL statement** in the service that owns the data. Redis only *suggests* candidates; Postgres *decides*.

### Architecture

```mermaid
flowchart LR
    C([Passenger / Driver]) -->|HTTP| GW[nginx gateway :8080]
    GW --> RS[ride-service]
    GW --> DS[driver-service]
    GW --> XS[dispatch-service<br/>saga orchestrator]
    RS --- RDB[(ride_db)]
    DS --- DDB[(driver_db)]
    XS --- XDB[(dispatch_db)]
    XS --- REDIS[(Redis GEO)]
    RS & DS & XS <-->|outbox → topics → inbox| RP{{Redpanda}}
    RS & DS & XS -.OTLP.-> LGTM[Grafana LGTM]
```

| Service | Owns | Requirements |
|---|---|---|
| **ride-service** | Rides, passengers, `Idempotency-Key`s, ride state machine, status history | AC-01, AC-06, AC-08 |
| **driver-service** | Driver status and location, driver state machine, the **atomic reservation** | AC-02, AC-03, AC-09 |
| **dispatch-service** | Saga, offers, exclusions, 30 s timeouts, matching, Redis GEO projection | AC-04, AC-05, AC-07, AC-10 |

Why microservices when a modular monolith is allowed: it was a team learning goal, and the split follows how the parts scale (location writes dominate at scale). The honest comparison is in [ADR-006](docs/adr/ADR-006-service-architecture.md). Full C4 views, message catalogue and data model: [docs/architecture.md](docs/architecture.md).

### How a ride is dispatched

```
1. POST /rides ──► ride REQUESTED ──(RideRequested)──► dispatch starts a saga
2. saga ──StartMatching──► ride MATCHING
3. saga picks the nearest eligible driver (Redis GEO + Haversine) ──ReserveDriver──► driver AVAILABLE→OFFERED (atomic)
4. saga creates a 30 s offer ──AssignDriver──► ride DRIVER_ASSIGNED
5a. driver accepts  ─► PIVOT ─MarkDriverBusy + MarkAccepted─► driver BUSY, ride DRIVER_ACCEPTED ─► complete ─► driver AVAILABLE
5b. driver rejects / 30 s timeout ─► driver excluded for this ride, released, ride back to MATCHING ─► step 3
5c. nobody eligible ─► ride stays MATCHING; retried every 10 s and as soon as a driver becomes available
 *  passenger cancels at any time ─► the saga compensates (cancels the offer, releases the driver)
```

Every flow and race as a sequence diagram: [docs/sequence-diagram.md](docs/sequence-diagram.md).

### Code structure

Each service follows **Clean Architecture**: dependencies point inward only, and `make test` fails if they don't ([ADR-013](docs/adr/ADR-013-clean-architecture.md)).

```
services/<ride|driver|dispatch>/
├── cmd/main.go           composition root: builds adapters, injects them into use cases
└── internal/
    ├── domain/           entities + business rules: state machines, matching (pure Go)
    ├── usecase/          application logic + ports.go (interfaces for DB, outbox, …)
    └── adapter/          http · messaging (Kafka) · postgres · redis · driverclient
pkg/                      shared: messaging contract, broker (outbox/inbox/consumer), pgutil, obs, geo, …
test/arch · test/e2e      architecture rules · black-box tests
```

Full layout: [architecture §14](docs/architecture.md#14-repository-layout).

### Requirements coverage

| AC | Requirement | How it is met | Key proof | Details |
|---|---|---|---|---|
| 01 | Create ride | `POST /rides` → `REQUESTED`, UUIDv7, coordinate/passenger validation, `Idempotency-Key` stored with the response | `TestConcurrentDuplicateCreationCreatesOneRide` | [→](docs/requirements-traceability.md#ac-01-create-ride) |
| 02 | Driver availability | `OFFLINE ⇄ AVAILABLE`; `OFFERED`/`BUSY` cannot go offline; every status change is a conditional update | `TestDriverStatusLifecycle` | [→](docs/requirements-traceability.md#ac-02-driver-availability) |
| 03 | Driver location | Stored with a version; versioned events keep the GEO index on the latest location | `TestIndexKeepsOnlyLatestAvailableDrivers` | [→](docs/requirements-traceability.md#ac-03-driver-location) |
| 04 | Matching | Nearest `AVAILABLE` within 2 km (configurable), Haversine, ties → smallest `driverId` | `TestEquidistantPicksSmallestDriverID` | [→](docs/requirements-traceability.md#ac-04-driver-matching) |
| 05 | Ride offer | Offer `PENDING` 30 s; only the offered driver can answer; timeout = rejection | `TestOnlyTheOfferedDriverCanRespond`, `TestOfferTimeoutRematches` | [→](docs/requirements-traceability.md#ac-05-ride-offer) |
| 06 | State machine | Pure, table-driven; invalid → `409`; duplicates are no-ops; history persisted | `TestInvalidTransitions` | [→](docs/requirements-traceability.md#ac-06-ride-state-machine) |
| 07 | Rematching | Permanent per-ride exclusions; next nearest; stays `MATCHING` if nobody left | `TestRejectionRematchesAndExcludesTheDriverForever` | [→](docs/requirements-traceability.md#ac-07-rematching) |
| 08 | Idempotency | Idempotency-Key, inbox, saga PK, idempotent state machines | `TestDuplicateRideRequestedStartsOneSaga`, `TestDuplicateAcceptKeepsValidState` | [→](docs/requirements-traceability.md#ac-08-idempotency) |
| 09 | Concurrent assignment | Atomic `UPDATE … WHERE status='AVAILABLE'` + partial unique indexes | `TestConcurrentReservationOnlyOneWins` (50 rounds) | [→](docs/requirements-traceability.md#ac-09-concurrent-driver-assignment) |
| 10 | Event-driven dispatch | Outbox → Redpanda → saga; retries, DLQ, resume | `TestHappyPath`, crash drill | [→](docs/requirements-traceability.md#ac-10-event-driven-dispatch) |

### Matching strategy

*Nearest `AVAILABLE` driver within `MATCH_RADIUS_M` (default 2000 m); equidistant drivers → smallest `driverId`.*

1. driver-service publishes every status and location change with a version. dispatch-service keeps a **Redis GEO set of AVAILABLE drivers**, updated by a Lua script that ignores older versions, so matching uses the **latest location**.
2. `GEOSEARCH … BYRADIUS … ASC` returns candidates. The pure function `SelectNearest` drops excluded and non-available drivers, computes **Haversine** distance, cuts at the radius and sorts by `(distance, driverId)`.
3. Redis only suggests. The pick is confirmed by driver-service's atomic reservation, so a stale index costs a retry, never a wrong assignment.

Alternatives (PostGIS, Tile38, Elasticsearch, batch assignment, ETA routing): [ADR-001](docs/adr/ADR-001-matching.md).

### Concurrency strategy

**The race:** two sagas read Redis at the same moment and both pick D001. "Read status, then write" would let both succeed.

**The guard:** the owner of the data decides in one statement; the number of updated rows is the answer.

```sql
UPDATE drivers SET status='OFFERED', current_ride_id=$ride, version=version+1
WHERE id=$driver AND status='AVAILABLE'      -- 1 row = reserved · 0 rows = another ride won
```

Postgres row locking serialises the racers. The loser gets `DriverReservationFailed` and tries the next driver, or waits. The same pattern decides every other race:

* accept vs reject vs timeout vs cancel: `UPDATE offers … WHERE status='PENDING'`
* a release only applies while the driver still holds that ride
* ride transitions: `SELECT … FOR UPDATE`, then the state machine
* lock order is always saga → offer, so there are no deadlocks

Defence in depth: partial unique indexes (at most one `PENDING` offer per driver and per ride) and a `CHECK` on drivers. Ten alternatives compared (mutex, optimistic version, SERIALIZABLE, Redlock, etcd, actors, …): [ADR-002](docs/adr/ADR-002-concurrency.md).

### Idempotency strategy

| What | How |
|---|---|
| `POST /rides` | The `Idempotency-Key` row is inserted **first, in the ride's transaction**, with a request hash and the response. Same key + same body → original `201` with `Idempotent-Replayed: true`. Same key + different body → `422 IDEMPOTENCY_KEY_REUSED`. A concurrent duplicate waits on the key, then replays |
| Driver acceptance | Conditional update on the offer; a repeat finds it already `ACCEPTED` by that driver → `200`, no new commands |
| Ride state transitions | A repeat (already in the target state with the same data) is a no-op success; an invalid one is `409` |
| Every consumed message | Inbox row `(consumer, event_id)` in the handler's transaction; duplicates are skipped and logged |
| `RideCreated ×3` | The saga's primary key is the ride id, so there is one saga whatever the number of deliveries |
| Driver commands | Carry `rideId`; a late `ReleaseDriver(R1)` never frees a driver now serving R2 |

### Event-driven dispatch

* **Why asynchronous:** `POST /rides` answers immediately; matching may take several offer rounds and minutes and must not hold a request open. Spikes queue in Redpanda, services can restart without losing work, and dispatch scales by partition.
* **How:** ride created → `RideRequested` (via the outbox) → Redpanda → dispatch consumer → saga → matching. Topics are keyed by `rideId`/`driverId`, so messages about one ride stay in order.
* **If a message is delivered twice:** delivery is at-least-once. The inbox skips the copy, and if it comes back under a new id, the domain guards (saga PK, state machines, `rideId` checks) make it a no-op.
* **If processing fails:** the transaction rolls back (no partial state, no outgoing messages) and the offset is not committed. The message is retried 5× with exponential backoff and jitter, then goes to `<topic>.dlq` with an `ERROR` log; replay with `make replay-dlq`. If a reply is lost, the saga resumer re-sends the last command after 30 s (safe: commands are idempotent). A crashed service catches up from Redpanda after restart (verified with a kill/restart drill).

Details: [ADR-004](docs/adr/ADR-004-messaging-outbox-inbox.md), [ADR-009](docs/adr/ADR-009-message-broker.md), [architecture §12](docs/architecture.md#12-failure-modes-and-recovery).

### Observability

* **Logs:** plain `key=value` lines. Every line of a ride's journey carries `ride_id`, `driver_id`, `event_id`, `causation_id` and the ride's `trace_id`. HTTP calls are logged with request and response bodies; every produced and consumed message is logged with its value.
* **Traces:** **one trace per ride**, from `POST /rides` through offers, rejections, timeouts and acceptance to completion, across all services and Redpanda.
* **Metrics:** offers by outcome, time to first offer, reservation conflicts, outbox backlog, consumer outcomes, HTTP latency, on a provisioned Grafana dashboard.

**"What happened to ride R001?"**

```bash
make trace-ride RIDE=<rideId>     # the ride's state changes from all services, in order + a Tempo link
make trace-logs TRACE=<traceId>   # every log line of the journey, incl. bodies and message values
```

Runbook (Loki and Tempo queries, dashboard, alerts): [docs/observability.md](docs/observability.md).

### Scalability

Location updates grow to **250,000 writes/s at 1M drivers**, while dispatch stays below ~1,000/s. Scaling is about the location stream and the geo index, not rides.

| Bottleneck | Strategy | Trade-off |
|---|---|---|
| **B1** location writes on one `driver_db` primary (~50–100k drivers) | **S1** location via a compacted topic + batched snapshots | **T1** microservices vs modular monolith |
| **B2** one Redis GEO key | **S2** GEO sharded by H3 cell + area-owned dispatchers | **T2** Redis projection (fast, slightly stale) vs fresh DB reads |
| **B3** hot zones: many rides chase the same drivers | **S5** batch matching per hot cell | **T3** greedy nearest vs global assignment |

Plus B4–B6, S3/S4/S6, T4/T5, the 1k → 100k → 1M evolution and the seven topics the challenge lists (location storage, matching, DB, broker, workers, caching, partitioning): [docs/scalability.md](docs/scalability.md).

---

## Design Decisions

### Why this tech stack?

| Area | Chosen | Why (short) | Alternatives compared in |
|---|---|---|---|
| Language | Go 1.27, standard library first | ~20 MB images, millisecond start-up, goroutines for consumer loops and workers, explicit code | [ADR-007](docs/adr/ADR-007-language-and-libraries.md) (vs Java/Spring, Kotlin, .NET, Node, Python, Rust; Gin/Echo/Fiber; GORM/sqlc) |
| Architecture | 3 microservices, DB per service | Team goal; split by how parts scale | [ADR-006](docs/adr/ADR-006-service-architecture.md) |
| Code structure | Clean Architecture, enforced by a test | Flows testable with fakes; atomic SQL in one adapter | [ADR-013](docs/adr/ADR-013-clean-architecture.md) |
| Consistency | Orchestrated saga, hand-rolled | Loop + timer + compensations need one owner; no workflow server | [ADR-003](docs/adr/ADR-003-saga-orchestration.md) (vs 2PC, choreography, Temporal, Camunda, …) |
| Messaging | Outbox + inbox, at-least-once | No lost or phantom events, no extra infrastructure | [ADR-004](docs/adr/ADR-004-messaging-outbox-inbox.md) |
| Broker | Redpanda | Kafka API in one light container | [ADR-009](docs/adr/ADR-009-message-broker.md) |
| Data | PostgreSQL 18 + Redis 8 | Conditional updates, `SKIP LOCKED`, partial unique indexes; in-memory geo search | [ADR-008](docs/adr/ADR-008-data-stores.md), [ADR-001](docs/adr/ADR-001-matching.md) |
| API | REST + RFC 9457, UUIDv7 | Matches the spec; standard errors; time-sortable ids | [ADR-011](docs/adr/ADR-011-api-and-identifiers.md) |
| Observability | OpenTelemetry → Grafana LGTM | Vendor-neutral; one container locally | [ADR-005](docs/adr/ADR-005-observability.md) |
| Runtime | Colima + Compose + lazydocker, nginx | No Docker Desktop; one-command start | [ADR-010](docs/adr/ADR-010-runtime-gateway-tooling.md) |
| Testing | Unit + fakes + testcontainers + e2e + Postman | AC-09 proven on a real database | [ADR-012](docs/adr/ADR-012-testing-strategy.md) |

### What trade-offs did we make?

| We chose | Over | Gained | Paid |
|---|---|---|---|
| Microservices + saga | Modular monolith | Independent scaling of the location path; practice with distributed patterns | Eventual consistency (ms); more infrastructure; well over the 4–6 h timebox |
| Redis GEO projection | Querying the driver database | Fast matching, no load on the reservation rows | Index may be stale; costs a retry, never a wrong assignment |
| Hand-rolled saga | Temporal | No workflow server; every mechanism visible | We own timers, retries and resume code |
| Clean Architecture | One package per service | Testable use cases, swappable adapters, enforced boundaries | ~15 ports, more files; transaction passed implicitly in `context` |
| Accept returns at the pivot | Waiting for ride + driver updates | Fast, honest response | The ride shows `DRIVER_ACCEPTED` a few ms later |

### What patterns did we use?

Saga orchestration with compensations and a pivot · transactional outbox · idempotent consumer (inbox) · idempotency key with stored response · explicit state machines · compare-and-set in SQL · semantic locks (`OFFERED`, `DRIVER_ASSIGNED`) · CQRS-style read model (GEO projection) · database per service · API gateway · Clean Architecture with ports, adapters and a composition root · unit of work via a transaction port · retry with backoff + jitter and a dead-letter queue · correlation/causation ids and one trace per long-running journey.

---

## Known Limitations

* **Accept is synchronous on the offer, eventual on the ride:** `200 ACCEPTED` is the pivot; ride and driver statuses follow within milliseconds.
* **No push to drivers:** drivers poll `GET /drivers/{id}/offer` (real-time delivery is out of scope).
* **Waiting rides wait until cancelled:** no max-wait auto-cancel.
* **One outbox relay per service instance:** per-key ordering assumes one instance; replicas need relay leader election or CDC.
* **Transient outages longer than the retry budget (~10 s) send messages to the DLQ**, which then need `make replay-dlq`.
* **No cleanup jobs** for inbox, published outbox rows or old idempotency keys.
* **Redis loss at runtime:** automatic rebuild only on dispatch start (otherwise `make rebuild-geo`); the Lua script is single-node, not cluster-safe as written.
* **Stage-1 storage:** each location update is a Postgres write; one Postgres server hosts all three databases.
* **No authentication**; fixture endpoints (`/internal/*`) are reachable only on service ports, not via the gateway.
* **Not load-tested;** alerts are documented but not wired.

---

## Challenges Faced

| Challenge | What happened | How we solved it |
|---|---|---|
| Docker without Docker Desktop | `docker compose` was an "unknown command"; testcontainers could not find the Colima socket | `cliPluginsExtraDirs` in `~/.docker/config.json`; `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` in the Makefile |
| A restarted service stopped consuming for ~45 s | With franz-go's `BlockRebalanceOnPoll`, `Close()` blocks unless `AllowRebalance()` comes first; the container was killed before leaving its consumer group | `AllowRebalance()` before `Close()`; 10 s session timeout |
| Gateway 404s after a rebuild | nginx resolves upstream names once at start-up | Upstreams as variables + Docker DNS resolver |
| Potential deadlock | The timeout sweeper locked offer → saga while accept locked saga → offer | One lock order everywhere (saga → offer) + `SKIP LOCKED` |
| One ride = many traces | Accept/reject calls, timeouts and retries each started a new trace | Store the creating request's `traceparent` with the ride; later steps continue it, with a span link |
| One transaction across Clean Architecture layers | Use cases must not see pgx, yet lock + state + history + outbox must commit together | A `TxManager.WithinTx(ctx, fn)` port; repositories and the outbox join the transaction in `ctx` |
| Hidden infrastructure in inner layers | Shared packages silently pulled the Kafka client and OTel SDK into use cases | Split pure contracts from infrastructure; an architecture test now fails on such leaks |
| Flaky e2e test | It acted when the driver was `OFFERED`, sometimes before the offer row existed | Wait for `DRIVER_ASSIGNED` (offer exists) first |

## What We Learned

* **The database is the best lock.** One conditional `UPDATE` and its row count beat any distributed lock for "only one may win", and they survive crashes.
* **Exactly-once is a property of the handler, not the broker.** At-least-once delivery + an inbox + idempotent state machines is simple and robust.
* **Sagas need a pivot.** Deciding what can still be undone (reserve, assign) and what only moves forward (after accept) made every failure case easy to reason about.
* **Long flows need deliberate observability.** Per-request tracing splits a minutes-long journey; one trace per ride and correlated logs fixed it.
* **Architecture rules are worth enforcing in code.** The dependency test found real leaks that code review missed.
* **A modular monolith would have been enough** for 1,000 drivers; microservices earn their cost only on the location path at 100k+ drivers.

## With More Time, We Would...

* Move the saga to **Temporal** if more workflows appear (payments, scheduled rides).
* Build the **location fast path** and **GEO sharding by H3 cell** (S1, S2).
* Replace outbox polling with **CDC**; add relay leader election for replicas.
* Add **cleanup jobs**, a max-wait auto-cancel, and **push** offers (SSE/WebSocket).
* **Batch matching** in hot zones; **k6 load tests**; wired **alerts**; fake-based use-case tests for driver and dispatch too.
* Follow **TDD** with a visible red → green → refactor history.

## AI Tools Used

* **An AI coding assistant** in VS Code:
  * **Design:** a system-design workflow (requirements → estimates → architecture → trade-offs → failure modes); drafted the ADRs and comparison tables. We set the constraints (microservices + saga, Go, Redis GEO + Redpanda + Postgres, Colima) and reviewed the decisions.
  * **Implementation:** generated most of the Go code, SQL, Docker/Compose files, scripts and the Postman collection, iterating against the running stack; later refactored the services to Clean Architecture.
  * **Testing and debugging:** wrote the unit, integration and e2e tests; found and fixed the consumer-group shutdown bug, the gateway DNS issue, a lock-ordering risk and a flaky test by running the system and reading its logs and traces.
  * **Documentation:** this README and `docs/`.
* **What we checked ourselves:** requirement coverage against the ACs, the trade-offs, and the final review of code and docs. Every claim in the docs (tests, recovery, traces) was verified by running it.

---

## Documentation

**Start here:** [docs/README.md](docs/README.md) has reading paths for reviewers, developers and on-call engineers.

| Document | Answers |
|---|---|
| [requirements-traceability.md](docs/requirements-traceability.md) | Is every requirement met, where, and how is it proven? |
| [architecture.md](docs/architecture.md) | How is it built? (C4 views, API, state machines, saga, messages, data model, failures, decisions) |
| [sequence-diagram.md](docs/sequence-diagram.md) | What happens step by step, including races and crashes? |
| [testing.md](docs/testing.md) | How is it tested, and which invariants do the tests protect? |
| [scalability.md](docs/scalability.md) | How does it grow from 1k to 1M drivers? |
| [observability.md](docs/observability.md) | What happened to ride R001? |
| [local-development.md](docs/local-development.md) | How do I install, run, demo and test it? |
| [adr/](docs/adr/README.md) | Why each decision, compared with the alternatives (13 ADRs) |
