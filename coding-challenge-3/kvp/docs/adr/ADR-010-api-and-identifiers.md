# ADR-010: API style, idempotency keys and identifiers: REST + problem+json, Idempotency-Key in Postgres, UUIDv7

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-01 (`POST /rides`, `Idempotency-Key`, validation), AC-05 (offer endpoints), AC-08
* **Related:** [architecture.md §6](../architecture.md#6-public-api), [ADR-008](ADR-008-data-stores.md)

## Part 1: API style (client → system)

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **REST + JSON over HTTP/1.1** | The challenge's example endpoints are REST; works with curl and Postman; easy to log and debug | Weaker typing (no schema in code) | ✅ **Chosen** |
| gRPC (protobuf) | Strong contracts, streaming, fast | Needs codegen and tooling for clients; poor curl/Postman story; gateway transcoding needed for browsers | ❌ For the public API; a candidate for internal sync calls |
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
| **Polling `GET /drivers/{id}/offer`** | Zero infrastructure; testable with curl and Postman | Latency = poll interval; wasted requests | ✅ **Chosen** (real-time push is out of scope) |
| WebSocket | Real-time, bidirectional | Connection management, sticky sessions | Future |
| Server-Sent Events | Real-time, simple, HTTP | One-way; proxies may buffer | Future (simplest push) |
| Mobile push (FCM / APNs) | Works when the app is backgrounded | Needs credentials and devices | Production |
| MQTT | Good on mobile networks | Extra broker | ❌ |

## Part 5: versioning

* Messages: the topic name carries the version (`ride.events.v1`) and every envelope has `schemaVersion`.
* HTTP: unversioned paths, as in the challenge's examples. A breaking change would add `/v2/...` or a media type version.
* Schemas: plain JSON with Go structs. A schema registry (Avro/Protobuf with Redpanda's built-in registry) is the next step when more teams consume the topics.
