package usecase

import (
	"context"
	"log/slog"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
)

// Projection keeps the GEO index of AVAILABLE drivers in sync with
// driver-service (ADR-001): a CQRS-style read model, rebuildable at any time.
type Projection struct {
	Index     GeoIndex
	Directory DriverDirectory
	// OnAvailable is called when a driver becomes AVAILABLE (wakes waiting sagas).
	OnAvailable func(ctx context.Context)
}

// DriverEvent is a driver.events.v1 message.
type DriverEvent struct {
	Type     string // DriverStatusChanged or DriverLocationUpdated
	DriverID string
	Status   string
	Location geo.Point
	Version  int64
}

// Apply projects one driver event. The index ignores versions it has already
// seen, so duplicates and out-of-order events are harmless (no inbox needed).
func (p *Projection) Apply(ctx context.Context, ev DriverEvent) error {
	if ev.Type != messaging.DriverStatusChanged && ev.Type != messaging.DriverLocationUpdated {
		return nil
	}
	applied, err := p.Index.Apply(ctx, ev.DriverID, ev.Status, ev.Location, ev.Version)
	if err != nil {
		return err
	}
	if !applied {
		slog.DebugContext(ctx, "older driver event ignored by geo index", "version", ev.Version)
		return nil
	}
	if ev.Type == messaging.DriverStatusChanged {
		slog.InfoContext(ctx, "geo index updated", "status", ev.Status, "version", ev.Version)
		if ev.Status == "AVAILABLE" && p.OnAvailable != nil {
			p.OnAvailable(ctx)
		}
	}
	return nil
}

// Rebuild loads every driver from driver-service and applies it.
func (p *Projection) Rebuild(ctx context.Context) (int, error) {
	drivers, err := p.Directory.ListDrivers(ctx)
	if err != nil {
		return 0, err
	}
	for _, d := range drivers {
		if _, err := p.Index.Apply(ctx, d.ID, d.Status, d.Location, d.Version); err != nil {
			return 0, err
		}
	}
	slog.InfoContext(ctx, "geo index rebuilt from driver-service", "drivers", len(drivers))
	return len(drivers), nil
}

// RebuildIfEmpty rebuilds when the index has no drivers (first start, Redis loss).
func (p *Projection) RebuildIfEmpty(ctx context.Context) error {
	if n, err := p.Index.Size(ctx); err == nil && n > 0 {
		return nil
	}
	_, err := p.Rebuild(ctx)
	return err
}
