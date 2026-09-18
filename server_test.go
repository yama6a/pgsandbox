package pgsandbox

import (
	"os/exec"
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

	var maxConns string
	require.NoError(t, first.admin.QueryRow(t.Context(), "show max_connections").Scan(&maxConns))
	assert.Equal(t, "1000", maxConns)

	//nolint:gosec // fixed argument list, name comes from the code under test
	out, err := exec.CommandContext(t.Context(), "docker", "inspect", first.name,
		"--format", "{{.State.Running}} {{.Config.Image}} {{index .HostConfig.Tmpfs \"/var/lib/postgresql\"}}").Output()
	require.NoError(t, err)
	assert.Equal(t, "true postgres:18-alpine rw\n", string(out))
}

func TestEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		dockerHost  string
		inContainer bool
		wantHost    string
		wantPort    string
	}{
		{name: "local daemon", wantHost: "127.0.0.1", wantPort: "32768"},
		{name: "unix socket", dockerHost: "unix:///var/run/docker.sock", wantHost: "127.0.0.1", wantPort: "32768"},
		{name: "named pipe", dockerHost: "npipe:////./pipe/docker", wantHost: "127.0.0.1", wantPort: "32768"},
		{name: "tcp daemon", dockerHost: "tcp://10.0.0.7:2375", wantHost: "10.0.0.7", wantPort: "32768"},
		{name: "ssh daemon", dockerHost: "ssh://core@build-box", wantHost: "build-box", wantPort: "32768"},
		{name: "inside a container", inContainer: true, wantHost: "172.17.0.7", wantPort: "5432"},
		{name: "inside a container, remote daemon", inContainer: true, dockerHost: "tcp://10.0.0.7:2375", wantHost: "10.0.0.7", wantPort: "32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, port := endpoint(tc.dockerHost, tc.inContainer, "172.17.0.7", "32768")
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantPort, port)
		})
	}
}

func TestLaunchWithoutDocker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := launch(t.Context(), 99)
	require.ErrorIs(t, err, errNoDocker)
}
