# Requirements Traceability

Every requirement of [Coding Challenge #3](../../coding-challenge-3.md) is mapped to how it is met, where the code is, which automated tests prove it, and how to check it yourself. Nothing in the challenge is left unmapped. Gaps and partial answers are marked ⚠️ and explained.

**Contents:**
- [How to read this page](#how-to-read-this-page)
- [1. Acceptance criteria (AC-01 … AC-10)](#1-acceptance-criteria)
- [2. Additional evaluation requirements (§5)](#2-additional-evaluation-requirements-5)
- [3. Team constraints](#3-team-constraints)
- [4. Scope, assumptions and out of scope](#4-scope-assumptions-and-out-of-scope)
- [5. Evidence: latest test run](#5-evidence-latest-test-run)

## How to read this page

**Test legend.** Test names are Go test functions; Postman entries are folders in [postman/ridehail.postman_collection.json](../postman/ridehail.postman_collection.json).

| Tag | Suite | File |
|---|---|---|
| **U-ride** | unit: ride state machine | [services/ride/internal/domain/ride_test.go](../services/ride/internal/domain/ride_test.go) |
| **U-rideUC** | unit: ride use cases with in-memory fakes | [services/ride/internal/usecase/rides_test.go](../services/ride/internal/usecase/rides_test.go) |
| **U-driver** | unit: driver status rules | [services/driver/internal/domain/driver_test.go](../services/driver/internal/domain/driver_test.go) |
| **U-match** | unit: matching rules | [services/dispatch/internal/domain/match_test.go](../services/dispatch/internal/domain/match_test.go) |
| **U-geo** | unit: coordinates, Haversine | [pkg/geo/geo_test.go](../pkg/geo/geo_test.go) |
| **I-ride** | integration (real Postgres): ride HTTP + use cases | [services/ride/internal/adapter/http/http_integration_test.go](../services/ride/internal/adapter/http/http_integration_test.go) |
| **I-driver** | integration (real Postgres): driver repository + use cases | [services/driver/internal/adapter/postgres/postgres_integration_test.go](../services/driver/internal/adapter/postgres/postgres_integration_test.go) |
| **I-saga** | integration (real Postgres): saga orchestrator | [services/dispatch/internal/usecase/orchestrator_integration_test.go](../services/dispatch/internal/usecase/orchestrator_integration_test.go) |
| **I-geo** | integration (real Redis): GEO index | [services/dispatch/internal/adapter/redis/redis_integration_test.go](../services/dispatch/internal/adapter/redis/redis_integration_test.go) |
| **E** | end-to-end through the gateway against `make up` | [test/e2e/e2e_test.go](../test/e2e/e2e_test.go) |
| **P-HP**, **P-nn** | Postman folder: the happy-path scenario, or the folder for AC-nn (**P-02** covers AC-02 and AC-03, **P-05** covers AC-05 and AC-07, **P-05T** is the offer-timeout folder) | [postman/](../postman/) |

**Verify yourself:** with the stack running (`make up`). `make demo` walks through most of these scenarios with every request/response logged.

---

## 1. Acceptance criteria

### AC-01: Create Ride

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| `POST /rides` creates a ride | ride-service HTTP adapter → `CreateRide` use case | [adapter/http](../services/ride/internal/adapter/http/http.go), [usecase/rides.go](../services/ride/internal/usecase/rides.go) | U-rideUC `TestCreateRidePublishesRideRequested`; I-ride `TestCreateRideStartsRequestedAndCanBeRetrieved`; E `TestHappyPath`; P-HP, P-01 | `curl -X POST localhost:8080/rides -H 'Idempotency-Key: k1' -d '{…}'` |
| A unique `rideId` is generated | UUIDv7 (time-sortable, RFC 9562) | `messaging.NewID()` | I-ride `TestCreateRideStartsRequestedAndCanBeRetrieved` | response `rideId` |
| Ride starts in `REQUESTED` | `domain.NewRide` | [domain/ride.go](../services/ride/internal/domain/ride.go) | U-rideUC, I-ride (`starts in REQUESTED`), P-HP, P-01 | response `status` |
| Invalid coordinates rejected; latitude ∈ [-90, 90], longitude ∈ [-180, 180] | Validated in the HTTP adapter (missing fields) and in `domain.NewRide`/`geo.Point.Validate` (ranges); DB `CHECK` constraints as a last guard | [pkg/geo](../pkg/geo/geo.go), [migrations](../services/ride/internal/adapter/postgres/migrations/001_rides.sql) | U-geo `TestValidate` (bounds inclusive, ±0.0001 outside, NaN, missing); U-rideUC `TestCreateRideValidation`; I-ride `TestCreateValidation`; P-01 | `422 INVALID_COORDINATES` |
| `passengerId` required and must reference a seeded passenger | `domain.ErrPassengerRequired`; `PassengerRepository.Exists` | [usecase/rides.go](../services/ride/internal/usecase/rides.go) | U-rideUC `TestCreateRideValidation`; I-ride `TestCreateValidation` (missing, unknown); P-01 | `422 PASSENGER_REQUIRED` / `UNKNOWN_PASSENGER` |
| Clients must send `Idempotency-Key` | Missing or over 200 chars → `400` | HTTP adapter | I-ride `TestCreateValidation` (missing key); P-01 | `400 IDEMPOTENCY_KEY_REQUIRED` |
| Same key + same request returns the original result, no new ride | Key inserted **first, in the ride's transaction**, with request hash + response snapshot; a replay returns the snapshot with `Idempotent-Replayed: true` | `IdempotencyStore` port, [adapter/postgres](../services/ride/internal/adapter/postgres/postgres.go) | U-rideUC `TestCreateRideIdempotency`; I-ride `TestDuplicateRideCreationReturnsOriginal`, **`TestConcurrentDuplicateCreationCreatesOneRide`** (10 parallel); E `TestDuplicateRideCreation`; P-08 | repeat the curl: same `rideId` |
| Same key + different request rejected | SHA-256 of normalised fields differs → `422` | `usecase.RequestHash` | U-rideUC; I-ride `TestIdempotencyKeyReusedWithDifferentRequestIsRejected`; E; P-08 | `422 IDEMPOTENCY_KEY_REUSED` |
| Created ride can be retrieved | `GET /rides/{id}` | HTTP adapter | I-ride; E; P-HP | `curl localhost:8080/rides/<id>` |

### AC-02: Driver Availability

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Switch `OFFLINE ⇄ AVAILABLE` via `PUT /drivers/{id}/availability` | `SetStatusIf(from, to)`: one conditional UPDATE | [usecase/drivers.go](../services/driver/internal/usecase/drivers.go), [adapter/postgres](../services/driver/internal/adapter/postgres/postgres.go) | U-driver `TestAvailabilityToggle`; I-driver `TestDriverStatusLifecycle`; P-02 | `curl -X PUT …/drivers/D011/availability -d '{"status":"AVAILABLE"}'` |
| Only `AVAILABLE` drivers can receive offers | GEO index contains only AVAILABLE drivers; matcher re-checks status; reservation is `WHERE status='AVAILABLE'` | [domain/match.go](../services/dispatch/internal/domain/match.go), `ReserveIfAvailable` | U-driver `TestOnlyAvailableDriversCanBeOffered`; U-match `TestOfflineDriverExcluded`, `TestBusyAndOfferedDriversExcluded` | — |
| Offer: `AVAILABLE → OFFERED`; accept: `OFFERED → BUSY` | `ReserveDriver` / `MarkDriverBusy` commands | `ReserveIfAvailable`, `MarkBusyIfOffered` | U-driver `TestOfferMakesAvailableOffered`, `TestAcceptMakesBusyAndCompleteReturnsAvailable`; I-driver `TestDriverStatusLifecycle`; E `TestHappyPath`; P-HP | `GET /drivers/{id}` after each step |
| Reject or cancel before acceptance: `OFFERED → AVAILABLE` | `ReleaseDriver(rideId)` compensation | `ReleaseIfHeld` | U-driver `TestRejectOrCancelBeforeAcceptReturnsToAvailable`; I-driver `TestRejectBeforeAcceptReturnsAvailable`; I-saga `TestCancelReleasesTheOfferedDriver`; E `TestRejectRematchesToNextDriver`, `TestCancelReleasesOfferedDriver`; P-05, P-09 | |
| Completing an accepted ride: `BUSY → AVAILABLE` | `RideCompleted` → `ReleaseDriver` | saga `HandleRideEvent` | U-driver; I-saga `TestCompletionReleasesTheDriver`; E `TestHappyPath`; P-HP | |
| `OFFERED`/`BUSY` cannot receive another offer or go directly `OFFLINE` | Conditional updates match nothing → `409` / `DriverReservationFailed` | `SetStatusIf`, `ReserveIfAvailable` | U-driver `TestOfferedOrBusyCannotGoOffline`; I-driver `TestDriverStatusLifecycle`, `TestSecondReservationIsRejectedWithReply`; E `TestCancelReleasesOfferedDriver` | `409 INVALID_TRANSITION` |
| All other transitions rejected | Domain rules return `ErrInvalidTransition`/`ErrStale`; API only accepts `AVAILABLE`/`OFFLINE` | [domain/driver.go](../services/driver/internal/domain/driver.go) | U-driver `TestInvalidDriverTransitions`, `TestReleaseForAnotherRideIsIgnored`; P-02 (`BUSY` via API → 422) | |

### AC-03: Driver Location

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Location can be updated (`PUT /drivers/{id}/location`) | `UpdateLocation` use case; any status | [usecase/drivers.go](../services/driver/internal/usecase/drivers.go) | I-driver `TestEveryChangeEmitsVersionedEvent`; P-02 | `curl -X PUT …/location -d '{"latitude":…,"longitude":…}'` |
| Invalid coordinates rejected | `geo.PointInput.ToPoint` + `Validate` | [pkg/geo](../pkg/geo/geo.go) | U-geo `TestValidate`; E `TestInvalidRequests`; P-02 | `422 INVALID_COORDINATES` |
| Latest location is stored | `drivers.lat/lng` + `version+1`; `DriverLocationUpdated` event | [adapter/postgres](../services/driver/internal/adapter/postgres/postgres.go) | I-driver `TestEveryChangeEmitsVersionedEvent`; P-02 (response shows the latest) | `GET /drivers/{id}` |
| Matching uses the latest known location | Event → Redis GEO projection (Lua: newer `version` wins, so stale/duplicate events are ignored); matcher uses the exact coordinates | [usecase/projection.go](../services/dispatch/internal/usecase/projection.go), [adapter/redis](../services/dispatch/internal/adapter/redis/redis.go) | **I-geo `TestIndexKeepsOnlyLatestAvailableDrivers`** (moves, out-of-order events); P-10 (driver moves next to a waiting ride → gets it) | [sequence §7](sequence-diagram.md#7-driver-availability-and-location-feed-the-geo-index) |

### AC-04: Driver Matching

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Only available drivers are considered | GEO set holds AVAILABLE only; `Rank` drops any other status | [domain/match.go](../services/dispatch/internal/domain/match.go) | U-match `TestOfflineDriverExcluded`, `TestBusyAndOfferedDriversExcluded` | |
| Default max radius 2 km, configurable | `MATCH_RADIUS_M` (default `2000`) | [cmd/main.go](../services/dispatch/cmd/main.go) | U-match `TestDriverOutsideRadiusExcluded` (incl. a 5 km radius) | `MATCH_RADIUS_M=5000 make up` |
| Drivers outside the radius excluded | `GEOSEARCH BYRADIUS` + exact Haversine re-check | domain + redis adapter | U-match `TestDriverOutsideRadiusExcluded` (the spec's 0.8 / 1.5 / 3.2 km example); E `TestHappyPath` (3.2 km driver never chosen); P-HP, P-04 | |
| Drivers ordered by distance | `Rank` sorts by `(distance, driverId)` | domain | U-match `TestNearestDriverSelected` (asserts `[A, B]`) | |
| Nearest suitable driver selected; ties → smallest `driverId` | `SelectNearest` | domain | U-match `TestNearestDriverSelected`, **`TestEquidistantPicksSmallestDriverID`**; I-saga `TestNearestDriverGetsOfferAndAcceptMovesForward`; P-04 (tie → A) | `make demo` step 4 |
| No driver selected when none suitable | `SelectNearest` → `ok=false`; saga `WAITING_FOR_DRIVER`, ride stays `MATCHING` | domain + saga | U-match `TestNoDriverAvailable`; P-06 | |
| Haversine; invalid input rejected before matching | `geo.HaversineM` (R = 6371.0088 km); coordinates validated at the API | [pkg/geo](../pkg/geo/geo.go) | U-geo `TestHaversine` | design: [ADR-001](adr/ADR-001-matching.md) |

### AC-05: Ride Offer

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Offer sets ride `DRIVER_ASSIGNED`, stores `assignedDriverId`, driver `OFFERED` (pending, not accepted) | `ReserveDriver` → offer `PENDING` → `AssignDriver` | [usecase/orchestrator.go](../services/dispatch/internal/usecase/orchestrator.go) `onReserved` | U-ride `TestAssignStoresDriver`; I-saga `TestNearestDriverGetsOfferAndAcceptMovesForward`; E `TestHappyPath`; P-HP | `GET /drivers/{id}/offer` |
| Only the driver holding the current offer can accept/reject | `UPDATE offers … WHERE ride_id AND driver_id AND status='PENDING'` | `OfferRepository.RespondPending` | I-saga **`TestOnlyTheOfferedDriverCanRespond`**; P-05 (other driver → 404) | `404 OFFER_NOT_FOUND` |
| Accept: ride `DRIVER_ACCEPTED`, driver `BUSY` | Pivot → `MarkDriverBusy` + `MarkAccepted` | [usecase/offers.go](../services/dispatch/internal/usecase/offers.go) `Accept` | I-saga; E `TestHappyPath`; P-HP, P-08 | |
| Reject: ride `MATCHING`, `assignedDriverId` cleared, driver `AVAILABLE` | `endOffer`: exclusion + `ReleaseDriver` + `UnassignDriver` | `offers.go` | U-ride `TestRejectionReturnsToMatchingAndClearsDriver`; I-saga `TestRejectionRematchesAndExcludesTheDriverForever`; E `TestRejectRematchesToNextDriver`; P-05 | |
| A rejecting driver never gets that ride again; still eligible for other rides | `ride_exclusions(ride_id, driver_id)` PK; matcher skips exclusions | postgres adapter + `match` | I-saga (asserts the excluded driver is re-offered **another** ride); E; P-05 | `GET /rides/{id}/dispatch` → `exclusions` |
| Configurable timeout (default 30 s); expiry = rejection | `OFFER_TIMEOUT`; sweeper `ExpireIfDue` → same `endOffer` path | `offers.go` `SweepExpired` | I-saga `TestTimeoutIsTreatedAsRejection`; **E `TestOfferTimeoutRematches`** (real 30 s); P-05T | wait 30 s |
| Rejection → attempt another eligible driver | `Reject` → `Match` | `offers.go` | I-saga; E; P-05 | `make demo` step 5 |
| Cancel allowed in `REQUESTED`/`MATCHING`/`DRIVER_ASSIGNED` (and `DRIVER_ACCEPTED`); releases `OFFERED`/`BUSY` driver | Ride state machine + saga `cancel` compensation | ride domain, saga | U-ride `TestValidTransitions`; I-saga `TestCancelReleasesTheOfferedDriver`; E `TestCancelReleasesOfferedDriver`; P-06, P-09 | |
| Completion after acceptance | `POST /rides/{id}/complete` (`DRIVER_ACCEPTED → COMPLETED`) | ride domain | U-ride; E `TestHappyPath`; P-HP | |

### AC-06: Ride State Machine

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Lifecycle explicitly implemented | Table-driven pure `Apply(ride, action)` | [domain/ride.go](../services/ride/internal/domain/ride.go) | U-ride (all) | [architecture §7](architecture.md#7-state-machines) |
| Valid transitions succeed | — | — | U-ride `TestValidTransitions` (9 transitions) | |
| Invalid transitions rejected (e.g. `COMPLETED → DRIVER_ASSIGNED`) | `ErrInvalidTransition` → `409` / `CommandRejected` | — | U-ride **`TestInvalidTransitions`** (12 cases incl. the spec's example); U-rideUC `TestCommandOnTerminalRideIsRejectedWithReply`; E `TestInvalidRequests`; P-06 | `409 INVALID_TRANSITION` |
| Duplicate transitions handled safely | Same target + same data → no-op success, no event | — | U-ride `TestDuplicateTransitionsAreNoOps`; U-rideUC `TestDuplicateCommandIsAppliedAndRepliedOnce`; I-ride `TestCommandsAreIdempotentAndHistoryIsRecorded` | repeat cancel → `200` |
| State changes persisted, incl. clearing `assignedDriverId` | `UPDATE rides … version+1` + `ride_transitions` row, same transaction | [adapter/postgres](../services/ride/internal/adapter/postgres/postgres.go) | I-ride `TestCommandsAreIdempotentAndHistoryIsRecorded` (3 history rows); P-HP (history) | `GET /rides/{id}/history` |
| Timeout follows rejection | Same `endOffer` + `UnassignDriver` | `offers.go` | I-saga `TestTimeoutIsTreatedAsRejection`; E `TestOfferTimeoutRematches` | |
| `CANCELLED` and `COMPLETED` terminal; driver `AVAILABLE` after either | `Status.Terminal()`; release on both | ride domain, saga | U-ride `TestTerminalStates`; I-saga `TestCancelReleasesTheOfferedDriver`, `TestCompletionReleasesTheDriver`; P-HP, P-06 | |

### AC-07: Rematching

| Acceptance bullet | How it is met | Code | Tests | Verify yourself |
|---|---|---|---|---|
| Rejecting drivers excluded from all later attempts for that ride, even if available again; eligible for other rides | `ride_exclusions` (permanent per ride); `SelectNearest(…, excluded)` | saga + domain | U-match `TestExcludedDriverSkipped`; I-saga **`TestRejectionRematchesAndExcludesTheDriverForever`** (also after a retry, and other rides still get the driver) | `GET /rides/{id}/dispatch` |
| Another available driver can be selected | `Reject` → `Match` | `offers.go` | I-saga; E `TestRejectRematchesToNextDriver`; P-05 | |
| Same driver not repeatedly selected | Exclusions + per-round skips | saga | I-saga (counts `ReserveDriver` = 2 for 2 drivers) | |
| No eligible drivers → ride stays `MATCHING`, no `assignedDriverId` | `WAITING_FOR_DRIVER` saga state; ride untouched | saga | I-saga; E `TestRejectRematchesToNextDriver`; P-05, P-06 | |

### AC-08: Idempotency

| Acceptance bullet | How it is met | Code | Tests |
|---|---|---|---|
| Ride creation with `Idempotency-Key` | See AC-01 | ride use case + postgres | U-rideUC `TestCreateRideIdempotency`; I-ride `TestDuplicateRideCreationReturnsOriginal`, `TestConcurrentDuplicateCreationCreatesOneRide`; E `TestDuplicateRideCreation`; P-08 |
| `RideCreated ×3` must not create three rides | Saga PK = `rideId` + inbox on `eventId` | saga + `broker.Inbox` | I-saga **`TestDuplicateRideRequestedStartsOneSaga`** (same id ×3 + new id ×1 → 1 saga) |
| Driver acceptance (`DriverAccepted ×2` → valid state) | Conditional update; repeat returns the stored outcome, no new commands | `offers.go` `Accept` | I-saga `TestDuplicateAcceptIsIdempotent`; E **`TestDuplicateAcceptKeepsValidState`** (5 parallel); P-08 |
| Ride state transition | Idempotent state machine + inbox | ride domain + use case | U-ride `TestDuplicateTransitionsAreNoOps`; U-rideUC `TestDuplicateCommandIsAppliedAndRepliedOnce`; I-ride `TestCommandsAreIdempotentAndHistoryIsRecorded` |
| Every consumed command/event (all services) | Inbox row in the handler's transaction | [pkg/broker](../pkg/broker/outbox.go) | I-driver `TestDuplicateCommandIsProcessedOnce` |
| Strategy documented | — | — | [ADR-004](adr/ADR-004-messaging-outbox-inbox.md), [ADR-010 §2](adr/ADR-010-api-and-identifiers.md#part-2-idempotency-key-design-ac-01-ac-08), [architecture §11](architecture.md#11-consistency-concurrency-and-idempotency) |

### AC-09: Concurrent Driver Assignment

| Acceptance bullet | How it is met | Code | Tests |
|---|---|---|---|
| The race is identified | Read Redis → reserve: two sagas can pick the same driver | — | [ADR-002 "The races"](adr/ADR-002-concurrency.md), [sequence §4](sequence-diagram.md#4-two-rides-race-for-the-same-driver-ac-09) |
| Prevents one driver holding two offers/rides | **Atomic `UPDATE drivers … WHERE status='AVAILABLE'`** (row lock); defence in depth: partial unique indexes on pending offers; `CHECK` on drivers | `ReserveIfAvailable` ([adapter/postgres](../services/driver/internal/adapter/postgres/postgres.go)), [dispatch migration](../services/dispatch/internal/adapter/postgres/migrations/001_dispatch.sql) | I-driver **`TestConcurrentReservationOnlyOneWins`** (50 rounds, 2 goroutines behind a barrier, real Postgres) |
| Only one ride creates the offer; the other stays `MATCHING` or tries another driver | Loser gets `DriverReservationFailed` → round skip → next candidate or `WAITING_FOR_DRIVER` | saga `HandleReply` | I-driver `TestSecondReservationIsRejectedWithReply`; I-saga **`TestTwoRidesCannotOfferTheSameDriver`**; E **`TestConcurrentRidesCannotShareADriver`** (parallel HTTP; the loser gets the driver after the winner cancels); P-09; `make demo` step 9 |
| Related race: accept vs timeout | One conditional update on the offer wins | `RespondPending`, `ExpireIfDue` | I-saga `TestAcceptRacingTimeoutHasOneWinner` (20 rounds) |
| Strategy documented | — | — | [ADR-002](adr/ADR-002-concurrency.md) (compares 10 approaches) |

### AC-10: Event-Driven Dispatch

| Acceptance bullet | How it is met | Code | Tests / evidence |
|---|---|---|---|
| Ride Created → Dispatch Event → Driver Matching | `POST /rides` → outbox `RideRequested` → Redpanda → dispatch consumer → saga → `Match` | [usecase/rides.go](../services/ride/internal/usecase/rides.go), [pkg/broker](../pkg/broker/), [adapter/messaging](../services/dispatch/internal/adapter/messaging/messaging.go) | E `TestHappyPath` (201 returns `REQUESTED`, matching happens afterwards); U-rideUC `TestCreateRidePublishesRideRequested`; `make trace-logs` shows produce → consume → match |
| README explains why asynchronous | — | — | [README › Event-driven](../README.md#48-event-driven-dispatch), [ADR-004](adr/ADR-004-messaging-outbox-inbox.md) |
| …what happens if a message is delivered twice | Inbox + idempotent domain | — | same; tests in AC-08 |
| …what happens if processing fails | Rollback, no offset commit, 5 retries with backoff + jitter, DLQ, resumer | [pkg/broker/consumer.go](../pkg/broker/consumer.go) | same; [architecture §12](architecture.md#12-failure-modes-and-recovery); manual crash drill |

---

## 2. Additional evaluation requirements (§5)

### Testing & Validation: minimum coverage

| Required test | Covered by |
|---|---|
| Matching: nearest driver selected | U-match `TestNearestDriverSelected` |
| Matching: driver outside radius excluded | U-match `TestDriverOutsideRadiusExcluded` |
| Matching: offline driver excluded | U-match `TestOfflineDriverExcluded` |
| Matching: busy driver excluded | U-match `TestBusyAndOfferedDriversExcluded` |
| Matching: no driver available | U-match `TestNoDriverAvailable` |
| Driver status: `AVAILABLE → OFFERED` on offer | U-driver `TestOfferMakesAvailableOffered`; I-driver `TestDriverStatusLifecycle` |
| Driver status: reject/cancel before accept → `AVAILABLE` | U-driver `TestRejectOrCancelBeforeAcceptReturnsToAvailable`; I-driver `TestRejectBeforeAcceptReturnsAvailable` |
| Driver status: accept → `BUSY`; complete → `AVAILABLE` | U-driver `TestAcceptMakesBusyAndCompleteReturnsAvailable`; I-driver `TestDriverStatusLifecycle` |
| Driver status: invalid transitions rejected | U-driver `TestInvalidDriverTransitions`, `TestOfferedOrBusyCannotGoOffline` |
| State machine: valid / invalid / duplicate | U-ride `TestValidTransitions` / `TestInvalidTransitions` / `TestDuplicateTransitionsAreNoOps` |
| State machine: rejection → `MATCHING`, clears `assignedDriverId` | U-ride `TestRejectionReturnsToMatchingAndClearsDriver` |
| Rematching: rejection, second driver, rejected driver excluded | I-saga `TestRejectionRematchesAndExcludesTheDriverForever`; E `TestRejectRematchesToNextDriver` |
| Concurrency: two rides cannot offer/assign the same driver | I-driver `TestConcurrentReservationOnlyOneWins`; I-saga `TestTwoRidesCannotOfferTheSameDriver`; E `TestConcurrentRidesCannotShareADriver` |
| Idempotency: duplicate ride creation | I-ride `TestDuplicateRideCreationReturnsOriginal`, `TestConcurrentDuplicateCreationCreatesOneRide`; E |
| Idempotency: duplicate driver acceptance | I-saga `TestDuplicateAcceptIsIdempotent`; E `TestDuplicateAcceptKeepsValidState` |

The test strategy (levels, invariants, how to run) is in [testing.md](testing.md).

### System Architecture

| Requirement | Where |
|---|---|
| System architecture diagram | [architecture §2–§4](architecture.md#2-system-context-c4-level-1) (C4 context, containers, components), README diagram |
| Reasoning explained | [architecture §1, §3, §13](architecture.md#13-decisions-at-a-glance) + 12 ADRs, each comparing alternatives |

### Scalability & Performance Design

| Requirement | Where |
|---|---|
| Evolution 1,000 → 100,000 → 1,000,000 drivers | [scalability §2](scalability.md#2-evolution-path) |
| Topics: location storage, matching performance, DB scaling, broker scaling, dispatch worker scaling, caching, partitioning | [scalability §3](scalability.md#3-topic-by-topic) (one row per topic per scale) |
| ≥ 3 bottlenecks / ≥ 3 strategies / ≥ 2 trade-offs | **6 / 6 / 5** in [scalability §4–§6](scalability.md#4-bottlenecks) |

### Observability

| Requirement | Where |
|---|---|
| Trace a ride by `rideId`, `driverId`, `eventId` in logs | Every log line carries `ride_id`, `driver_id`, `event_id`, `causation_id`, `trace_id` ([observability §1](observability.md#log-format)) |
| "What happened to ride R001?" from the logs | `make trace-ride RIDE=…` / `make trace-logs TRACE=…`; Loki; Tempo (one trace per ride) ([observability](observability.md)) |

### Documentation & Communication

| Required | Status |
|---|---|
| `README.md` | ✅ [README](../README.md) |
| `docs/architecture.md` | ✅ |
| `docs/sequence-diagram.md` | ✅ |
| `docs/adr/ADR-001-matching.md` | ✅ |
| `docs/adr/ADR-002-concurrency.md` | ✅ |
| API contract (extra) | ✅ [api/openapi.yaml](../api/openapi.yaml) (REST, browsable at http://localhost:8080/docs/), [api/proto](../api/proto/) (gRPC v2) |
| README: how to run | ✅ [README › How to run](../README.md#2-how-to-run) |
| README: how to run tests | ✅ [README › How to test](../README.md#3-how-to-test) |
| README: architecture | ✅ [README › Architecture](../README.md#41-architecture) |
| README: matching strategy | ✅ [README › Matching](../README.md#45-matching-strategy) |
| README: concurrency strategy | ✅ [README › Concurrency](../README.md#46-concurrency-strategy) |
| README: idempotency strategy | ✅ [README › Idempotency](../README.md#47-idempotency-strategy) |
| README: event-driven approach | ✅ [README › Event-driven](../README.md#48-event-driven-dispatch) |
| README: scalability considerations | ✅ [README › Scalability](../README.md#410-scalability) |
| README: known limitations | ✅ [README › Documentation](../README.md#6-documentation) → [architecture §15](architecture.md#15-known-limitations) |

## 3. Team constraints

| # | Constraint | Met by |
|---|---|---|
| T1 | Microservices + saga | 3 services, DB per service ([ADR-006](adr/ADR-006-service-architecture.md)); orchestrated saga ([ADR-003](adr/ADR-003-saga-orchestration.md)) |
| T2 | Go | Go 1.27 ([ADR-007](adr/ADR-007-language-and-libraries.md)); Clean Architecture per service ([ADR-012](adr/ADR-012-clean-architecture.md)) |
| T3 | Observability | OpenTelemetry → Grafana LGTM; one trace per ride ([ADR-005](adr/ADR-005-observability.md), [observability.md](observability.md)) |
| T4 | Everything in Docker; Colima + lazydocker via brew, no Docker Desktop | `make up` ([local-development.md](local-development.md), [docker-compose.yml](../docker-compose.yml)) |
| T5 | Redis GEO + Kafka/Redpanda + PostgreSQL | [ADR-001](adr/ADR-001-matching.md), [ADR-008](adr/ADR-008-data-stores.md), [ADR-009](adr/ADR-009-message-broker.md) |
| T6 | Documents in `docs/` | this folder ([README › Documentation](../README.md#6-documentation)) |

## 4. Scope, assumptions and out of scope

**Assumptions**

1. Passengers `P001–P010` and drivers `D001–D020` are seeded (registration is out of scope). Tests and the demo create extra drivers through a fixture endpoint that the gateway does not expose.
2. "Driver receives an offer" = a `PENDING` offer readable via `GET /drivers/{id}/offer` (REST polling) or pushed on the gRPC v2 stream `OfferService/WatchOffers`. Mobile push notifications are out of scope.
3. A ride with no eligible driver stays `MATCHING`. Retrying it (every 10 s, and immediately when a driver becomes available) is an addition beyond the ACs.
4. `DRIVER_ACCEPTED → CANCELLED` is allowed, as the spec's cancellation diagram shows, and releases a `BUSY` driver.
5. Offer expiry uses the database clock, so clock skew between services does not matter.
6. "Accept" returns `200` at the pivot. Ride and driver statuses follow within milliseconds (eventual consistency between services).

**Out of scope** (challenge §7): payments, pricing, authentication, registration, full trip management, real-time push, multi-region, Kubernetes, ETA/traffic, ML, mobile apps, admin UI.

## 5. Evidence: latest test run

Run on 2026-11-01 (Colima, Apple Silicon), after adding gRPC v2, the OpenAPI contract test, golangci-lint and CI. The same suites run in GitHub Actions on every push ([testing › CI](testing.md#continuous-integration)).

| Suite | Command | Result |
|---|---|---|
| Lint (golangci-lint + OpenAPI) | `make lint` | 0 issues ✅ |
| Unit + architecture | `make test` | 32 tests in 6 packages ✅ |
| Integration (testcontainers: Postgres 18, Redis 8) | `make test-int` | 23 tests in 4 packages ✅ |
| End-to-end (gateway REST + gRPC + OpenAPI contract, incl. the real 30 s timeout) | `make test-e2e` | 12 tests ✅ (~38 s) |
| Postman / newman | `make postman` | 56 requests, 115 assertions, 0 failed ✅ |
| Same as CI (`OFFER_TIMEOUT=5s make up`) | e2e + Postman | 12 tests ✅ (~13 s), 115 assertions ✅ |
| Architecture rules | part of `make test` | domain pure; no infrastructure in inner rings; adapters independent ✅ |
