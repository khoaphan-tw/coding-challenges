# ADR-012: Code structure: Clean Architecture inside each service

* **Status:** Accepted (supersedes the earlier "functional core + one app package" layout)
* **Date:** 2026-11-01
* **Related:** [ADR-006 service architecture](ADR-006-service-architecture.md) (how the *system* is split), [ADR-011 testing](ADR-011-testing-strategy.md), [architecture §14](../architecture.md#14-repository-layout) (layout)

## Context

ADR-006 decides how the **system** is split (3 services). This ADR decides how the **code inside each service** is organised.

The first version used a pure domain package plus one `app` package that mixed HTTP handlers, Kafka handlers and SQL. It was small and fast to write, but the business flow (e.g. the saga) could only be tested with a real database, and nothing stopped HTTP or SQL concerns from leaking into business logic.

## Options compared

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Clean Architecture** (domain → use cases + ports → adapters) | Use cases testable with fakes; SQL stays in one adapter; the dependency rule can be tested; transactions cross layers through a `TxManager` port | More files and interfaces | ✅ **Chosen** |
| 3-layer (controller → service → repository) | Familiar; less ceremony | Business code depends on persistence: either `*pgx.Tx` is passed around, or each repository opens its own transaction and the "lock → decide → write state + history + outbox" invariant breaks | ❌ |

Hexagonal (ports & adapters) is essentially the same idea; Clean Architecture's named **domain** and **use case** rings make the dependency rule testable ([test/arch](../../test/arch/arch_test.go)).

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
        ├── grpc/          Driving (driver, dispatch): gRPC API v2 streams → same use cases as http/
        ├── messaging/     Driving: decode Kafka envelope → call use case
        ├── postgres/      Driven: implements repository ports with pgx (+ embedded migrations)
        ├── redis/         Driven (dispatch): GEO index, implements CandidateSource + GeoIndex
        └── driverclient/  Driven (dispatch): driver-service HTTP client, implements DriverDirectory
pkg/                       Shared, framework-level code (outer ring)
├── messaging/             Pure message contract: envelope, topics, types
├── broker/                Outbox, relay, inbox, consumer with retry/DLQ (implements Publisher/Inbox ports)
├── pgutil/                Connect, migrate, TxManager (implements the TxManager port)
├── obs/  obs/otelsetup/   Correlation, trace helpers (API) / SDK and exporter wiring
├── httpx/, grpcx/, svc/, geo/, testpg/
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

* ✅ Use cases are unit-testable with in-memory fakes (e.g. [ride usecase tests](../../services/ride/internal/usecase/rides_test.go), in ms with no Docker).
* ✅ Swapping a technology means writing an adapter (e.g. Redis → Valkey, or a gRPC API next to REST) without touching use cases.
* ⚠️ The transaction travels implicitly in `context.Context`, guarded by `pgutil.Tx(ctx)` returning an error when there is none.

## When to revisit

* A service grows several aggregates: split `usecase/` per feature (vertical slices inside the Clean rings).
* Many read endpoints: add a separate query side (CQRS) that reads directly from the database without going through the domain.
