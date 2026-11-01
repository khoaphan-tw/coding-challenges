# ADR-009: Message broker: Redpanda (Kafka API)

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-10 (event-driven dispatch); team constraint T5 ("Kafka or Redpanda")
* **Related:** [ADR-004 reliable messaging](ADR-004-messaging-outbox-inbox.md) (how we use it)

## Context

The saga sends commands, replies and events between three services. We need:

* **per-key ordering**: all messages about one ride (or one driver) in order;
* **durable retention**, so a stopped service catches up after restart;
* **consumer groups**, so instances can scale out;
* **replay**, for rebuilding projections and replaying DLQs;
* a **small local footprint** (Colima VM).

## Options compared

| Criterion | **Redpanda** | Apache Kafka (KRaft) | RabbitMQ (quorum queues / streams) | NATS JetStream | Redis Streams | Apache Pulsar | AWS SQS/SNS · GCP Pub/Sub · Azure Service Bus | In-memory bus |
|---|---|---|---|---|---|---|---|---|
| Model | Partitioned log | Partitioned log | Queues (+ streams plugin) | Streams + consumers | Append-only stream per key | Segmented log + queues | Managed queues/topics | Go channels |
| Per-key ordering | ✅ by partition key | ✅ | ⚠️ per queue (single active consumer / consistent-hash exchange) | ✅ per subject | ✅ per stream | ✅ key-shared | ⚠️ FIFO variants only | ✅ |
| Replay / retention | ✅ | ✅ | ⚠️ streams only | ✅ | ✅ (trimmed) | ✅ tiered | ❌ (queues) | ❌ |
| Consumer groups + rebalancing | ✅ Kafka protocol | ✅ | ✅ competing consumers | ✅ | ✅ (manual claim of pending entries) | ✅ | ✅ | ❌ |
| Delayed messages (offer timeout) | ❌ | ❌ | ✅ (plugin / TTL + DLX) | ⚠️ via redelivery delay (`NakWithDelay`) | ❌ | ✅ native | ✅ SQS delay ≤ 15 min | ❌ |
| Local footprint | **1 container, no JVM/ZooKeeper; runs with `--smp 1 --memory 1G`** | 1 container (KRaft, JVM ~0.5–1 GB) | 1 container (Erlang) | 1 tiny binary | Already have Redis | Several components (broker, bookie, ZK/metadata) | Cloud only (LocalStack emulation) | none |
| Ecosystem / tooling | Kafka API (franz-go, Kafka Connect, Debezium) + Redpanda Console + `rpk` | Largest | Large | Growing | Redis clients | Smaller | Managed | — |
| Durability across crashes | ✅ Raft-replicated | ✅ | ✅ quorum queues | ✅ | ⚠️ depends on AOF fsync | ✅ | ✅ | ❌ |
| Fit with team constraint T5 | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Verdict | ✅ **Chosen** | Same API, heavier locally | Good for task queues; weaker for replayable event logs | Lightest real option; not in T5 | Would reuse Redis, but mixes the read model with the durable log | Overkill | Not offline | Allowed by the challenge, but doesn't prove durability or redelivery |

**Redpanda vs Apache Kafka:** both speak the Kafka protocol, so the code (franz-go) is identical. Redpanda is a single C++ binary with Raft built in (no ZooKeeper, no JVM), starts in seconds, and has a dev mode (`--mode dev-container`) tuned for laptops. It also ships `rpk` (used by `make replay-dlq`) and Redpanda Console. Kafka 4.x with KRaft has also dropped ZooKeeper, but still needs a JVM heap. Switching to Kafka or a managed Kafka (Confluent Cloud, MSK, Aiven) is a configuration change: `KAFKA_BROKERS`.

**Why not a broker with delayed messages for the offer timeout?** Delays would be handy, but the 30 s timeout must survive restarts and be cancelled when the driver answers. A Postgres sweeper on `offers.expires_at` does both ([ADR-003](ADR-003-saga-orchestration.md)). An alternative is a Redis ZSET delay queue.

## Decision

* **Redpanda v26.2** (`redpandadata/redpanda:v26.2.6`), single node in dev-container mode, plus Redpanda Console v3 on `:8088`.
* Topics: `ride.events.v1`, `driver.events.v1`, `ride.commands.v1`, `driver.commands.v1`, `dispatch.replies.v1` (6 partitions each), plus a `.dlq` per topic. Each service creates them on startup with `kadm`.
* Keys: `rideId` for ride topics and replies, `driverId` for driver topics.
* Client: franz-go (idempotent producer, manual commits, `BlockRebalanceOnPoll`, 10 s session timeout; [ADR-007](ADR-007-language-and-libraries.md)).

## Consequences

* ✅ Kafka semantics (ordering, replay, consumer groups) with a laptop-friendly footprint; portable to any Kafka-compatible service.
* ⚠️ Single node locally (replication factor 1): no broker fault tolerance in dev. Production uses 3+ nodes with RF=3.
* ⚠️ No native delayed delivery, so timers live in Postgres.

## When to revisit

* Organisation standardises on managed Kafka: change `KAFKA_BROKERS` (no code change).
* Need for many short-lived queues or per-message delays: consider NATS JetStream or RabbitMQ alongside.
