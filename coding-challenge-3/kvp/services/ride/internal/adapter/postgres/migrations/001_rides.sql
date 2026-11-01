CREATE TABLE passengers (
    id   text PRIMARY KEY,
    name text NOT NULL
);

-- Seeded passengers (registration is out of scope).
INSERT INTO passengers (id, name)
SELECT 'P' || lpad(i::text, 3, '0'), 'Passenger ' || i FROM generate_series(1, 10) AS i;

CREATE TABLE rides (
    id                 uuid PRIMARY KEY,
    passenger_id       text NOT NULL REFERENCES passengers (id),
    pickup_lat         double precision NOT NULL CHECK (pickup_lat BETWEEN -90 AND 90),
    pickup_lng         double precision NOT NULL CHECK (pickup_lng BETWEEN -180 AND 180),
    dest_lat           double precision NOT NULL CHECK (dest_lat BETWEEN -90 AND 90),
    dest_lng           double precision NOT NULL CHECK (dest_lng BETWEEN -180 AND 180),
    status             text NOT NULL CHECK (status IN
                         ('REQUESTED','MATCHING','DRIVER_ASSIGNED','DRIVER_ACCEPTED','CANCELLED','COMPLETED')),
    assigned_driver_id text,
    current_offer_id   uuid,
    version            bigint NOT NULL DEFAULT 1,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Durable status history: one row per transition.
CREATE TABLE ride_transitions (
    id          bigserial PRIMARY KEY,
    ride_id     uuid NOT NULL REFERENCES rides (id),
    from_status text,
    to_status   text NOT NULL,
    action      text NOT NULL,
    driver_id   text,
    offer_id    uuid,
    cause       text,            -- event id or "http:<method> <path>"
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ride_transitions_ride ON ride_transitions (ride_id, id);

-- Idempotency-Key for POST /rides (AC-01, AC-08).
CREATE TABLE idempotency_keys (
    key          text PRIMARY KEY,
    request_hash text NOT NULL,
    ride_id      uuid NOT NULL,
    status_code  int  NOT NULL,
    response     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
