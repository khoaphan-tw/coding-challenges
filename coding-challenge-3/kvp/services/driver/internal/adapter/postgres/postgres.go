// Package postgres is driver-service's driven adapter: DriverRepository with
// pgx. Status changes are single conditional UPDATE statements; Postgres row
// locking makes each check-and-set atomic (ADR-002, AC-09).
package postgres

import (
	"context"
	"embed"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/geo"
	"ridehail/pkg/pgutil"
	"ridehail/services/driver/internal/domain"
	"ridehail/services/driver/internal/usecase"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is driver-service's schema.
var Migrations, _ = fs.Sub(migrationsFS, "migrations")

// Drivers implements usecase.DriverRepository.
type Drivers struct {
	Pool *pgxpool.Pool
}

var _ usecase.DriverRepository = (*Drivers)(nil)

const cols = `id, status, lat, lng, current_ride_id, version, location_updated_at, updated_at`

func scan(row pgx.Row) (domain.Driver, error) {
	var d domain.Driver
	err := row.Scan(&d.ID, &d.Status, &d.Latitude, &d.Longitude, &d.CurrentRideID, &d.Version, &d.LocationUpdatedAt, &d.UpdatedAt)
	return d, err
}

// casResult maps "no row matched" to applied=false.
func casResult(d domain.Driver, err error) (domain.Driver, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}

func (r *Drivers) db(ctx context.Context) pgutil.DB { return pgutil.Conn(ctx, r.Pool) }

// Get returns one driver.
func (r *Drivers) Get(ctx context.Context, id string) (domain.Driver, error) {
	d, err := scan(r.db(ctx).QueryRow(ctx, `SELECT `+cols+` FROM drivers WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, usecase.ErrDriverNotFound
	}
	return d, err
}

// List returns drivers, optionally filtered by status.
func (r *Drivers) List(ctx context.Context, status string) ([]domain.Driver, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT `+cols+` FROM drivers WHERE ($1 = '' OR status = $1) ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Driver, error) { return scan(row) })
}

// SetStatusIf: from → to only when the current status is from.
func (r *Drivers) SetStatusIf(ctx context.Context, id string, from, to domain.Status) (domain.Driver, bool, error) {
	return casResult(scan(r.db(ctx).QueryRow(ctx, `UPDATE drivers SET status=$2, version=version+1, updated_at=now()
		WHERE id=$1 AND status=$3 RETURNING `+cols, id, to, from)))
}

// ReserveIfAvailable: AVAILABLE → OFFERED for rideID. Of two concurrent calls,
// the second waits for the first's row lock, re-checks the WHERE clause and
// matches nothing.
func (r *Drivers) ReserveIfAvailable(ctx context.Context, id, rideID string) (domain.Driver, bool, error) {
	return casResult(scan(r.db(ctx).QueryRow(ctx, `UPDATE drivers SET status='OFFERED', current_ride_id=$2,
		version=version+1, updated_at=now() WHERE id=$1 AND status='AVAILABLE' RETURNING `+cols, id, rideID)))
}

// MarkBusyIfOffered: OFFERED → BUSY while holding rideID.
func (r *Drivers) MarkBusyIfOffered(ctx context.Context, id, rideID string) (domain.Driver, bool, error) {
	return casResult(scan(r.db(ctx).QueryRow(ctx, `UPDATE drivers SET status='BUSY', version=version+1, updated_at=now()
		WHERE id=$1 AND status='OFFERED' AND current_ride_id=$2 RETURNING `+cols, id, rideID)))
}

// ReleaseIfHeld: OFFERED|BUSY → AVAILABLE while holding rideID.
func (r *Drivers) ReleaseIfHeld(ctx context.Context, id, rideID string) (domain.Driver, bool, error) {
	return casResult(scan(r.db(ctx).QueryRow(ctx, `UPDATE drivers SET status='AVAILABLE', current_ride_id=NULL,
		version=version+1, updated_at=now() WHERE id=$1 AND status IN ('OFFERED','BUSY') AND current_ride_id=$2
		RETURNING `+cols, id, rideID)))
}

// UpdateLocation stores the latest location.
func (r *Drivers) UpdateLocation(ctx context.Context, id string, p geo.Point) (domain.Driver, error) {
	d, err := scan(r.db(ctx).QueryRow(ctx, `UPDATE drivers SET lat=$2, lng=$3, version=version+1,
		location_updated_at=now(), updated_at=now() WHERE id=$1 RETURNING `+cols, id, p.Latitude, p.Longitude))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, usecase.ErrDriverNotFound
	}
	return d, err
}

// Upsert creates or resets an OFFLINE/AVAILABLE driver (fixture).
func (r *Drivers) Upsert(ctx context.Context, id string, status domain.Status, p geo.Point) (domain.Driver, bool, error) {
	return casResult(scan(r.db(ctx).QueryRow(ctx, `INSERT INTO drivers(id, status, lat, lng, location_updated_at)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, lat=EXCLUDED.lat, lng=EXCLUDED.lng,
		  version=drivers.version+1, location_updated_at=now(), updated_at=now()
		  WHERE drivers.status IN ('OFFLINE','AVAILABLE')
		RETURNING `+cols, id, status, p.Latitude, p.Longitude)))
}

// Ready checks the database.
func (r *Drivers) Ready(ctx context.Context) error { return r.Pool.Ping(ctx) }
