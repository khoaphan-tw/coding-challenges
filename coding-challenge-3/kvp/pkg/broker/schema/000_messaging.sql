-- Transactional outbox: written in the same transaction as the state change.
CREATE TABLE IF NOT EXISTS outbox (
    id           bigserial PRIMARY KEY,
    topic        text        NOT NULL,
    msg_key      text        NOT NULL,
    event_id     uuid        NOT NULL UNIQUE,
    type         text        NOT NULL,
    envelope     jsonb       NOT NULL,
    headers      jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- Inbox: one row per processed message per consumer (idempotent consumer).
CREATE TABLE IF NOT EXISTS inbox (
    consumer     text        NOT NULL,
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
