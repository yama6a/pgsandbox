package pgsandbox

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSettings(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"001.sql": {Data: []byte("select 1;")}}
	noop := func(context.Context, *pgx.Conn) error { return nil }

	t.Run("major must be positive", func(t *testing.T) {
		t.Parallel()
		_, err := newSettings(0)
		require.ErrorIs(t, err, errBadMajor)
		_, err = newSettings(-3)
		require.ErrorIs(t, err, errBadMajor)
	})

	t.Run("image follows the major", func(t *testing.T) {
		t.Parallel()
		s, err := newSettings(18)
		require.NoError(t, err)
		assert.Equal(t, "postgres:18-alpine", s.image())
	})

	t.Run("a bad migrations fs surfaces from newSettings", func(t *testing.T) {
		t.Parallel()
		_, err := newSettings(18, Migrations(fstest.MapFS{}))
		require.ErrorIs(t, err, errNoScripts)
	})

	t.Run("blueprint name depends on scripts and migrator keys", func(t *testing.T) {
		t.Parallel()
		plain, err := newSettings(18)
		require.NoError(t, err)
		withFS, err := newSettings(18, Migrations(fsys))
		require.NoError(t, err)
		withFn, err := newSettings(18, Migrations(fsys), Migrate("goose", noop))
		require.NoError(t, err)
		again, err := newSettings(18, Migrations(fsys), Migrate("goose", noop))
		require.NoError(t, err)

		assert.NotEqual(t, plain.blueprint(), withFS.blueprint())
		assert.NotEqual(t, withFS.blueprint(), withFn.blueprint())
		assert.Equal(t, withFn.blueprint(), again.blueprint())
		assert.Regexp(t, `^bp_[0-9a-f]{16}$`, withFn.blueprint())
		assert.Len(t, withFn.migrators, 1)
	})

	t.Run("keep on failure is off by default", func(t *testing.T) {
		t.Parallel()
		s, err := newSettings(18)
		require.NoError(t, err)
		assert.False(t, s.keep)
		s, err = newSettings(18, KeepOnFailure())
		require.NoError(t, err)
		assert.True(t, s.keep)
	})
}
