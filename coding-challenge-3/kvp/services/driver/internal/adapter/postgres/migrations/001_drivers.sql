CREATE TABLE drivers (
    id                  text PRIMARY KEY,
    status              text NOT NULL CHECK (status IN ('OFFLINE','AVAILABLE','OFFERED','BUSY')),
    lat                 double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng                 double precision NOT NULL CHECK (lng BETWEEN -180 AND 180),
    current_ride_id     uuid,
    -- OFFERED/BUSY always hold a ride; OFFLINE/AVAILABLE never do.
    CONSTRAINT ride_held_iff_engaged CHECK ((status IN ('OFFERED','BUSY')) = (current_ride_id IS NOT NULL)),
    version             bigint NOT NULL DEFAULT 1,
    location_updated_at timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Seeded drivers around Ben Thanh / District 1, Ho Chi Minh City.
-- D001..D010 are AVAILABLE, ~220 m apart going north (D010 is ~2 km away),
-- D011..D020 are OFFLINE. Registration is out of scope.
INSERT INTO drivers (id, status, lat, lng, location_updated_at)
SELECT 'D' || lpad(i::text, 3, '0'),
       CASE WHEN i <= 10 THEN 'AVAILABLE' ELSE 'OFFLINE' END,
       10.7769 + 0.002 * ((i - 1) % 10 + 1),
       106.7009,
       now()
FROM generate_series(1, 20) AS i;
