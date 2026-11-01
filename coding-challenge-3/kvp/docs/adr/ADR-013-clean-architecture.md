# ADR-013: Code structure: Clean Architecture inside each service

* **Status:** Accepted (supersedes the earlier "functional core + one app package" layout)
* **Date:** 2026-11-01
* **Related:** [ADR-006 service architecture](ADR-006-service-architecture.md) (how the *system* is split), [ADR-012 testing](ADR-012-testing-strategy.md), [architecture §14](../architecture.md#14-repository-layout) (layout)

## Context

ADR-006 decides how the **system** is split (3 services). This ADR decides how the **code inside each service** is organised.

The first version used a pure domain package plus one `app` package that mixed HTTP handlers, Kafka handlers and SQL. It was small and fast to write, but the business flow (e.g. the saga) could only be tested with a real database, and nothing stopped HTTP or SQL concerns from leaking into business logic.

## Decision drivers

* Business rules and use cases testable **without** Postgres, Redis or Redpanda.
* The correctness-critical SQL (atomic conditional updates, `FOR UPDATE`, outbox in the same transaction; [ADR-002](ADR-002-concurrency.md), [ADR-004](ADR-004-messaging-outbox-inbox.md)) must stay explicit and in one place.
* One obvious place for each kind of code: a reviewer should find "where is the rule?" and "where is the SQL?" immediately.
* Not over-engineered for services of a few hundred lines each.

## Options compared

| Structure | Idea | Business logic testable without DB? | Where does the transaction live? | Ceremony | Verdict |
|---|---|---|---|---|---|
| Single package per service | Handlers + SQL + rules together | ❌ | Anywhere | None | ❌ Rules buried in handlers |
| **Functional core + imperative shell** (previous version) | Pure domain package; one "app" package with HTTP + messaging + SQL | Rules ✅, flows ❌ | In the app package | Low | Superseded: flows (saga) needed a DB to test |
| 3-layer (controller → service → repository) | Layers by technical role; dependencies point **down** to data access | Only with mocks of concrete repositories | Usually leaks: the service needs `*sql.Tx` or the repository owns it | Low | ❌ The domain depends on persistence; transactions spanning several repositories get awkward |
| Hexagonal (ports & adapters) | Application core with ports; adapters on both sides | ✅ with fake adapters | Core asks a port for a unit of work | Medium | Very close: Clean Architecture below *is* hexagonal with explicit inner rings |
| Onion | Concentric rings: domain model → domain services → application → infrastructure | ✅ | Application ring | Medium | Equivalent to Clean; naming differs |
| **Clean Architecture** (entities → use cases → interface adapters → frameworks and drivers) | Dependency rule: source code depends only inward; use cases own their ports | ✅ with fakes; ✅ with real adapters in integration tests | **Port `TxManager.WithinTx(ctx, fn)`**; repositories join the transaction from `ctx` | Medium | ✅ **Chosen** |
| Vertical slice | One folder per feature (request → handler → DB) | Per slice | Per slice | Low | ❌ Good for CRUD-heavy apps; our services have one cohesive aggregate each |
| DDD tactical + CQRS + event sourcing | Aggregates, command/query split, event store | ✅ | Event store | High | ❌ Too heavy ([ADR-003](ADR-003-saga-orchestration.md)); we only keep a CQRS-style read model for GEO |

**Why Clean Architecture over plain hexagonal?** In practice they are the same here. We use Clean's explicit **domain** and **use case** rings because the domain (state machines, matching) is substantial and pure, while the use cases (saga orchestration, idempotent command handling) are the application logic. Naming the rings makes the dependency rule testable ([test/arch](../../test/arch/arch_test.go)).

**Why not 3-layer?** Our key invariant is "lock row → decide → write state + history + outbox in one transaction". In 3-layer, either the service passes `*pgx.Tx` around (the database leaks into business code) or each repository opens its own transaction (the invariant breaks). Clean Architecture's transaction port keeps the invariant without either problem.

## Decision

Every service has the same layout. Dependencies point inward only:

```
services/<svc>/
├── cmd/main.go            Composition root: builds adapters, injects them into use cases, starts the driving adapters
└── internal/
    ├── domain/            Entities + business rules. Pure Go; imports only the standard library and pkg/geo.
    ├── usecase/           Application logic + ports (interfaces it needs) + input/output types.
    │                      Imports domain, pkg/messaging (pure message contract), pkg/obs (OTel API).
    └── adapter/
        ├── http/          Driving: decode request → call use case → present JSON / RFC 9457 problem
        ├── messaging/     Driving: decode Kafka envelope → call use case
        ├── postgres/      Driven: implements repository ports with pgx (+ embedded migrations)
        ├── redis/         Driven (dispatch): GEO index, implements CandidateSource + GeoIndex
        └── driverclient/  Driven (dispatch): driver-service HTTP client, implements DriverDirectory
pkg/                       Shared, framework-level code (outer ring)
├── messaging/             Pure message contract: envelope, topics, types
├── broker/                Outbox, relay, inbox, consumer with retry/DLQ (implements Publisher/Inbox ports)
├── pgutil/                Connect, migrate, TxManager (implements the TxManager port)
├── obs/  obs/otelsetup/   Correlation, trace helpers (API) / SDK and exporter wiring
├── httpx/, svc/, geo/, testpg/
```

### Ports per service

| Service | Ports (declared in `usecase/ports.go`) | Implemented by |
|---|---|---|
| ride | `TxManager`, `RideRepository`, `PassengerRepository`, `IdempotencyStore`, `Publisher`, `Inbox` | `pgutil.TxManager`, `adapter/postgres.Repos`, `broker.Publisher`, `broker.Inbox` |
| driver | `TxManager`, `DriverRepository` (atomic compare-and-set methods: `ReserveIfAvailable`, `MarkBusyIfOffered`, `ReleaseIfHeld`, `SetStatusIf`), `Publisher`, `Inbox` | `pgutil.TxManager`, `adapter/postgres.Drivers`, `broker.*` |
| dispatch | `TxManager`, `SagaRepository`, `OfferRepository` (compare-and-set on `PENDING`), `ExclusionRepository`, `CandidateSource`, `GeoIndex`, `DriverDirectory`, `Publisher`, `Inbox` | `pgutil.TxManager`, `adapter/postgres.{Sagas,Offers,Exclusions}`, `adapter/redis.Index`, `adapter/driverclient.Client`, `broker.*` |

### How a transaction crosses the layers

```go
// usecase (no pgx import)
err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
    cur, err := u.Rides.GetForUpdate(ctx, id)      // adapter: SELECT … FOR UPDATE in the ctx transaction
    next, changed, err := domain.Apply(cur, action) // domain: pure rule
    next, err = u.Rides.Update(ctx, next)           // adapter: UPDATE … (same transaction)
    _, err = u.Publisher.Publish(ctx, topic, key, env) // broker: INSERT INTO outbox (same transaction)
    ...
})
```

`pgutil.TxManager` stores the `pgx.Tx` in `ctx`; adapters call `pgutil.Conn(ctx, pool)` and the outbox/inbox call `pgutil.Tx(ctx)`, which refuses to run outside a transaction.

**Atomicity stays in the adapter, and the rule stays in the domain:** a repository port like `ReserveIfAvailable` *is* the single conditional `UPDATE … WHERE status='AVAILABLE'`. When it reports `applied=false`, the use case asks the pure domain rule (`domain.Reserve`) whether that means "duplicate" or "not allowed".

### Pragmatic exceptions (documented on purpose)

* **Cross-cutting observability in use cases:** `log/slog`, the OpenTelemetry **API** (metrics, `obs.ContinueTrace`), and `pkg/obs` context helpers. Wrapping logging and tracing behind ports adds ceremony with no testing benefit; the SDK and exporters stay outside (`obs/otelsetup`).
* **Message envelope in use cases:** `pkg/messaging.Envelope` is a pure data contract (the published language between services), not infrastructure. Use cases build envelopes; `pkg/broker` moves them.
* **Shared ports per service, not per use case:** each service declares its ports once in `usecase/ports.go`, which is enough at this size.

### Enforced, not just documented

[test/arch/arch_test.go](../../test/arch/arch_test.go) runs with `make test` and fails if:

* a `domain` package imports anything but the standard library and `pkg/geo`;
* a `domain` or `usecase` package (transitively) reaches pgx, franz-go, go-redis, the OTel SDK/exporters, `pkg/broker`, `pkg/pgutil`, `pkg/svc` or `pkg/httpx`, or any of our packages that import `net/http`;
* an inner layer imports an `adapter` package;
* an adapter imports a sibling adapter (adapters talk only through use cases);
* `pkg/messaging` (the contract) depends on infrastructure.

## Consequences

* ✅ Use cases are unit-testable with in-memory fakes (e.g. [ride usecase tests](../../services/ride/internal/usecase/rides_test.go): create, idempotency, duplicate commands, rejection reply, in ms with no Docker).
* ✅ Concurrency and SQL behaviour are still proven against real Postgres, now at the adapter or use-case level (integration tests wire the real adapters exactly like `cmd/main.go`).
* ✅ Swapping a technology means writing an adapter. Examples: Redis → Valkey, or PostGIS for `CandidateSource`, or Temporal for the saga, would touch `adapter/` and `cmd/` only.
* ⚠️ More files and interfaces (~15 ports across 3 services). Each port has exactly one production implementation, and the fakes exist for tests.
* ⚠️ The transaction travels in `context.Context` (implicit). It is guarded by `pgutil.Tx(ctx)` erroring when there is none, and it is a widely used Go pattern, but it is less explicit than passing a `Tx` parameter.

## When to revisit

* A service grows several aggregates: split `usecase/` per feature (vertical slices inside the Clean rings).
* Many read endpoints: add a separate query side (CQRS) that reads directly from the database without going through the domain.
