// Package testpg starts a throwaway Postgres (testcontainers) for integration
// tests and applies migrations. Used only from *_integration_test.go files.
package testpg

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"ridehail/pkg/broker"
	"ridehail/pkg/pgutil"
)

// Main runs the package's tests against a fresh Postgres with migrations applied.
// It sets *pool before m.Run and terminates the container afterwards.
func Main(m *testing.M, pool **pgxpool.Pool, migrations ...fs.FS) {
	ctx := context.Background()
	ctr, err := tcpg.Run(ctx, "postgres:18-alpine",
		tcpg.WithDatabase("test"), tcpg.WithUsername("test"), tcpg.WithPassword("test"),
		tcpg.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres container:", err)
		os.Exit(1)
	}
	code := func() int {
		defer testcontainers.TerminateContainer(ctr) //nolint:errcheck
		url, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		p, err := pgutil.Connect(cctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer p.Close()
		for _, mig := range append([]fs.FS{broker.Migrations}, migrations...) {
			if err := pgutil.Migrate(ctx, p, mig); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		*pool = p
		return m.Run()
	}()
	os.Exit(code)
}
