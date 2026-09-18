// Package pgsandbox hands a Go test its own migrated Postgres database.
//
// One Postgres container per major version is started through testcontainers and reused by
// every test process and every later run. Inside it, the migrations for a given set are run once
// into a blueprint database; each test then gets a clone of that blueprint, which is a file copy,
// and the clone is dropped when the test ends. Tests can run in parallel.
package pgsandbox

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const setupTimeout = 2 * time.Minute

// DB is a test's private database.
type DB struct {
	// Pool is connected to the database and closed when the test ends.
	Pool *pgxpool.Pool
	// DSN is a postgres:// URL for anything that opens its own connection.
	DSN string
	// Name is the database name inside the shared container.
	Name string
}

// New returns a database for this test on Postgres <major>, migrated as the options describe,
// and drops it in t.Cleanup. It fails the test on any error.
func New(tb testing.TB, major int, opts ...Option) *DB {
	tb.Helper()

	cfg, err := newSettings(major, opts...)
	if err != nil {
		tb.Fatalf("pgsandbox: %v", err)
	}

	// Not t.Context: the container and the blueprint outlive this test.
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	srv, err := startServer(ctx, major)
	if err != nil {
		tb.Fatalf("pgsandbox: %v", err)
	}

	name, err := cloneFromBlueprint(ctx, srv, cfg)
	if err != nil {
		tb.Fatalf("pgsandbox: %v", err)
	}
	dsn := srv.dsn(name)

	tb.Cleanup(func() {
		if cfg.keep && tb.Failed() {
			tb.Logf("pgsandbox: kept database %s for inspection: %s", name, dsn)
			return
		}
		if err := dropDatabase(context.Background(), srv.admin, name); err != nil {
			tb.Logf("pgsandbox: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tb.Fatalf("pgsandbox: connect to %s: %v", name, err)
	}
	tb.Cleanup(pool.Close)

	return &DB{Pool: pool, DSN: dsn, Name: name}
}
