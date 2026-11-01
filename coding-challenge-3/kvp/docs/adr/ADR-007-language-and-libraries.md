# ADR-007: Language and libraries: Go with the standard library first, plus a few focused drivers

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** team constraint T2 (Go)
* **Related:** [architecture §14](../architecture.md#14-repository-layout) (code layout)

## Context

The team chose Go (T2). This ADR records why Go fits the problem compared with other common stacks, and why each library was picked over its alternatives. The guiding rule: **standard library first; one well-maintained, focused library where the standard library has nothing.** No frameworks that hide the mechanisms the challenge asks us to explain (transactions, offsets, locks).

## Part 1: language and runtime

| Criterion | **Go 1.27** | Java 21 + Spring Boot 3 | Kotlin + Ktor/Spring | C# / .NET 8 | TypeScript / Node (NestJS) | Python (FastAPI) | Rust (Axum/Tokio) |
|---|---|---|---|---|---|---|---|
| Concurrency model | Goroutines + channels, preemptive | Virtual threads (Loom) / reactive | Coroutines | async/await | Single-threaded event loop | asyncio (GIL) | async/await, zero-cost |
| Container image / memory | ~15–25 MB static binary on distroless; ~20–40 MB RSS | 200+ MB image; 200–400 MB RSS (less with GraalVM native) | Similar to Java | ~100 MB image; ~80–150 MB RSS (AOT smaller) | ~150 MB image; ~60–100 MB RSS | ~150 MB image; ~60–100 MB RSS | ~10 MB; ~10 MB RSS |
| Startup | ms | seconds (ms with native image) | seconds | ~100s of ms | ~100s of ms | ~1 s | ms |
| Saga / messaging ecosystem | Libraries, not frameworks (franz-go, Watermill, Temporal SDK) | Richest (Spring Kafka, Axon, Eventuate, Camunda) | Spring ecosystem | MassTransit, NServiceBus | KafkaJS, NestJS microservices | Faust (stale), confluent-kafka | rdkafka bindings |
| Postgres / Kafka / Redis / OTel drivers | Mature, pure Go | Mature | Mature | Mature | Mature | Mature | Good, younger |
| Learning curve for the team | Low; small language | Medium (Spring magic) | Medium | Medium | Low | Low | High (ownership/lifetimes) |
| Fit for 10 containers on a laptop VM | Excellent | Heavy | Heavy | Good | Good | Good | Excellent |
| Verdict | ✅ **Chosen** | Best ecosystem, too heavy here | — | Strong alternative | Viable; weaker for CPU-bound matching | ❌ GIL, slower | ❌ Productivity cost in the timebox |

**Why Go:** small static binaries (three services plus infrastructure fit an 8 GB Colima VM), goroutines map naturally to "consumer loop + relay + sweeper + HTTP server" per service, mature pure-Go drivers for every component we use, and explicit code. Transactions, offsets and locks stay visible, which is exactly what the challenge scores. Java/Spring would give more out of the box (Spring Kafka, Spring Data, Axon sagas), but it hides those mechanisms and costs far more memory per service.

## Part 2: libraries

### HTTP routing

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **`net/http` `ServeMux` (Go 1.22+ method + path patterns)** | Standard; `GET /rides/{rideId}` routing; `r.PathValue`; zero dependencies | No middleware chain helpers (trivial to write) | ✅ **Chosen** |
| `go-chi/chi` | Idiomatic, `net/http`-compatible, middleware set | Not needed since Go 1.22 patterns | Runner-up |
| `gin-gonic/gin` | Popular, fast, binding/validation | Own context type; magic binding | ❌ |
| `labstack/echo` | Similar to Gin | Own context type | ❌ |
| `gofiber/fiber` | Very fast (fasthttp) | Not `net/http`; breaks `otelhttp` and standard middleware | ❌ |
| `gorilla/mux` | Long-time standard | Superseded by `ServeMux` patterns | ❌ |

### PostgreSQL access

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **`jackc/pgx/v5` (native) + `pgxpool`** | Fastest Go Postgres driver; native types (uuid, arrays, `text[]`); `BeginFunc` transactions; Postgres error codes | Postgres only (fine) | ✅ **Chosen** |
| `database/sql` + `lib/pq` | Standard interface | `lib/pq` is in maintenance mode; fewer types | ❌ |
| `sqlc` (SQL → typed Go code) | Compile-time checked queries | Code generation step; our queries are few and dynamic in places | Considered; would add it at more queries |
| `jmoiron/sqlx` | Struct scanning | Little gain over pgx `CollectRows` | ❌ |
| GORM | Fast CRUD, migrations | ORM hides `FOR UPDATE`, conditional `UPDATE … WHERE status=…` and row counts, the core of [ADR-002](ADR-002-concurrency.md) | ❌ |
| `ent` / `bun` | Schema-as-code / query builder | Same concern; extra abstraction | ❌ |

### Kafka / Redpanda client

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **`twmb/franz-go`** | Pure Go; full protocol support; idempotent producer; manual commit; `BlockRebalanceOnPoll`; `kadm` admin client; used and recommended in Redpanda's docs | Lower-level API (more control) | ✅ **Chosen** |
| `confluentinc/confluent-kafka-go` | Wraps librdkafka (battle-tested) | cgo, so harder static builds and cross-compiling on Apple Silicon | ❌ |
| `IBM/sarama` | Long history | Complex API; historically tricky consumer groups | ❌ |
| `segmentio/kafka-go` | Simple API | Fewer features (e.g. transactions, admin) | ❌ |
| Watermill (abstraction over the above) | Router, middleware, SQL outbox forwarder | Extra abstraction ([ADR-004](ADR-004-messaging-outbox-inbox.md)) | ❌ For now |

### Redis client

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **`redis/go-redis/v9`** | Official; GEO commands typed; Lua `Script`; pipelines; `redisotel` tracing | — | ✅ **Chosen** |
| `redis/rueidis` | Faster, client-side caching | Different API, smaller community | ❌ |
| `gomodule/redigo` | Minimal | Untyped commands; no OTel integration | ❌ |

### Migrations

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Embedded SQL files + 60-line migrator** (`pgutil.Migrate`, advisory lock, `schema_migrations`) | No dependency; migrations ship inside each binary; safe with replicas | Forward-only, no down migrations | ✅ **Chosen** |
| `pressly/goose` | Popular, Go or SQL migrations, embed support | Another dependency for ~6 SQL files | Runner-up |
| `golang-migrate/migrate` | CLI + library, many drivers | Separate up/down files; heavier | ❌ |
| Atlas (declarative schema) | Diffing, linting | Extra tool and concepts | ❌ |
| Flyway / Liquibase (JVM) | Industry standard | JVM container just for migrations | ❌ |

### Other

| Need | Chosen | Alternatives considered |
|---|---|---|
| IDs | `google/uuid` **v7** | v4 (random, poor index locality), ULID (`oklog/ulid`, same idea, non-standard), Snowflake (needs worker ids), `bigserial` (leaks volume, not globally unique). See [ADR-010](ADR-010-api-and-identifiers.md) |
| Logging | `log/slog` | zap, zerolog, logrus ([ADR-005](ADR-005-observability.md)) |
| Telemetry | OpenTelemetry Go SDK + `otelhttp`, `redisotel`, `otelslog` | Vendor agents ([ADR-005](ADR-005-observability.md)) |
| Configuration | Environment variables (`svc.Env`, `svc.Duration`) | `spf13/viper`, `knadh/koanf`, `kelseyhightower/envconfig`: unnecessary for ~10 settings |
| Dependency injection | Manual constructor wiring in each `cmd/main.go` (the Clean Architecture composition root, [ADR-012](ADR-012-clean-architecture.md)) | `google/wire`, `uber-go/fx`: unnecessary for 3 small services |
| Testing | `testing` + `testcontainers-go` | testify, ginkgo/gomega, mocks ([ADR-011](ADR-011-testing-strategy.md)) |

## Decision

Go 1.27, one Go module, standard library first. Third-party code only for drivers: pgx, franz-go, go-redis, OpenTelemetry, uuid, testcontainers.

## Consequences

* ✅ ~20 MB images; services start in milliseconds; low memory in the Colima VM.
* ✅ Every important mechanism (transactions, conditional updates, offsets, retries) is visible in our code.
* ⚠️ We wrote small pieces that frameworks would provide: the outbox relay, the consumer loop with retry/DLQ, the migrator and the HTTP helpers (~500 lines in `pkg/`).
* ⚠️ No ORM or code generation, so SQL is hand-written and checked only by integration tests.

## When to revisit

* Many more queries: add **sqlc**.
* Many message types and middlewares: **Watermill**.
* Several workflows: **Temporal Go SDK** ([ADR-003](ADR-003-saga-orchestration.md)).
