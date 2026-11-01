// Package redis is dispatch-service's driven adapter for the Redis GEO
// projection of driver availability and location (ADR-001). It implements the
// use-case ports CandidateSource and GeoIndex. It is a disposable read model:
// the truth is driver_db, and the index can be rebuilt at any time.
package redis

import (
	"context"
	"fmt"
	"strconv"

	goredis "github.com/redis/go-redis/v9"

	"ridehail/pkg/geo"
	"ridehail/services/dispatch/internal/domain"
	"ridehail/services/dispatch/internal/usecase"
)

// GeoKey holds only AVAILABLE drivers.
const GeoKey = "geo:drivers:available"

func hashKey(id string) string { return "driver:" + id }

// applyScript updates the driver hash and the GEO set only when the event is
// newer than what is stored, so duplicate and out-of-order events are ignored.
// KEYS[1]=driver hash, KEYS[2]=geo set; ARGV: version, status, lat, lng, id.
var applyScript = goredis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'version')
if cur and tonumber(cur) >= tonumber(ARGV[1]) then return 0 end
redis.call('HSET', KEYS[1], 'version', ARGV[1], 'status', ARGV[2], 'lat', ARGV[3], 'lng', ARGV[4])
if ARGV[2] == 'AVAILABLE' then
  redis.call('GEOADD', KEYS[2], ARGV[4], ARGV[3], ARGV[5])
else
  redis.call('ZREM', KEYS[2], ARGV[5])
end
return 1`)

// Index reads and writes the projection.
type Index struct {
	R *goredis.Client
}

var (
	_ usecase.CandidateSource = (*Index)(nil)
	_ usecase.GeoIndex        = (*Index)(nil)
)

// Apply records a driver's status/location at version. It returns false when
// the event was older than (or equal to) the stored version.
func (ix *Index) Apply(ctx context.Context, driverID, status string, p geo.Point, version int64) (bool, error) {
	n, err := applyScript.Run(ctx, ix.R, []string{hashKey(driverID), GeoKey},
		version, status, strconv.FormatFloat(p.Latitude, 'f', -1, 64),
		strconv.FormatFloat(p.Longitude, 'f', -1, 64), driverID).Int()
	return n == 1, err
}

// Size returns the number of AVAILABLE drivers in the index.
func (ix *Index) Size(ctx context.Context) (int64, error) { return ix.R.ZCard(ctx, GeoKey).Result() }

// Candidates returns up to count AVAILABLE drivers within radiusM, nearest
// first, with exact coordinates and status from the driver hash.
func (ix *Index) Candidates(ctx context.Context, pickup geo.Point, radiusM float64, count int) ([]domain.Candidate, error) {
	locs, err := ix.R.GeoSearchLocation(ctx, GeoKey, &goredis.GeoSearchLocationQuery{
		GeoSearchQuery: goredis.GeoSearchQuery{
			Longitude: pickup.Longitude, Latitude: pickup.Latitude,
			Radius: radiusM + 1, RadiusUnit: "m", // +1 m: GEO hashes are ~0.6 m precise; match re-checks exactly
			Sort: "ASC", Count: count,
		},
		WithCoord: true,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("geosearch: %w", err)
	}
	if len(locs) == 0 {
		return nil, nil
	}
	pipe := ix.R.Pipeline()
	cmds := make([]*goredis.SliceCmd, len(locs))
	for i, l := range locs {
		cmds[i] = pipe.HMGet(ctx, hashKey(l.Name), "status", "lat", "lng")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("driver hashes: %w", err)
	}
	out := make([]domain.Candidate, 0, len(locs))
	for i, l := range locs {
		c := domain.Candidate{DriverID: l.Name, Location: geo.Point{Latitude: l.Latitude, Longitude: l.Longitude}, Status: "AVAILABLE"}
		v := cmds[i].Val()
		if s, ok := v[0].(string); ok {
			c.Status = s
		}
		lat, e1 := strconv.ParseFloat(str(v[1]), 64)
		lng, e2 := strconv.ParseFloat(str(v[2]), 64)
		if e1 == nil && e2 == nil {
			c.Location = geo.Point{Latitude: lat, Longitude: lng}
		}
		out = append(out, c)
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
