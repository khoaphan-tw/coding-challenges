# ADR-011: Testing strategy: pure unit tests + testcontainers integration tests + black-box e2e

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §5 Testing & Validation (matching, driver status, state machine, rematching, concurrency, idempotency); root README "Tests & Reliability" (coverage + edge cases)
* **Related:** [testing.md](../testing.md) (test levels, invariants), [requirements-traceability.md](../requirements-traceability.md) (requirement ↔ test map)

## Context

The riskiest code is not in Go functions; it is in the **interaction with Postgres**: row locks, conditional updates, unique indexes and `SKIP LOCKED`. AC-09 explicitly asks for a **concurrency test with two rides competing for one driver**. A test that mocks the database cannot prove that.

## Options compared

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Real Postgres/Redis in tests (testcontainers-go)** | Proves SQL, locks, constraints and Lua with the same engines as production | Needs Docker; ~2–5 s per package | ✅ **Chosen** for integration tests |
| Mocks (`gomock`, `sqlmock`) | Fast, no Docker | Cannot show that `WHERE status='AVAILABLE'` serialises two transactions | ❌ For SQL. Hand-written fakes are still used for use-case flows |

The rest of the strategy needs no comparison: pure unit tests for rules, use-case tests with in-memory fakes of the ports, an architecture test for the dependency rule, black-box e2e tests against the running stack, and a Postman collection for reviewers. Assertions use the standard `testing` package with table-driven tests.

## Decision

| Layer | Tool | Count | Highlights |
|---|---|---|---|
| Unit | `go test` | 6 packages | Ride use cases with fakes of the ports; architecture rules; ride state machine (valid / invalid / duplicate / terminal), driver status rules, matching (radius, offline, busy, tie-break, exclusions), coordinate validation |
| Integration | `go test -tags integration` + testcontainers | 4 packages | **50 rounds of two goroutines reserving one driver** on real Postgres; idempotent create incl. 10 parallel duplicates; saga flows with simulated participants (reject → rematch, timeout, cancel/complete compensation, accept racing timeout, duplicate events); Redis GEO versioning |
| E2E | `go test -tags e2e` against `make up` | 12 tests | Happy path, rematch, duplicate create/accept, two rides one driver, cancel release, invalid transitions, the real 30 s timeout, gRPC v2 location stream and offer push, every REST response validated against `api/openapi.yaml` |
| Exploratory / review | Postman collection (56 requests: happy path + one folder per AC), `make demo` | — | Same scenarios, readable request/response logs |
| Static checks | golangci-lint ([.golangci.yml](../../.golangci.yml)), Redocly for the OpenAPI spec | — | Bug-finding linters only (errcheck, staticcheck, govet, unused, ineffassign, bodyclose, noctx, misspell) + gofmt/goimports; no style-only linters |
| CI | GitHub Actions ([cc3-kvp.yml](../../../../.github/workflows/cc3-kvp.yml)) | 3 jobs | `lint` ∥ `test` (unit + integration on the runner's Docker) → `e2e` (whole stack, e2e + Postman) on every push |

Rules that make tests reliable:

* **Every test owns its data:** random ids, and in e2e/Postman a random spot on the map, so tests run in any order and in parallel with the demo.
* **No sleeps for correctness:** e2e and Postman poll until a state is reached (async dispatch). Sleeps are used only to give a wrong outcome time to appear.
* **Real concurrency:** a barrier channel releases goroutines at the same moment.

## Consequences

* ✅ AC-09 is proven against the real database, not a mock.
* ✅ The unit tests run in under a second without Docker; CI runs every level on each push.
* ⚠️ Integration tests need a container engine (Colima locally) and ~15 s per run.

## When to revisit

* More services or teams consuming topics: contract tests or a schema registry.
* Performance targets: k6 scenarios for contention in hot zones (B3).
