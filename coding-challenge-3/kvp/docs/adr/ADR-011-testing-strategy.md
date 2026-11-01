# ADR-011: Testing strategy: pure unit tests + testcontainers integration tests + black-box e2e

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §5 Testing & Validation (matching, driver status, state machine, rematching, concurrency, idempotency); root README "Tests & Reliability" (coverage + edge cases)
* **Related:** [testing.md](../testing.md) (test levels, invariants), [requirements-traceability.md](../requirements-traceability.md) (requirement ↔ test map)

## Context

The riskiest code is not in Go functions; it is in the **interaction with Postgres**: row locks, conditional updates, unique indexes and `SKIP LOCKED`. AC-09 explicitly asks for a **concurrency test with two rides competing for one driver**. A test that mocks the database cannot prove that.

## Options compared

| Option | What it proves | Speed | Realism | Verdict |
|---|---|---|---|---|
| **Pure unit tests** (state machines, matching, validation) | Business rules | ms | Rules only | ✅ Chosen: all rules extracted into pure functions |
| **Use-case tests with in-memory fakes of the ports** | Application flows (idempotency, duplicate commands, replies) | ms | Flows, not SQL | ✅ Chosen: enabled by Clean Architecture ([ADR-012](ADR-012-clean-architecture.md)) |
| **Architecture test** (`go list` dependency rules) | Layers depend inward; no infrastructure in inner rings | ~2 s | Structure | ✅ Chosen ([test/arch](../../test/arch/arch_test.go)) |
| Generated mocks (`gomock`, `mockery`) / `sqlmock` | That code calls the mock as expected | ms | ❌ Cannot show that `WHERE status='AVAILABLE'` serialises two transactions | ❌ Hand-written fakes for flows; real Postgres for SQL |
| In-memory fake repository | Logic with a fake store | ms | ❌ Fakes don't reproduce Postgres locking or isolation | ❌ |
| **testcontainers-go** (real Postgres 18 / Redis 8 per test package) | SQL, locks, constraints, Lua, real concurrency | ~2–5 s per package | ✅ Same engines as production | ✅ Chosen for integration tests |
| Shared docker-compose test environment | Same, shared across tests | Fast after start | ✅ | ❌ Tests interfere; manual lifecycle |
| Embedded Postgres (`fergusstrange/embedded-postgres`) | Real Postgres binary, no Docker | ~1–2 s | ✅ for Postgres only | Viable; no Redis/Redpanda equivalent |
| **Black-box e2e in Go against the running stack** | Whole system through the gateway, incl. Redpanda and timeouts | seconds | ✅ Highest | ✅ Chosen (`make test-e2e`) |
| Postman / newman collection | Same flows, readable for reviewers, manual exploration | seconds | ✅ | ✅ Also provided ([postman/](../../postman/)) |
| Load / race tools (k6, Gatling, vegeta) | Throughput, contention under load | minutes | ✅ | Future (not required) |
| Contract tests (Pact) between services | Message/API contracts | fast | Partial | Future: one envelope, 3 services, e2e covers it today |
| Property-based tests (`testing/quick`, `rapid`) | Invariants over random inputs | fast | Rules only | Future: e.g. "matching never picks an excluded driver" |

**Assertion style:** standard `testing` with `t.Fatalf`, plus table-driven tests. testify was considered; the standard library keeps tests dependency-free and is idiomatic Go. Ginkgo/Gomega (BDD style) adds a DSL the team does not need.

## Decision

| Layer | Tool | Count | Highlights |
|---|---|---|---|
| Unit | `go test` | 6 packages | Ride use cases with fakes of the ports; architecture rules; ride state machine (valid / invalid / duplicate / terminal), driver status rules, matching (radius, offline, busy, tie-break, exclusions), coordinate validation |
| Integration | `go test -tags integration` + testcontainers | 4 packages | **50 rounds of two goroutines reserving one driver** on real Postgres; idempotent create incl. 10 parallel duplicates; saga flows with simulated participants (reject → rematch, timeout, cancel/complete compensation, accept racing timeout, duplicate events); Redis GEO versioning |
| E2E | `go test -tags e2e` against `make up` | 8 tests | Happy path, rematch, duplicate create/accept, two rides one driver, cancel release, invalid transitions, the real 30 s timeout |
| Exploratory / review | Postman collection (119 requests, 12 scenario folders), `make demo` | — | Same scenarios, readable request/response logs |

Rules that make tests reliable:

* **Every test owns its data:** random ids, and in e2e/Postman a random spot on the map, so tests run in any order and in parallel with the demo.
* **No sleeps for correctness:** e2e and Postman poll until a state is reached (async dispatch). Sleeps are used only to give a wrong outcome time to appear.
* **Real concurrency:** a barrier channel releases goroutines at the same moment.

## Consequences

* ✅ AC-09 is proven against the real database, not a mock.
* ✅ The unit tests run in under a second without Docker.
* ⚠️ Integration tests need a container engine (Colima) and ~10–20 s per run.
* ⚠️ No TDD history: tests were written alongside the code, not before. The root README's TDD bonus (+5) does not apply.

## When to revisit

* More services or teams consuming topics: contract tests or a schema registry.
* Performance targets: k6 scenarios for contention in hot zones (B3).
