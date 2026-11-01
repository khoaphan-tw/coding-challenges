# ADR-010: API style, idempotency keys and identifiers: REST + problem+json (v1), gRPC streams for two driver APIs (v2), Idempotency-Key in Postgres, UUIDv7

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-01 (`POST /rides`, `Idempotency-Key`, validation), AC-05 (offer endpoints), AC-08
* **Related:** [architecture.md §6](../architecture.md#6-public-api), [ADR-008](ADR-008-data-stores.md)

## Part 1: API style (client → system)

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **REST + JSON over HTTP/1.1** | The challenge's example endpoints are REST; works with curl and Postman; easy to log and debug | Weaker typing (no schema in code) | ✅ **Chosen** |
| gRPC (protobuf) | Strong contracts, streaming, fast | Needs codegen and tooling for clients; poor curl/Postman story; gateway transcoding needed for browsers | ❌ As the *only* public API. ✅ As v2 for the two driver-app APIs that are streams by nature ([Part 6](#part-6-grpc-api-v2-for-two-driver-app-apis)) |
| GraphQL | Flexible reads | Mutations with idempotency and state machines gain nothing; extra server | ❌ |
| Connect-RPC / Twirp | gRPC-compatible, works over HTTP/JSON | Extra codegen | ❌ |

**Errors:**

| Option | Verdict |
|---|---|
| **RFC 9457 `application/problem+json` + a stable `code`** (`INVALID_COORDINATES`, `IDEMPOTENCY_KEY_REUSED`, `INVALID_TRANSITION`, `OFFER_NOT_CURRENT`, `OFFER_EXPIRED`, …) | ✅ Standard shape plus machine-readable codes for tests and clients |
| Ad-hoc `{ "error": "..." }` | ❌ No contract |
| Google `google.rpc.Status` style | ❌ gRPC-oriented |

**Status codes:** `201` on create; `200` on transitions, including idempotent repeats with the `Idempotent-Replayed: true` header; `400` for malformed input or a missing key; `422` for semantic validation (coordinates, unknown passenger, key reused); `404` for unknown resources; `409` for state conflicts (invalid transition, offer no longer current or expired).

## Part 2: Idempotency-Key design (AC-01, AC-08)

| Decision point | Options considered | Chosen | Why |
|---|---|---|---|
| Where the key travels | Header · body field · derived from the body hash | **`Idempotency-Key` header** (required) | The challenge specifies it; follows the IETF `Idempotency-Key` header draft and common payment-API practice |
| Where it is stored | Postgres (same transaction) · Redis `SET NX` · in-memory | **Postgres `idempotency_keys`, same transaction as the ride** | Atomic with the ride; concurrent duplicates serialize on the primary key ([ADR-008](ADR-008-data-stores.md)) |
| What is stored | Key only · key + ride id · key + full response | **key + request hash + status + full response body** | Replay returns exactly the original result (same `rideId`, `createdAt`, …) |
| Same key, different body | Return the original anyway · overwrite · reject | **Reject `422 IDEMPOTENCY_KEY_REUSED`** | AC-01 requires it; silently returning another ride hides client bugs |
| Request fingerprint | Raw-bytes hash · normalised fields hash | **SHA-256 of the normalised fields** (passenger, pickup, destination) | Whitespace or key-order changes are not a different request |
| Concurrent duplicates | Lock row · insert-first | **Insert the key first** (`ON CONFLICT DO NOTHING`); the second request blocks on the PK, then replays | No race window; tested with 10 parallel requests |
| Expiry | None · TTL | None yet (cleanup job documented as a limitation) | Out of scope |

Other mutations are **naturally idempotent** and need no key: accept, reject, cancel and complete are state transitions. A repeat finds the target state and returns `200` with the same body ([ADR-002](ADR-002-concurrency.md)).

## Part 3: identifiers

| Option | Sortable by time | Index locality | Coordination | Guessability | Verdict |
|---|---|---|---|---|---|
| `bigserial` | ✅ | ✅ | DB sequence (per database) | Enumerable; leaks volume | ❌ Not globally unique across services |
| UUIDv4 | ❌ | ❌ random inserts fragment B-tree indexes | None | Unguessable | ❌ |
| **UUIDv7 (RFC 9562)** | ✅ (ms timestamp prefix) | ✅ | None | Hard to guess (74 random bits) | ✅ **Chosen**: rides, offers, event ids |
| ULID | ✅ | ✅ | None | Hard to guess | Equivalent, but not the IETF standard; no native `uuid` column type |
| Snowflake / Sonyflake (64-bit) | ✅ | ✅ | Worker-id assignment | Enumerable-ish | ❌ Coordination for no benefit |
| KSUID | ✅ (second precision) | ✅ | None | Hard to guess | ❌ Less common |

Seeded records keep readable ids (`P001`, `D001`) as in the challenge's examples. Driver ids are `text`, so test fixtures can use `PM-A-…` or `E2E-…`.

## Part 4: how a driver receives an offer

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Polling `GET /drivers/{id}/offer`** | Zero infrastructure; testable with curl and Postman | Latency = poll interval; wasted requests | ✅ **v1** (simple, required by the challenge's REST style) |
| **gRPC server stream `OfferService/WatchOffers`** | One connection; offers pushed within ~0.5 s; typed contract; works on mobile with HTTP/2 | Needs a gRPC client; long-lived connections through the gateway | ✅ **v2** ([Part 6](#part-6-grpc-api-v2-for-two-driver-app-apis)) |
| WebSocket | Real-time, bidirectional | Connection management, sticky sessions | Future |
| Server-Sent Events | Real-time, simple, HTTP | One-way; proxies may buffer | Future (simplest push) |
| Mobile push (FCM / APNs) | Works when the app is backgrounded | Needs credentials and devices | Production |
| MQTT | Good on mobile networks | Extra broker | ❌ |

## Part 5: versioning

* Messages: the topic name carries the version (`ride.events.v1`) and every envelope has `schemaVersion`.
* HTTP (v1): unversioned paths, as in the challenge's examples. A breaking change would add `/v2/...` or a media type version.
* gRPC (v2): the protobuf package carries the version (`ridehail.driver.v2`, `ridehail.dispatch.v2`); fields are only added, never renumbered.
* Schemas: plain JSON with Go structs. A schema registry (Avro/Protobuf with Redpanda's built-in registry) is the next step when more teams consume the topics.

## Part 6: gRPC API v2 for two driver-app APIs

**Question:** should the API use gRPC instead of REST?

**Answer: not instead, but next to it, for two APIs only.** gRPC adds code generation, an HTTP/2-only gateway route and clients that curl and Postman's collection format cannot drive. That cost pays off only where gRPC's main advantage, **streaming over one long-lived connection**, matters. For one-shot commands (create ride, accept, cancel) REST is simpler and the challenge specifies it.

| API | Calls per driver | Fit for gRPC | Decision |
|---|---|---|---|
| `PUT /drivers/{id}/location` | Every 4–5 s while online (250k/s at 1M drivers, [scalability](../scalability.md)) | ✅ A stream is the natural shape: one connection, no per-update HTTP request/headers/TLS cost, per-update acks | **v2: `DriverLocationService/StreamLocations`** (bidirectional stream) |
| `GET /drivers/{id}/offer` (polling) | Every 1–2 s while waiting | ✅ Server push replaces polling: lower latency, no empty requests | **v2: `OfferService/WatchOffers`** (server stream) |
| `POST /rides` | Once per ride | ❌ One-shot; `Idempotency-Key` is an HTTP idiom | REST only |
| accept / reject / cancel / complete | Once per action | ❌ One-shot commands | REST only |
| Internal service-to-service | Almost all asynchronous via Redpanda; one rare sync call (GEO rebuild) | ❌ No hot sync path to optimise | Not needed |

**Options for the v2 transport:**

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **gRPC streams (HTTP/2, protobuf)** | Typed contract in `.proto`; bidirectional and server streaming; one connection; flow control; first-class Go support; OpenTelemetry instrumentation (`otelgrpc`) | Codegen; browsers need gRPC-Web; nginx needs a separate HTTP/2 listener | ✅ **Chosen** |
| WebSocket + JSON | Works in browsers; simple | No contract or codegen; hand-made framing, acks and reconnection | ❌ |
| Server-Sent Events | Simplest push over HTTP/1.1 | One direction only (no location upload) | Good for offers alone, not for locations |
| MQTT | Built for mobile telemetry | Another broker to run | ❌ |
| Connect-RPC | gRPC-compatible and also plain HTTP/JSON | Extra library; less common than gRPC | Valid alternative |

**How it is built**

* Contracts: [api/proto/ridehail/driver/v2/location.proto](../../api/proto/ridehail/driver/v2/location.proto), [api/proto/ridehail/dispatch/v2/offers.proto](../../api/proto/ridehail/dispatch/v2/offers.proto). Package names carry the version (`ridehail.driver.v2`). Generated Go code is committed in `api/gen` (`make proto` regenerates it).
* Each service gets a new **driving adapter** `internal/adapter/grpc` that calls the *same use case* as the REST handler (Clean Architecture, [ADR-012](ADR-012-clean-architecture.md)). The architecture test treats gRPC as infrastructure that inner layers must not import.
* `pkg/grpcx` adds tracing (`otelgrpc`), a log line per stream and per message (with `trace_id`, `driver_id` and the message as JSON, like HTTP bodies), the standard gRPC health service and server reflection.
* Gateway: nginx listens on `:9090` with HTTP/2 and routes by gRPC service name (`/ridehail.driver.v2.DriverLocationService/` → driver-service, `/ridehail.dispatch.v2.OfferService/` → dispatch-service). Stream timeouts are raised to 1 h; `WatchOffers` sends a `HEARTBEAT` every 15 s so idle streams survive proxies.
* `WatchOffers` push: each open stream checks the driver's pending offer every 500 ms inside dispatch-service (one indexed read). The network polling of v1 is gone; replacing the in-service check with Postgres `LISTEN/NOTIFY` or an offers topic is the next step at scale.
* Errors: `StreamLocations` reports a bad update **in its ack** (`accepted=false`, `error_code` with the REST codes) instead of failing the stream, so one bad GPS fix does not disconnect the driver. Request-level errors use gRPC status codes (`INVALID_ARGUMENT` for a missing `driver_id`).

**Consequences**

* ✅ The two highest-frequency driver calls use one connection each; offers arrive without polling.
* ✅ v1 stays unchanged; clients move per endpoint.
* ⚠️ Two API styles to document and test (proto files + OpenAPI). gRPC is covered by e2e tests and `grpcurl`, not by the Postman collection.
* ⚠️ Long-lived streams pin a driver to one service instance; scaling out needs connection-aware load balancing (L7 gRPC balancing in the gateway).
