//go:build integration

package redis

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"ridehail/pkg/geo"
)

func TestIndexKeepsOnlyLatestAvailableDrivers(t *testing.T) {
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "redis:8-alpine", ExposedPorts: []string{"6379/tcp"},
			WaitingFor: wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer testcontainers.TerminateContainer(ctr) //nolint:errcheck
	addr, _ := ctr.PortEndpoint(ctx, "6379/tcp", "")
	ix := &Index{R: goredis.NewClient(&goredis.Options{Addr: addr})}

	pickup := geo.Point{Latitude: 10.7769, Longitude: 106.7009}
	near := geo.Point{Latitude: 10.7790, Longitude: 106.7009} // ~230 m
	far := geo.Point{Latitude: 10.7900, Longitude: 106.7009}  // ~1.5 km
	out := geo.Point{Latitude: 10.8100, Longitude: 106.7009}  // ~3.7 km

	must := func(applied bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}
	must(ix.Apply(ctx, "A", "AVAILABLE", near, 1))
	must(ix.Apply(ctx, "B", "AVAILABLE", far, 1))
	must(ix.Apply(ctx, "C", "AVAILABLE", out, 1))
	must(ix.Apply(ctx, "D", "OFFLINE", near, 1))

	c, err := ix.Candidates(ctx, pickup, 2000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 2 || c[0].DriverID != "A" || c[1].DriverID != "B" {
		t.Fatalf("want [A B] nearest first, got %+v", c)
	}

	// A becomes OFFERED (v2): leaves the index.
	must(ix.Apply(ctx, "A", "OFFERED", near, 2))
	// A late, older AVAILABLE event (v1 duplicate) must not bring A back.
	if must(ix.Apply(ctx, "A", "AVAILABLE", near, 1)) {
		t.Fatal("older event must be ignored")
	}
	c, _ = ix.Candidates(ctx, pickup, 2000, 10)
	if len(c) != 1 || c[0].DriverID != "B" {
		t.Fatalf("want [B], got %+v", c)
	}

	// Latest location wins: B moves next to the pickup.
	must(ix.Apply(ctx, "B", "AVAILABLE", geo.Point{Latitude: 10.7770, Longitude: 106.7009}, 2))
	c, _ = ix.Candidates(ctx, pickup, 2000, 10)
	if len(c) != 1 || geo.HaversineM(pickup, c[0].Location) > 20 {
		t.Fatalf("matching must use the latest location, got %+v", c)
	}
}
