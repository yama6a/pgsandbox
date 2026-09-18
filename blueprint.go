package pgsandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const sqlstateUnknownDatabase = "3D000"

// cloneFromBlueprint returns a fresh database holding the migrated schema. The blueprint for the
// settings' fingerprint is built by the first caller, under a server-side lock so parallel
// callers wait instead of racing to build it. Later callers, in this process or any other, clone.
func cloneFromBlueprint(ctx context.Context, srv *server, cfg *settings) (string, error) {
	blueprint := cfg.blueprint()
	clone := "sb_" + randomSuffix()

	// A session lock, not a transaction lock: CREATE DATABASE cannot run inside a transaction.
	lock, err := srv.admin.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire admin connection: %w", err)
	}
	defer lock.Release()

	lockKey := int64(crc32.ChecksumIEEE([]byte(blueprint)))
	if _, err := lock.Exec(ctx, "select pg_advisory_lock($1)", lockKey); err != nil {
		return "", fmt.Errorf("lock blueprint %s: %w", blueprint, err)
	}
	defer func() { _, _ = lock.Exec(context.WithoutCancel(ctx), "select pg_advisory_unlock($1)", lockKey) }()

	// Every statement runs on the locked connection: the pool may be fully held by callers
	// waiting on the same lock, and a second acquire here would wait for one of them forever.
	err = createDatabase(ctx, lock, clone, blueprint)
	if err == nil {
		return clone, nil
	}
	if !isUnknownDatabase(err) {
		return "", err
	}

	if err := buildBlueprint(ctx, lock, srv, cfg, clone, blueprint); err != nil {
		_ = dropDatabase(context.WithoutCancel(ctx), lock, clone)
		return "", err
	}
	return clone, nil
}

// buildBlueprint migrates a fresh database, then snapshots it as the blueprint. The clone is
// the one handed back to the caller, so the first test pays only for the migrations.
func buildBlueprint(ctx context.Context, admin executor, srv *server, cfg *settings, clone, blueprint string) error {
	if err := createDatabase(ctx, admin, clone, ""); err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, srv.dsn(clone))
	if err != nil {
		return fmt.Errorf("connect to %s: %w", clone, err)
	}
	if err := migrate(ctx, conn, cfg); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return err
	}
	// The snapshot needs the source free of connections.
	if err := conn.Close(ctx); err != nil {
		return fmt.Errorf("close migration connection: %w", err)
	}

	return createDatabase(ctx, admin, blueprint, clone)
}

func migrate(ctx context.Context, conn *pgx.Conn, cfg *settings) error {
	for _, s := range cfg.scripts {
		if _, err := conn.Exec(ctx, s.body); err != nil {
			return fmt.Errorf("migration %s: %w", s.name, err)
		}
	}
	for _, m := range cfg.migrators {
		if err := m.run(ctx, conn); err != nil {
			return fmt.Errorf("migrator %q: %w", m.key, err)
		}
	}
	return nil
}

// executor is what a pool and a single pooled connection have in common.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func createDatabase(ctx context.Context, admin executor, name, template string) error {
	stmt := "create database " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		stmt += " template " + pgx.Identifier{template}.Sanitize()
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	return nil
}

func dropDatabase(ctx context.Context, admin executor, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := admin.Exec(ctx, "select pg_terminate_backend(pid) from pg_stat_activity where datname = $1", name)
	if err != nil {
		return fmt.Errorf("disconnect clients of %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	return nil
}

func isUnknownDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateUnknownDatabase
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
