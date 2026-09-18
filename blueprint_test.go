package pgsandbox

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloneFromBlueprint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	srv, err := startServer(ctx, testMajor)
	require.NoError(t, err)

	t.Run("parallel callers share one build", func(t *testing.T) {
		t.Parallel()
		var builds atomic.Int32
		key := "key-" + randomSuffix()
		cfg, err := newSettings(testMajor,
			Migrations(os.DirFS("testdata/migrations")),
			Migrate(key, func(context.Context, *pgx.Conn) error {
				builds.Add(1)
				return nil
			}),
		)
		require.NoError(t, err)

		const callers = 10
		names := make([]string, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				name, err := cloneFromBlueprint(ctx, srv, cfg)
				assert.NoError(t, err)
				names[i] = name
			})
		}
		wg.Wait()
		for _, name := range names {
			t.Cleanup(func() { assert.NoError(t, dropDatabase(ctx, srv.admin, name)) })
		}

		assert.Equal(t, int32(1), builds.Load())
		assert.Len(t, uniq(names), callers)
		assert.Equal(t, 1, countDatabases(ctx, t, srv, cfg.blueprint()))
		for _, name := range names {
			assert.Equal(t, 3, countColumns(ctx, t, srv, name, "shelves"))
		}
	})

	t.Run("a different key builds a different blueprint", func(t *testing.T) {
		t.Parallel()
		a, err := newSettings(testMajor, Migrate("key-a-"+randomSuffix(), func(context.Context, *pgx.Conn) error { return nil }))
		require.NoError(t, err)
		b, err := newSettings(testMajor, Migrate("key-b-"+randomSuffix(), func(context.Context, *pgx.Conn) error { return nil }))
		require.NoError(t, err)

		na, err := cloneFromBlueprint(ctx, srv, a)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, dropDatabase(ctx, srv.admin, na)) })
		nb, err := cloneFromBlueprint(ctx, srv, b)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, dropDatabase(ctx, srv.admin, nb)) })

		assert.NotEqual(t, a.blueprint(), b.blueprint())
		assert.Equal(t, 1, countDatabases(ctx, t, srv, a.blueprint()))
		assert.Equal(t, 1, countDatabases(ctx, t, srv, b.blueprint()))
	})

	t.Run("a failing migration leaves no blueprint behind", func(t *testing.T) {
		t.Parallel()
		boom := fmt.Errorf("boom")
		cfg, err := newSettings(testMajor, Migrate("key-fail-"+randomSuffix(), func(context.Context, *pgx.Conn) error { return boom }))
		require.NoError(t, err)

		_, err = cloneFromBlueprint(ctx, srv, cfg)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, 0, countDatabases(ctx, t, srv, cfg.blueprint()))
	})
}

func TestDropDatabase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	srv, err := startServer(ctx, testMajor)
	require.NoError(t, err)
	cfg, err := newSettings(testMajor, Migrations(os.DirFS("testdata/migrations")))
	require.NoError(t, err)

	name, err := cloneFromBlueprint(ctx, srv, cfg)
	require.NoError(t, err)

	// An open connection would block a plain DROP DATABASE.
	conn, err := pgx.Connect(ctx, srv.dsn(name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	require.NoError(t, dropDatabase(ctx, srv.admin, name))
	assert.Equal(t, 0, countDatabases(ctx, t, srv, name))
}

func countDatabases(ctx context.Context, t *testing.T, srv *server, pattern string) int {
	t.Helper()
	var n int
	require.NoError(t, srv.admin.QueryRow(ctx, "select count(*) from pg_database where datname like $1", pattern).Scan(&n))
	return n
}

func countColumns(ctx context.Context, t *testing.T, srv *server, database, table string) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, srv.dsn(database))
	require.NoError(t, err)
	defer conn.Close(ctx)
	var n int
	require.NoError(t, conn.QueryRow(ctx, "select count(*) from information_schema.columns where table_name = $1", table).Scan(&n))
	return n
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
