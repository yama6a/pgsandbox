package pgsandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Renovate tracks this pin; the number is the only place the version is written.
const testMajor = 18

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("parallel tests get isolated migrated databases", func(t *testing.T) {
		t.Parallel()
		for _, label := range []string{"top", "middle", "bottom"} {
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				db := New(t, testMajor, Migrations(os.DirFS("testdata/migrations")))

				_, err := db.Pool.Exec(t.Context(), "insert into shelves (label, capacity) values ($1, 4)", label)
				require.NoError(t, err)

				var got []string
				rows, err := db.Pool.Query(t.Context(), "select label from shelves")
				require.NoError(t, err)
				got, err = pgx.CollectRows(rows, pgx.RowTo[string])
				require.NoError(t, err)
				assert.Equal(t, []string{label}, got)
				assert.Contains(t, db.DSN, db.Name)
			})
		}
	})

	t.Run("the database is dropped after the test", func(t *testing.T) {
		t.Parallel()
		var name string
		//nolint:paralleltest // must finish before the parent looks for the database
		t.Run("inner", func(t *testing.T) {
			name = New(t, testMajor, Migrations(os.DirFS("testdata/migrations"))).Name
		})
		srv, err := startServer(t.Context(), testMajor)
		require.NoError(t, err)
		assert.Equal(t, 0, countDatabases(t.Context(), t, srv, name))
	})

	t.Run("a migrator runs once per key", func(t *testing.T) {
		t.Parallel()
		var runs atomic.Int32
		key := "once-" + randomSuffix()
		record := Migrate(key, func(ctx context.Context, conn *pgx.Conn) error {
			runs.Add(1)
			if _, err := conn.Exec(ctx, "create table marks (id int)"); err != nil {
				return fmt.Errorf("create marks: %w", err)
			}
			return nil
		})

		for range 3 {
			db := New(t, testMajor, record)
			var n int
			require.NoError(t, db.Pool.QueryRow(t.Context(), "select count(*) from marks").Scan(&n))
			assert.Equal(t, 0, n)
		}
		assert.Equal(t, int32(1), runs.Load())
	})

	t.Run("a bad major fails the test before touching docker", func(t *testing.T) {
		t.Parallel()
		tb := &recordingTB{T: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			New(tb, 0)
		}()
		<-done
		assert.Contains(t, tb.fatal, "major version must be positive")
	})
}

func TestKeepOnFailure(t *testing.T) {
	t.Parallel()
	if os.Getenv("PGSANDBOX_HELPER") == "1" {
		db := New(t, testMajor, KeepOnFailure())
		//nolint:gosec // the path comes from the parent test's own environment
		require.NoError(t, os.WriteFile(os.Getenv("PGSANDBOX_NAME_FILE"), []byte(db.Name), 0o600))
		t.Fatal("deliberate failure")
	}

	nameFile := filepath.Join(t.TempDir(), "name")
	//nolint:gosec // re-runs this test binary with a fixed argument list
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "^TestKeepOnFailure$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "PGSANDBOX_HELPER=1", "PGSANDBOX_NAME_FILE="+nameFile)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, out.String())
	assert.Equal(t, 1, exit.ExitCode())

	name, err := os.ReadFile(nameFile)
	require.NoError(t, err)
	srv, err := startServer(t.Context(), testMajor)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, dropDatabase(context.Background(), srv.admin, string(name))) })

	assert.Equal(t, 1, countDatabases(t.Context(), t, srv, string(name)))
	assert.Contains(t, out.String(), srv.dsn(string(name)))
}

// recordingTB captures Fatalf instead of ending the test, so New's failure path can be asserted.
type recordingTB struct {
	*testing.T
	fatal string
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = strings.TrimSpace(format + " " + strings.Join(stringify(args), " "))
	runtime.Goexit()
}

func stringify(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if err, ok := a.(error); ok {
			out[i] = err.Error()
			continue
		}
		if s, ok := a.(string); ok {
			out[i] = s
		}
	}
	return out
}
