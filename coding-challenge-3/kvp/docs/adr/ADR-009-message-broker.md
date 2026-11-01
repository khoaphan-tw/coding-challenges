# ADR-009: Message broker: Redpanda (Kafka API)

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-10 (event-driven dispatch); team constraint T5 ("Kafka or Redpanda")
* **Related:** [ADR-004 reliable messaging](ADR-004-messaging-outbox-inbox.md) (how we use it)

## Context

The saga sends commands, replies and events between three services. We need **per-key ordering** (all messages about one ride in order), **durable retention** (a stopped service catches up after restart), **consumer groups** (instances scale out), **replay** (projections, DLQ) and a **small local footprint** (Colima VM).

## Options compared

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Redpanda** | Kafka API (same client code); one C++ binary, no JVM or ZooKeeper; starts in seconds; laptop dev mode; `rpk` CLI and Redpanda Console | Smaller community than Kafka | ✅ **Chosen** |
| Apache Kafka (KRaft) | The standard; largest ecosystem | Needs a JVM heap; heavier on a laptop | Same API: switching is a `KAFKA_BROKERS` change |
| RabbitMQ | Simple queues; delayed messages | No replayable, partitioned log | ❌ Weaker fit for event replay |

**Offer timeout:** neither Kafka nor Redpanda has delayed messages. The 30 s timeout must survive restarts and be cancelled when the driver answers, so it lives in Postgres (`offers.expires_at` + a sweeper, [ADR-003](ADR-003-saga-orchestration.md)).

## Decision

* **Redpanda v26.2** (`redpandadata/redpanda:v26.2.6`), single node in dev-container mode, plus Redpanda Console v3 on `:8088`.
* Topics: `ride.events.v1`, `driver.events.v1`, `ride.commands.v1`, `driver.commands.v1`, `dispatch.replies.v1` (6 partitions each), plus a `.dlq` per topic. Each service creates them on startup with `kadm`.
* Keys: `rideId` for ride topics and replies, `driverId` for driver topics.
* Client: franz-go (idempotent producer, manual commits, `BlockRebalanceOnPoll`, 10 s session timeout; [ADR-007](ADR-007-language-and-libraries.md)).

## Consequences

* ✅ Kafka semantics (ordering, replay, consumer groups) with a laptop-friendly footprint; portable to any Kafka-compatible service.
* ⚠️ Single node locally (replication factor 1); production needs 3+ nodes with RF=3.

## When to revisit

* The organisation standardises on managed Kafka: change `KAFKA_BROKERS` (no code change).
