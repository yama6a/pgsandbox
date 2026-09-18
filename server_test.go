package pgsandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartServer(t *testing.T) {
	t.Parallel()

	first, err := startServer(t.Context(), testMajor)
	require.NoError(t, err)
	second, err := startServer(t.Context(), testMajor)
	require.NoError(t, err)

	assert.Same(t, first, second)
	assert.Equal(t, "pgsandbox-18", first.name)

	var one int
	require.NoError(t, first.admin.QueryRow(t.Context(), "select 1").Scan(&one))
	assert.Equal(t, 1, one)

	var fsync string
	require.NoError(t, first.admin.QueryRow(t.Context(), "show fsync").Scan(&fsync))
	assert.Equal(t, "off", fsync)

	// Without the reap label the reaper's filter never matches, so the container outlives the run.
	info, err := first.ctr.Inspect(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, info.Config.Labels, "org.testcontainers.reap")
}
