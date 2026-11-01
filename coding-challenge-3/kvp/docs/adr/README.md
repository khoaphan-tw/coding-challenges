# Architecture Decision Records

Every ADR uses the same template: **context → the choice compared with one or two common alternatives (or simply why, when no comparison is needed) → decision → consequences → when to revisit.**

| ADR | Decision | Chosen | Covers |
|---|---|---|---|
| [001](ADR-001-matching.md) | Matching algorithm and spatial index | Greedy nearest within 2 km, ties by `driverId`; Redis GEO projection + Haversine in Go | AC-03, AC-04, AC-07 |
| [002](ADR-002-concurrency.md) | Concurrency control | Atomic conditional `UPDATE … WHERE status='AVAILABLE'`, row locks, partial unique indexes | AC-02, AC-05, AC-09 |
| [003](ADR-003-saga-orchestration.md) | Consistency across services | Orchestrated saga (pivot = offer accepted), hand-rolled | AC-05, AC-06, AC-07, AC-10, T1 |
| [004](ADR-004-messaging-outbox-inbox.md) | Reliable messaging | Transactional outbox + inbox, at-least-once, idempotent consumers, DLQ | AC-08, AC-10 |
| [005](ADR-005-observability.md) | Observability | OpenTelemetry → Grafana LGTM, slog text logs, one trace per ride | Observability, T3 |
| [006](ADR-006-service-architecture.md) | Architecture style | 3 microservices split by aggregate, database per service | T1, all ACs |
| [007](ADR-007-language-and-libraries.md) | Language and libraries | Go 1.27; `net/http`, pgx, franz-go, go-redis, OpenTelemetry | T2 |
| [008](ADR-008-data-stores.md) | Data stores | PostgreSQL 18 (truth), Redis 8 (disposable GEO index) | AC-06, AC-08, AC-09, T5 |
| [009](ADR-009-message-broker.md) | Message broker | Redpanda (Kafka API) | AC-10, T5 |
| [010](ADR-010-api-and-identifiers.md) | API, idempotency keys, identifiers | REST + RFC 9457 (v1), gRPC streams for 2 driver APIs (v2), `Idempotency-Key` stored with the response, UUIDv7 | AC-01, AC-05, AC-08 |
| [011](ADR-011-testing-strategy.md) | Testing strategy | Pure unit + fakes + testcontainers + e2e + Postman + architecture test | §5 Testing |
| [012](ADR-012-clean-architecture.md) | Code structure inside a service | Clean Architecture (domain → use cases + ports → adapters), enforced by a test | maintainability, testability |

**Read in this order if you have 20 minutes:** 006 (what the system is) → 003 (how it stays consistent) → 002 (how races are decided) → 004 (how messages are safe) → 001 (how drivers are picked) → 012 (how the code is organised).
