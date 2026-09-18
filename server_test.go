package pgsandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const longID = "c561ce51d0d3801241b9476e875ff503f128f6800b147f94ab985670054c0532"

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

func TestReachRemoteDaemon(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		dockerHost string
		wantHost   string
	}{
		{name: "tcp daemon", dockerHost: "tcp://10.0.0.7:2375", wantHost: "10.0.0.7"},
		{name: "ssh daemon", dockerHost: "ssh://core@build-box", wantHost: "build-box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sandbox containerInfo
			require.NoError(t, json.Unmarshal([]byte(`{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]}}}`), &sandbox))

			host, port, err := reach(t.Context(), tc.dockerHost, "pgsandbox-18", &sandbox)
			require.NoError(t, err)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, "32773", port)
		})
	}
}

func TestReachLocalDaemon(t *testing.T) {
	t.Parallel()
	cases := []string{"", "unix:///var/run/docker.sock", "npipe:////./pipe/docker"}
	for _, dockerHost := range cases {
		t.Run(dockerHost, func(t *testing.T) {
			t.Parallel()
			if inContainer() {
				t.Skip("the loopback path only applies on the host")
			}
			var sandbox containerInfo
			require.NoError(t, json.Unmarshal([]byte(`{"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32773"}]}}}`), &sandbox))

			host, port, err := reach(t.Context(), dockerHost, "pgsandbox-18", &sandbox)
			require.NoError(t, err)
			assert.Equal(t, "127.0.0.1", host)
			assert.Equal(t, "32773", port)
		})
	}
}

func TestSharedIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		self    string
		sandbox string
		want    string
	}{
		{
			name:    "same bridge",
			self:    `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"a","IPAddress":"172.17.0.8"}}}}`,
			sandbox: `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"a","IPAddress":"172.17.0.4"}}}}`,
			want:    "172.17.0.4",
		},
		{
			name:    "shared user-defined network among several",
			self:    `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"a","IPAddress":"172.17.0.8"},"ci":{"NetworkID":"c","IPAddress":"10.89.0.2"}}}}`,
			sandbox: `{"NetworkSettings":{"Networks":{"ci":{"NetworkID":"c","IPAddress":"10.89.0.3"}}}}`,
			want:    "10.89.0.3",
		},
		{
			name:    "no network in common",
			self:    `{"NetworkSettings":{"Networks":{"ci":{"NetworkID":"c","IPAddress":"10.89.0.2"}}}}`,
			sandbox: `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"a","IPAddress":"172.17.0.4"}}}}`,
			want:    "",
		},
		{
			name:    "same name on two daemons is not the same network",
			self:    `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"a","IPAddress":"172.17.0.8"}}}}`,
			sandbox: `{"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"b","IPAddress":"172.17.0.4"}}}}`,
			want:    "",
		},
		{
			name:    "both on the host stack",
			self:    `{"NetworkSettings":{"Networks":{"host":{"NetworkID":"h","IPAddress":""}}}}`,
			sandbox: `{"NetworkSettings":{"Networks":{"host":{"NetworkID":"h","IPAddress":""}}}}`,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var self, sandbox containerInfo
			require.NoError(t, json.Unmarshal([]byte(tc.self), &self))
			require.NoError(t, json.Unmarshal([]byte(tc.sandbox), &sandbox))
			assert.Equal(t, tc.want, sharedIP(&self, &sandbox))
		})
	}
}

func TestAttachableNetworks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "bridge and a user-defined network, sorted",
			doc:  `{"NetworkSettings":{"Networks":{"zz":{"IPAddress":"10.89.0.2"},"bridge":{"IPAddress":"172.17.0.8"}}}}`,
			want: []string{"bridge", "zz"},
		},
		{name: "host stack", doc: `{"NetworkSettings":{"Networks":{"host":{"IPAddress":""}}}}`},
		{name: "no networking", doc: `{"NetworkSettings":{"Networks":{"none":{"IPAddress":""}}}}`},
		{name: "nothing reported", doc: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var info containerInfo
			require.NoError(t, json.Unmarshal([]byte(tc.doc), &info))
			assert.Equal(t, tc.want, info.attachableNetworks())
		})
	}
}

func TestUsesHostNetwork(t *testing.T) {
	t.Parallel()

	var host, bridge containerInfo
	require.NoError(t, json.Unmarshal([]byte(`{"NetworkSettings":{"Networks":{"host":{}}}}`), &host))
	require.NoError(t, json.Unmarshal([]byte(`{"NetworkSettings":{"Networks":{"bridge":{}}}}`), &bridge))

	assert.True(t, host.usesHostNetwork())
	assert.False(t, bridge.usesHostNetwork())
}

func TestContainerIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mountinfo string
		want      []string
	}{
		{
			name:      "docker bind mounts",
			mountinfo: "411 388 253:17 /var/lib/docker/containers/" + longID + "/resolv.conf /etc/resolv.conf rw" + "\n" + "412 388 253:17 /var/lib/docker/containers/" + longID + "/hosts /etc/hosts rw",
			want:      []string{longID},
		},
		{
			name:      "podman bind mounts",
			mountinfo: "500 400 0:50 /overlay-containers/" + longID + "/userdata/resolv.conf /etc/resolv.conf rw",
			want:      []string{longID},
		},
		{
			name:      "no container mounts",
			mountinfo: "25 30 253:1 / /etc/hosts rw,relatime - ext4 /dev/sda1 rw",
		},
		{
			name:      "layer hashes are not ids",
			mountinfo: "30 25 0:40 / / rw - overlay rw,upperdir=/var/lib/docker/overlay2/" + longID + "/diff",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, containerIDs(tc.mountinfo))
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
