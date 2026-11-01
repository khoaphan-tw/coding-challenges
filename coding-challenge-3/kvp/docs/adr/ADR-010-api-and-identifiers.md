# ADR-010: API style, documentation, idempotency keys and identifiers: REST + problem+json (v1, OpenAPI), gRPC streams for two driver APIs (v2), Idempotency-Key in Postgres, UUIDv7

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-01 (`POST /rides`, `Idempotency-Key`, validation), AC-05 (offer endpoints), AC-08
* **Related:** [architecture.md §6](../architecture.md#6-public-api), [ADR-008](ADR-008-data-stores.md)

## Part 1: API style and errors

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **REST + JSON** | The challenge's example endpoints are REST; works with curl and Postman; easy to log and debug | No schema in code (covered by the OpenAPI spec, Part 7) | ✅ **Chosen** (v1) |
| gRPC (protobuf) | Typed contracts, streaming | Needs generated clients; poor curl/Postman story | ✅ Only for the two driver-app streams (v2, Part 6) |

**Errors:** RFC 9457 `application/problem+json` with a stable machine-readable `code` (`INVALID_COORDINATES`, `IDEMPOTENCY_KEY_REUSED`, `INVALID_TRANSITION`, `OFFER_NOT_CURRENT`, `OFFER_EXPIRED`, …): a standard shape that tests and clients can rely on.

**Status codes:** `201` on create; `200` on transitions, including idempotent repeats with the `Idempotent-Replayed: true` header; `400` for malformed input or a missing key; `422` for semantic validation (coordinates, unknown passenger, key reused); `404` for unknown resources; `409` for state conflicts (invalid transition, offer no longer current or expired).

## Part 2: Idempotency-Key design (AC-01, AC-08)

| Decision point | Chosen | Why |
|---|---|---|
| Where the key travels | **`Idempotency-Key` header** (required) | The challenge specifies it; follows the IETF draft and common payment-API practice |
| Where it is stored | **Postgres `idempotency_keys`, same transaction as the ride** | Atomic with the ride; concurrent duplicates serialize on the primary key ([ADR-008](ADR-008-data-stores.md)) |
| What is stored | **key + request hash + status + full response body** | A replay returns exactly the original result (same `rideId`, `createdAt`, …) |
| Same key, different body | **Reject `422 IDEMPOTENCY_KEY_REUSED`** | AC-01 requires it; silently returning another ride would hide client bugs |
| Request fingerprint | **SHA-256 of the normalised fields** (passenger, pickup, destination) | Whitespace or key-order changes are not a different request |
| Concurrent duplicates | **Insert the key first** (`ON CONFLICT DO NOTHING`); the second request waits on the primary key, then replays | No race window; tested with 10 parallel requests |

Other mutations are **naturally idempotent** and need no key: accept, reject, cancel and complete are state transitions. A repeat finds the target state and returns `200` with the same body ([ADR-002](ADR-002-concurrency.md)).

## Part 3: identifiers

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **UUIDv7 (RFC 9562)** | Time-sortable (good B-tree locality); no coordination; hard to guess | 16 bytes | ✅ **Chosen**: rides, offers, event ids |
| UUIDv4 | Common, unguessable | Random inserts fragment indexes | ❌ |

Seeded records keep readable ids (`P001`, `D001`) as in the challenge's examples. Driver ids are `text`, so test fixtures can use `PM-A-…` or `E2E-…`.

## Part 4: how a driver receives an offer

* **v1: polling `GET /drivers/{id}/offer`.** Zero infrastructure, testable with curl and Postman; latency = the poll interval.
* **v2: gRPC server stream `OfferService/WatchOffers`.** One connection; offers pushed within ~0.5 s (Part 6).

Mobile push (FCM/APNs) for a backgrounded app is out of scope.

## Part 5: versioning

* Messages: the topic name carries the version (`ride.events.v1`) and every envelope has `schemaVersion`.
* HTTP (v1): unversioned paths, as in the challenge's examples. A breaking change would add `/v2/...`.
* gRPC (v2): the protobuf package carries the version (`ridehail.driver.v2`, `ridehail.dispatch.v2`); fields are only added, never renumbered.

## Part 6: gRPC API v2 for two driver-app APIs

**gRPC next to REST, for two APIs only.** gRPC's main advantage is **streaming over one long-lived connection**. That matters only for the two calls a driver app makes continuously; one-shot commands (create ride, accept, cancel) stay REST.

| API | Calls per driver | Decision |
|---|---|---|
| `PUT /drivers/{id}/location` | Every 4–5 s while online (250k/s at 1M drivers, [scalability](../scalability.md)) | **v2: `DriverLocationService/StreamLocations`** (bidirectional stream: one connection, an ack per update) |
| `GET /drivers/{id}/offer` (polling) | Every 1–2 s while waiting | **v2: `OfferService/WatchOffers`** (server stream: offers pushed, no empty requests) |
| `POST /rides`, accept / reject / cancel / complete | Once per action | REST only |

| v2 transport | Pros | Cons | Verdict |
|---|---|---|---|
| **gRPC streams (HTTP/2, protobuf)** | Typed `.proto` contract; bidirectional and server streaming; first-class Go and OpenTelemetry support | Code generation; browsers need gRPC-Web | ✅ **Chosen** |
| WebSocket + JSON | Works in browsers | No contract; hand-made framing, acks and reconnection | ❌ |

**How it is built**

* Contracts: [api/proto/ridehail/driver/v2/location.proto](../../api/proto/ridehail/driver/v2/location.proto), [api/proto/ridehail/dispatch/v2/offers.proto](../../api/proto/ridehail/dispatch/v2/offers.proto). Generated Go code is committed in `api/gen` (`make proto` regenerates it).
* Each service gets a new **driving adapter** `internal/adapter/grpc` that calls the *same use case* as the REST handler (Clean Architecture, [ADR-012](ADR-012-clean-architecture.md)). The architecture test treats gRPC as infrastructure that inner layers must not import.
* `pkg/grpcx` adds tracing (`otelgrpc`), a log line per stream and per message (with `trace_id`, `driver_id` and the message as JSON), the standard gRPC health service and server reflection.
* Gateway: nginx listens on `:9090` with HTTP/2 and routes by gRPC service name. Stream timeouts are raised to 1 h; `WatchOffers` sends a `HEARTBEAT` every 15 s so idle streams survive proxies.
* `WatchOffers` push: each open stream checks the driver's pending offer every 500 ms inside dispatch-service (one indexed read). The network polling of v1 is gone; Postgres `LISTEN/NOTIFY` or an offers topic would remove the in-service check at scale.
* Errors: `StreamLocations` reports a bad update **in its ack** (`accepted=false`, `error_code` with the REST codes) instead of failing the stream, so one bad GPS fix does not disconnect the driver.

## Part 7: API documentation: OpenAPI spec and Postman collection

**Both, with different jobs.**

* **OpenAPI 3.0 spec** ([api/openapi.yaml](../../api/openapi.yaml)): the source of truth for the REST contract (every endpoint, schema, header, status and error `code`). It is the industry standard: reviewable in a pull request, and imported by Swagger UI, Postman, client generators and validators.
* **Postman collection** ([postman/](../../postman/)): the acceptance scenarios (request order, waiting for asynchronous results, checks), which a spec cannot express.
* gRPC v2 is described by its `.proto` files.

**How the spec stays correct:**

* `make openapi-lint` runs Redocly's recommended rules ([api/redocly.yaml](../../api/redocly.yaml) lists the 3 relaxed rules and why).
* The e2e test `TestRESTResponsesMatchOpenAPI` calls every REST endpoint through the gateway and validates each status, header and body against the spec. A field added to the spec but not returned, a wrong status or an undocumented error code fails the build.
* Swagger UI is served by the gateway at **http://localhost:8080/docs/**; "Try it out" calls the running stack.

## Consequences

* ✅ The REST API matches the challenge's examples, and its contract is enforced by a test.
* ✅ The two highest-frequency driver calls use one connection each; offers arrive without polling.
* ⚠️ Two API styles to document and test (OpenAPI + proto files).
