package pgsandbox

import (
	"encoding/json"
	"os"
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

func TestContainerInfoFromRealInspect(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/inspect-engine27.json")
	require.NoError(t, err)

	var infos []containerInfo
	require.NoError(t, json.Unmarshal(raw, &infos))
	require.Len(t, infos, 1)

	assert.True(t, infos[0].State.Running)
	assert.Equal(t, "172.17.0.4", infos[0].ip())
	assert.Equal(t, "32773", infos[0].hostPort())
}

func TestContainerInfoAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		doc      string
		wantIP   string
		wantPort string
	}{
		{
			name:     "engine 27 publishes the address twice",
			doc:      `{"NetworkSettings":{"IPAddress":"172.17.0.4","Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.4"}}}}`,
			wantIP:   "172.17.0.4",
			wantPort: "32773",
		},
		{
			name:     "engine 28 drops the top-level address",
			doc:      `{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.4"}}}}`,
			wantIP:   "172.17.0.4",
			wantPort: "32773",
		},
		{
			name:     "only a user-defined network",
			doc:      `{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]},"Networks":{"ci-net":{"IPAddress":"10.89.0.3"}}}}`,
			wantIP:   "10.89.0.3",
			wantPort: "32773",
		},
		{
			name:     "no address anywhere",
			doc:      `{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]},"Networks":{"bridge":{"IPAddress":""}}}}`,
			wantIP:   "",
			wantPort: "32773",
		},
		{
			name:     "dual-stack binding prefers IPv4",
			doc:      `{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"::","HostPort":"32789"},{"HostIp":"0.0.0.0","HostPort":"32789"}]}}}`,
			wantIP:   "",
			wantPort: "32789",
		},
		{
			name:     "IPv6 binding only",
			doc:      `{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"::","HostPort":"32789"}]}}}`,
			wantIP:   "",
			wantPort: "32789",
		},
		{
			name:     "nothing published",
			doc:      `{"NetworkSettings":{"IPAddress":"172.17.0.4","Ports":{}}}`,
			wantIP:   "172.17.0.4",
			wantPort: "",
		},
		{
			name:     "no network settings at all",
			doc:      `{}`,
			wantIP:   "",
			wantPort: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var info containerInfo
			require.NoError(t, json.Unmarshal([]byte(tc.doc), &info))
			assert.Equal(t, tc.wantIP, info.ip())
			assert.Equal(t, tc.wantPort, info.hostPort())
		})
	}
}

func TestInspectMissingContainer(t *testing.T) {
	t.Parallel()

	_, err := inspect(t.Context(), "pgsandbox-does-not-exist")
	require.Error(t, err)
}
