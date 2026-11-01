# Implementation Plan

## Repository layout

```
coding-challenge-3/kvp/
├── README.md                     # run, test, summary of strategies (links into docs/)
├── docker-compose.yml
├── Makefile
├── go.work                       # one Go workspace, three modules + shared lib
├── deploy/
│   ├── nginx.conf                # gateway routes
│   ├── postgres/init.sql         # 3 databases + users
│   └── grafana/dashboards/       # "Dispatch overview"
├── pkg/                          # shared library (no domain logic)
│   ├── geo/                      # Point, Validate, Haversine
│   ├── messaging/                # envelope, franz-go producer/consumer, outbox relay, inbox
│   ├── obs/                      # OTel setup, slog handler with ride_id/driver_id/event_id
│   └── httpx/                    # problem+json, middleware, health
├── services/
│   ├── ride/                     # cmd/ride, internal/{ride (state machine), api, store, handlers}
│   ├── driver/                   # cmd/driver, internal/{driver (state machine), api, store, handlers}
│   └── dispatch/                 # cmd/dispatch, internal/{saga, match, offer, geoindex, sweeper, api}
├── migrations/{ride,driver,dispatch}/   # goose SQL
├── scripts/demo.sh
├── test/e2e/                     # black-box tests against the compose stack
└── docs/
```

## Libraries

| Need | Choice | Why |
|---|---|---|
| HTTP | `net/http` (Go 1.22+ pattern routing) | No framework needed |
| Postgres | `jackc/pgx/v5` + `pgxpool` | Fast, native types |
| Migrations | `pressly/goose/v3` | SQL files, run as a one-shot container |
| Kafka/Redpanda | `twmb/franz-go` | Pure Go, idempotent producer, good Redpanda support |
| Redis | `redis/go-redis/v9` | GEO commands + Lua |
| IDs | `google/uuid` (v7) | Time-sortable |
| Observability | `go.opentelemetry.io/otel` + `otelhttp`, `otelpgx`, `kotel`, `otelslog` | Traces/metrics/logs in one SDK |
| Tests | `testing`, `stretchr/testify`, `testcontainers-go` | Real Postgres for concurrency tests |

## Required tests → files

| Area | Test | File |
|---|---|---|
| Matching | nearest · outside radius · offline · busy · none · tie-break · excluded | `services/dispatch/internal/match/select_test.go` (pure) |
| Driver status | AVAILABLE→OFFERED · reject/cancel→AVAILABLE · accept→BUSY · complete→AVAILABLE · invalid rejected · OFFERED/BUSY can't go OFFLINE | `services/driver/internal/driver/statemachine_test.go` |
| State machine | valid · invalid (`COMPLETED→DRIVER_ASSIGNED`) · duplicate no-op · reject clears `assignedDriverId` · terminal states | `services/ride/internal/ride/statemachine_test.go` |
| Rematching | reject → second driver · rejected driver never re-offered · none left → MATCHING · timeout = reject | `services/dispatch/internal/saga/saga_test.go` (fake participants, fake clock) |
| Concurrency | two rides, one driver, 50 iterations → exactly one offer | `services/driver/internal/store/reserve_concurrency_test.go` (testcontainers) + `test/e2e/concurrency_test.go` |
| Idempotency | same key → same ride · key reused with other body → 422 · duplicate accept → 200 once, one BUSY · duplicate `RideRequested` → one saga | `services/ride/internal/api/create_test.go`, `services/dispatch/internal/offer/accept_test.go`, `services/dispatch/internal/saga/dedup_test.go` |
| Validation | lat/lng bounds, missing passenger, unknown passenger | `pkg/geo/validate_test.go`, ride API test |
| End-to-end | full happy path, reject→rematch, timeout (with `OFFER_TIMEOUT=2s`), cancel race | `test/e2e/*_test.go` |

## Build order (timeboxed)

| # | Slice | Done when |
|---|---|---|
| 1 | Compose infra (Postgres, Redis, Redpanda, LGTM, gateway) + Makefile | `make up` is healthy |
| 2 | `pkg/geo`, both state machines, matcher. All pure, with unit tests | Most required tests green with no infra |
| 3 | ride-service: create (idempotency), get, cancel, complete + outbox relay | AC-01, AC-06 via curl |
| 4 | driver-service: availability, location, reserve/release/busy commands + events | AC-02, AC-03, AC-09 integration test |
| 5 | dispatch-service: geo projector, saga, offers, accept/reject, sweeper | AC-04, 05, 07, 10 end-to-end |
| 6 | Inbox dedup everywhere, DLQ, saga resumer | AC-08 tests |
| 7 | OTel wiring, `make trace-ride`, dashboard | Runbook works |
| 8 | README + demo script | Everything reproducible from a clean clone |

## Configuration (env)

| Var | Default | Service |
|---|---|---|
| `MATCH_RADIUS_M` | `2000` | dispatch |
| `OFFER_TIMEOUT` | `30s` | dispatch |
| `DISPATCH_RETRY_INTERVAL` | `10s` | dispatch |
| `SWEEPER_INTERVAL` | `1s` | dispatch |
| `OUTBOX_POLL_INTERVAL` | `50ms` | all |
| `CONSUMER_MAX_ATTEMPTS` | `5` | all |
| `DATABASE_URL`, `REDIS_ADDR`, `KAFKA_BROKERS`, `OTEL_EXPORTER_OTLP_ENDPOINT` | compose values | all |
