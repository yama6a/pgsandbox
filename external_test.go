package pgsandbox

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttach_SweepsKilledProcess(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		t.Skip("needs " + envDSN)
	}
	if os.Getenv("PGSANDBOX_HELPER") == "1" {
		db := New(t, testMajor, Migrations(os.DirFS("testdata/migrations")))
		//nolint:gosec // the path comes from the parent test's own environment
		require.NoError(t, os.WriteFile(os.Getenv("PGSANDBOX_NAME_FILE"), []byte(db.Name), 0o600))
		<-t.Context().Done()
		return
	}

	nameFile := filepath.Join(t.TempDir(), "name")
	//nolint:gosec // re-runs this test binary with a fixed argument list
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "^TestAttach_SweepsKilledProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "PGSANDBOX_HELPER=1", "PGSANDBOX_NAME_FILE="+nameFile)
	require.NoError(t, cmd.Start())

	var name string
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(nameFile)
		name = string(b)
		return err == nil && name != ""
	}, time.Minute, 50*time.Millisecond)
	suffix := name[len(name)-9:]
	srv, err := startServer(t.Context(), testMajor, dsn)
	require.NoError(t, err)
	require.Equal(t, 2, countDatabases(t.Context(), t, srv, "%"+suffix), "a live process keeps its clone and blueprint")

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	// The server notices the dead connection, and frees its lock, a moment after the kill.
	require.Eventually(t, func() bool {
		next, err := attach(t.Context(), testMajor, dsn)
		if err != nil {
			return false
		}
		_ = next.owner.Close(context.Background())
		next.admin.Close()
		return countDatabases(t.Context(), t, srv, "%"+suffix) == 0
	}, 30*time.Second, 200*time.Millisecond)
}

func TestAttach(t *testing.T) {
	t.Parallel()

	t.Run("a keyword dsn is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := attach(t.Context(), testMajor, "host=localhost user=sandbox password=secret")
		require.ErrorIs(t, err, errNotURL)
		assert.NotContains(t, err.Error(), "secret")
	})

	dsn := os.Getenv(envDSN)
	if dsn == "" {
		t.Skip("needs " + envDSN)
	}

	t.Run("databases carry the process suffix", func(t *testing.T) {
		t.Parallel()
		srv, err := startServer(t.Context(), testMajor, dsn)
		require.NoError(t, err)
		cfg, err := newSettings(testMajor, Migrations(os.DirFS("testdata/migrations")))
		require.NoError(t, err)

		db := New(t, testMajor, Server(dsn), Migrations(os.DirFS("testdata/migrations")))

		assert.Regexp(t, `^_[0-9a-f]{8}$`, srv.suffix)
		assert.Equal(t, cfg.blueprint()+srv.suffix, srv.blueprint(cfg))
		assert.Regexp(t, `^sb_[0-9a-f]{16}`+srv.suffix+`$`, db.Name)
		assert.Equal(t, 1, countDatabases(t.Context(), t, srv, srv.blueprint(cfg)))
	})

	t.Run("sweep drops only the databases of ended processes", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		srv, err := startServer(ctx, testMajor, dsn)
		require.NoError(t, err)

		live, liveSuffix, err := claimSuffix(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = live.Close(context.Background()) })
		dead, deadSuffix, err := claimSuffix(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = dead.Close(context.Background()) })

		deadBP := "bp_" + randomSuffix() + "_" + deadSuffix
		deadClone := "sb_" + randomSuffix() + "_" + deadSuffix
		kept := "sb_" + randomSuffix() + "_" + deadSuffix
		liveClone := "sb_" + randomSuffix() + "_" + liveSuffix
		for _, name := range []string{deadBP, deadClone, kept, liveClone} {
			require.NoError(t, createDatabase(ctx, srv.admin, name, ""))
			t.Cleanup(func() { assert.NoError(t, dropDatabase(context.Background(), srv.admin, name)) })
		}
		require.NoError(t, markKept(ctx, srv.admin, kept))
		// The owner ends. An explicit unlock frees the lock at once, where a closed connection
		// frees it only once the server notices.
		require.NoError(t, unlockSuffix(ctx, dead, deadSuffix))

		// A sweeper in another test process may hold the dead suffix's lock while this one tries.
		require.Eventually(t, func() bool {
			assert.NoError(t, sweep(ctx, srv.admin, srv.owner, srv.suffix[1:]))
			return countDatabases(ctx, t, srv, "%_"+deadSuffix) == 1
		}, 30*time.Second, 100*time.Millisecond)

		got := map[string]int{}
		for _, name := range []string{deadBP, deadClone, kept, liveClone} {
			got[name] = countDatabases(ctx, t, srv, name)
		}
		assert.Equal(t, map[string]int{deadBP: 0, deadClone: 0, kept: 1, liveClone: 1}, got)
	})

	t.Run("a different major fails", func(t *testing.T) {
		t.Parallel()
		_, err := attach(t.Context(), testMajor+1, dsn)
		require.ErrorIs(t, err, errMajorMismatch)
	})

	t.Run("a role without createdb fails", func(t *testing.T) {
		t.Parallel()
		srv, err := startServer(t.Context(), testMajor, dsn)
		require.NoError(t, err)

		role := "pgsandbox_nocreatedb_" + randomSuffix()
		password := randomSuffix()
		_, err = srv.admin.Exec(t.Context(), "create role "+pgx.Identifier{role}.Sanitize()+
			" login nocreatedb password '"+password+"'")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := srv.admin.Exec(context.Background(), "drop role "+pgx.Identifier{role}.Sanitize())
			assert.NoError(t, err)
		})

		u := srv.base
		u.User = url.UserPassword(role, password)
		_, err = attach(t.Context(), testMajor, u.String())
		require.ErrorIs(t, err, errNoCreateDB)
	})
}
