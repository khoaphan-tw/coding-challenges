# Testing

How the system is tested, what each level proves, and the invariants the tests protect. The per-requirement map (each acceptance bullet → test) is in [requirements-traceability.md](requirements-traceability.md). Why this strategy over the alternatives is in [ADR-011](adr/ADR-011-testing-strategy.md).

## 1. Principles

1. **Business rules are pure functions**, so they get fast, exhaustive unit tests (valid / invalid / duplicate / terminal).
2. **Concurrency and SQL are proven against the real database**, never a mock. A mock cannot show that `UPDATE … WHERE status='AVAILABLE'` serialises two transactions.
3. **Use cases are tested through their ports**: with in-memory fakes (milliseconds), or wired to the real adapters exactly as `cmd/main.go` does.
4. **The whole system is tested as a black box** through the gateway, including the real 30 s timeout.
5. **Every test owns its data:** random ids, and in e2e/Postman a random spot on the map, so tests run in any order, repeatedly, alongside the demo.
6. **No sleeps for correctness:** asynchronous results are awaited by polling until a state is reached.

## 2. Test levels

| Level | What it proves | Tool | Count | Command | Needs |
|---|---|---|---|---|---|
| Unit: domain | State machines, matching, validation | `go test` | 23 | `make test` | Go |
| Unit: use cases | Ride flows via fakes of the ports: idempotency, duplicate commands, rejection replies | `go test` | 5 | `make test` | Go |
| Architecture | Clean Architecture dependency rule | `go test` + `go list` | 4 | `make test` | Go |
| Integration | Real Postgres 18 / Redis 8 (testcontainers): atomic reservation, idempotent create, saga flows, GEO versioning | `go test -tags integration` | 23 | `make test-int` | Colima |
| End-to-end | Whole stack through nginx (REST and gRPC), Redpanda, Redis, Postgres | `go test -tags e2e` | 11 | `make test-e2e` | `make up` |
| Scenario / exploratory | 12 readable scenario folders with assertions | Postman / newman | 119 requests, 247 assertions | `make postman` | `make up`, Node |
| Demo | Logged walkthrough (every request/response) | bash + curl | — | `make demo` | `make up` |

| Package | Level | Tests |
|---|---|---|
| `services/ride/internal/domain` | unit | `TestValidTransitions`, `TestAssignStoresDriver`, `TestRejectionReturnsToMatchingAndClearsDriver`, `TestInvalidTransitions`, `TestDuplicateTransitionsAreNoOps`, `TestTerminalStates` |
| `services/ride/internal/usecase` | unit (fakes) | `TestCreateRidePublishesRideRequested`, `TestCreateRideIdempotency`, `TestCreateRideValidation`, `TestDuplicateCommandIsAppliedAndRepliedOnce`, `TestCommandOnTerminalRideIsRejectedWithReply` |
| `services/driver/internal/domain` | unit | `TestAvailabilityToggle`, `TestOfferedOrBusyCannotGoOffline`, `TestOfferMakesAvailableOffered`, `TestOnlyAvailableDriversCanBeOffered`, `TestRejectOrCancelBeforeAcceptReturnsToAvailable`, `TestAcceptMakesBusyAndCompleteReturnsAvailable`, `TestInvalidDriverTransitions`, `TestReleaseForAnotherRideIsIgnored` |
| `services/dispatch/internal/domain` | unit | `TestNearestDriverSelected`, `TestDriverOutsideRadiusExcluded`, `TestOfflineDriverExcluded`, `TestBusyAndOfferedDriversExcluded`, `TestNoDriverAvailable`, `TestEquidistantPicksSmallestDriverID`, `TestExcludedDriverSkipped` |
| `pkg/geo` | unit | `TestValidate`, `TestHaversine` |
| `test/arch` | architecture | `TestDomainIsPure`, `TestInnerLayersDoNotReachInfrastructure`, `TestMessagingContractIsPure`, `TestAdaptersDoNotDependOnEachOther` |
| `services/ride/internal/adapter/http` | integration | `TestCreateRideStartsRequestedAndCanBeRetrieved`, `TestDuplicateRideCreationReturnsOriginal`, `TestConcurrentDuplicateCreationCreatesOneRide`, `TestIdempotencyKeyReusedWithDifferentRequestIsRejected`, `TestCreateValidation`, `TestCommandsAreIdempotentAndHistoryIsRecorded` |
| `services/driver/internal/adapter/postgres` | integration | `TestConcurrentReservationOnlyOneWins`, `TestSecondReservationIsRejectedWithReply`, `TestDriverStatusLifecycle`, `TestRejectBeforeAcceptReturnsAvailable`, `TestEveryChangeEmitsVersionedEvent`, `TestDuplicateCommandIsProcessedOnce` |
| `services/dispatch/internal/usecase` | integration | `TestNearestDriverGetsOfferAndAcceptMovesForward`, `TestRejectionRematchesAndExcludesTheDriverForever`, `TestTimeoutIsTreatedAsRejection`, `TestDuplicateRideRequestedStartsOneSaga`, `TestDuplicateAcceptIsIdempotent`, `TestOnlyTheOfferedDriverCanRespond`, `TestAcceptRacingTimeoutHasOneWinner`, `TestTwoRidesCannotOfferTheSameDriver`, `TestCancelReleasesTheOfferedDriver`, `TestCompletionReleasesTheDriver` |
| `services/dispatch/internal/adapter/redis` | integration | `TestIndexKeepsOnlyLatestAvailableDrivers` |
| `test/e2e` | end-to-end | `TestHappyPath`, `TestRejectRematchesToNextDriver`, `TestDuplicateRideCreation`, `TestDuplicateAcceptKeepsValidState`, `TestConcurrentRidesCannotShareADriver`, `TestCancelReleasesOfferedDriver`, `TestInvalidRequests`, `TestOfferTimeoutRematches`; gRPC v2: `TestGRPCStreamLocationsUpdatesDriver`, `TestGRPCWatchOffersPushesOfferAndClose`, `TestGRPCWatchOffersRequiresDriverID` |

## 3. Invariants and the tests that protect them

| # | Invariant | Protected by |
|---|---|---|
| I1 | A driver holds at most one pending offer or accepted ride | `TestConcurrentReservationOnlyOneWins` (50 rounds, 2 goroutines, barrier), `TestTwoRidesCannotOfferTheSameDriver`, e2e `TestConcurrentRidesCannotShareADriver`; DB: partial unique indexes, `CHECK` on drivers |
| I2 | An offer ends exactly once (accept / reject / expire / cancel) | `TestAcceptRacingTimeoutHasOneWinner` (20 rounds), `TestOnlyTheOfferedDriverCanRespond` |
| I3 | One `Idempotency-Key` → exactly one ride | `TestConcurrentDuplicateCreationCreatesOneRide` (10 parallel), `TestCreateRideIdempotency` |
| I4 | One ride → exactly one saga, whatever the number of `RideRequested` deliveries | `TestDuplicateRideRequestedStartsOneSaga` |
| I5 | A redelivered command changes nothing and is answered once | `TestDuplicateCommandIsProcessedOnce`, `TestDuplicateCommandIsAppliedAndRepliedOnce`, `TestCommandsAreIdempotentAndHistoryIsRecorded` |
| I6 | An excluded driver never gets that ride again, but can serve others | `TestRejectionRematchesAndExcludesTheDriverForever`, `TestExcludedDriverSkipped` |
| I7 | A stale command never frees a driver who moved on | `TestReleaseForAnotherRideIsIgnored`, `TestDriverStatusLifecycle` |
| I8 | Terminal ride states stay terminal; the driver ends `AVAILABLE` | `TestTerminalStates`, `TestCancelReleasesTheOfferedDriver`, `TestCompletionReleasesTheDriver` |
| I9 | Matching uses the newest location; old events never override new ones | `TestIndexKeepsOnlyLatestAvailableDrivers` |
| I10 | Inner layers never depend on infrastructure | `test/arch` (4 tests) |

## 4. How the integration tests work

* **Containers:** `pkg/testpg.Main` starts a Postgres 18 container per test package and applies the same embedded migrations as production. The Redis test starts `redis:8-alpine`.
* **Wiring:** each test builds the use cases with the **real adapters** (`pgutil.TxManager`, the `postgres` repositories, `broker.Publisher`, `broker.Inbox`), as `cmd/main.go` does.
* **Saga tests without a broker:** a test "world" plays ride-service and driver-service. It reads the commands the orchestrator wrote to its outbox and answers them through the real messaging adapter, simulating each driver's `OFFERED`/`BUSY` holder. This exercises locks, conditional updates and unique indexes deterministically.
* **Real concurrency:** goroutines wait on a closed channel (barrier) and fire together.

## 5. Running

```bash
make test        # unit + architecture (no Docker), < 2 s
make test-int    # + integration (testcontainers on Colima), ~15 s
make up && make test-e2e   # end-to-end incl. the real 30 s timeout, ~40 s
make postman     # Postman collection via newman (needs Node)
go test -tags e2e -short ./test/e2e/   # e2e without the 30 s timeout test
```

With Colima, testcontainers needs `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`; the Makefile sets it. Troubleshooting: [local-development.md §8](local-development.md#8-troubleshooting).

## 6. Not tested (yet)

| Gap | Why | Next step |
|---|---|---|
| Load / throughput | Not required; no performance targets measured | k6 scenarios for location ingestion and hot-zone contention |
| Fake-based use-case tests for driver and dispatch | Covered by integration tests on real Postgres | Same pattern as `services/ride/internal/usecase/rides_test.go` |
| Broker failure injection (Redpanda down, DLQ) | Covered by design and a manual crash drill only | Toxiproxy or container pause in an e2e test |
| Contract tests between services | One shared envelope; e2e covers the flows | Schema registry or consumer-driven contracts if more teams consume the topics |
| TDD commit history | Tests were written alongside the code | — |
