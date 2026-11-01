// Package pgutil connects to Postgres, runs embedded SQL migrations and
// implements the use cases' transaction port (TxManager, transaction in ctx).
package pgutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool and waits (up to ~60 s) for the database to accept connections.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		if err = pool.Ping(ctx); err == nil {
			return pool, nil
		}
		if i >= 60 {
			pool.Close()
			return nil, fmt.Errorf("postgres not reachable: %w", err)
		}
		slog.WarnContext(ctx, "waiting for postgres", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Migrate applies every *.sql file in fsys (sorted by name) that has not been
// applied yet. An advisory lock makes concurrent replicas safe.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7262001)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7262001)`) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "migration applied", "migration", name)
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique-constraint violation.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ---- transaction in context (used by Clean Architecture repositories) ----

// DB is what repositories need: a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// TxManager implements the use cases' transaction port: it runs fn in one
// Postgres transaction and passes it through ctx, so every repository and the
// outbox called inside fn share it. Nested calls reuse the outer transaction.
type TxManager struct {
	Pool *pgxpool.Pool
}

// WithinTx runs fn inside a transaction (commit on nil error, rollback otherwise).
func (m TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}
	return pgx.BeginFunc(ctx, m.Pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// Conn returns the transaction in ctx, or the pool when there is none.
func Conn(ctx context.Context, pool *pgxpool.Pool) DB {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return pool
}

// Tx returns the transaction in ctx, or an error: for writes that must be
// atomic with other writes (outbox, inbox).
func Tx(ctx context.Context) (pgx.Tx, error) {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx, nil
	}
	return nil, errors.New("pgutil: no transaction in context (call inside TxManager.WithinTx)")
}
