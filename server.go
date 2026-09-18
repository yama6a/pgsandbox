package pgsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	serverUser   = "sandbox"
	serverPass   = "sandbox"
	adminDB      = "postgres"
	pgPort       = "5432"
	pgPortKey    = pgPort + "/tcp"
	startTimeout = 90 * time.Second
	pollInterval = 100 * time.Millisecond
)

var errNoDocker = errors.New("docker not found on PATH")

type server struct {
	name  string
	host  string
	port  string
	admin *pgxpool.Pool
}

// One server per Postgres major per process. The container itself is named, so other processes
// and later runs attach to the same one instead of starting their own.
//
//nolint:gochecknoglobals // process-wide registry of the shared containers
var registry = struct {
	sync.Mutex
	byMajor map[int]*server
	errs    map[int]error
}{byMajor: map[int]*server{}, errs: map[int]error{}}

func startServer(ctx context.Context, major int) (*server, error) {
	registry.Lock()
	defer registry.Unlock()

	if s, ok := registry.byMajor[major]; ok {
		return s, nil
	}
	if err, ok := registry.errs[major]; ok {
		return nil, err
	}

	s, err := launch(ctx, major)
	if err != nil {
		registry.errs[major] = err
		return nil, err
	}
	registry.byMajor[major] = s
	return s, nil
}

func launch(ctx context.Context, major int) (*server, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, errNoDocker
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	name := fmt.Sprintf("pgsandbox-%d", major)
	if err := ensureRunning(ctx, name, fmt.Sprintf("postgres:%d-alpine", major)); err != nil {
		return nil, err
	}

	info, err := inspect(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("address of %s: %w", name, err)
	}
	hostPort := info.hostPort()
	if hostPort == "" {
		return nil, fmt.Errorf("address of %s: no host binding for %s", name, pgPortKey)
	}

	_, inContainer := os.Stat("/.dockerenv")
	host, port := endpoint(os.Getenv("DOCKER_HOST"), inContainer == nil, info.ip(), hostPort)

	s := &server{name: name, host: host, port: port}
	s.admin, err = waitForPostgres(ctx, s.dsn(adminDB))
	if err != nil {
		return nil, fmt.Errorf("%s did not become ready: %w", name, err)
	}
	return s, nil
}

// ensureRunning creates the container, or starts it if it exists but is stopped. Two processes
// may race to create it; the loser sees a name conflict and comes back around to attach.
func ensureRunning(ctx context.Context, name, image string) error {
	for {
		info, err := inspect(ctx, name)
		switch {
		case err == nil && info.State.Running:
			return nil
		case err == nil:
			if _, err := docker(ctx, "start", name); err != nil {
				return fmt.Errorf("start %s: %w", name, err)
			}
			return nil
		}

		_, err = docker(ctx, "run", "--detach", "--name", name,
			"--env", "POSTGRES_USER="+serverUser,
			"--env", "POSTGRES_PASSWORD="+serverPass,
			"--env", "POSTGRES_DB="+adminDB,
			"--publish", "127.0.0.1::"+pgPort,
			"--tmpfs", "/var/lib/postgresql:rw",
			image,
			"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
			// Every test process shares this server, and go test runs NumCPU packages, each with
			// NumCPU parallel tests, each holding a small pool. The default 100 runs out fast.
			"-c", "max_connections=1000",
		)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "already in use") {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
}

// waitForPostgres polls until a TCP connection succeeds. The image runs a temporary server on
// a unix socket only while it initialises, so a successful TCP connect means the real one is up.
func waitForPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last error
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("pool: %w", err)
		}
		last = pool.Ping(ctx)
		if last == nil {
			return pool, nil
		}
		pool.Close()

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last: %w)", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// containerInfo is the part of `docker inspect` output this package reads.
//
//nolint:tagliatelle // the daemon's field names, not ours
type containerInfo struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Ports     map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// inspect reads the whole JSON document rather than asking for `--format`, because the docker
// CLI runs format templates with missingkey=error and Engine 28 dropped the top-level
// NetworkSettings.IPAddress, so a template naming a field that a given daemon omits fails the
// whole call instead of yielding an empty string.
func inspect(ctx context.Context, name string) (*containerInfo, error) {
	out, err := docker(ctx, "inspect", "--type", "container", name)
	if err != nil {
		return nil, err
	}

	var infos []containerInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("inspect %s: empty response", name)
	}
	return &infos[0], nil
}

// ip is the container's own address on the docker network, or "" when the daemon reports none.
// Engine 28 and newer report it per network and may omit the top-level copy entirely.
func (c *containerInfo) ip() string {
	if c.NetworkSettings.IPAddress != "" {
		return c.NetworkSettings.IPAddress
	}
	if n, ok := c.NetworkSettings.Networks["bridge"]; ok && n.IPAddress != "" {
		return n.IPAddress
	}
	for _, name := range slices.Sorted(maps.Keys(c.NetworkSettings.Networks)) {
		if ip := c.NetworkSettings.Networks[name].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// hostPort is the published port on the daemon's host. A daemon with IPv6 enabled publishes one
// binding per family, and only the IPv4 one is reachable over the loopback address used here.
func (c *containerInfo) hostPort() string {
	bindings := c.NetworkSettings.Ports[pgPortKey]
	for _, b := range bindings {
		if b.HostPort != "" && !strings.Contains(b.HostIP, ":") {
			return b.HostPort
		}
	}
	for _, b := range bindings {
		if b.HostPort != "" {
			return b.HostPort
		}
	}
	return ""
}

func docker(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// endpoint picks how to reach Postgres. A tcp or ssh DOCKER_HOST publishes the port on that
// machine. Tests running inside a container that talks to the host's daemon (CI sandboxes,
// devcontainers) cannot see the host's loopback, but can reach the container's bridge address
// directly. Everything else is the local loopback and the published port.
func endpoint(dockerHost string, inContainer bool, containerIP, hostPort string) (host, port string) {
	if h := publishedHost(dockerHost); h != "" {
		return h, hostPort
	}
	if inContainer && containerIP != "" {
		return containerIP, pgPort
	}
	return "127.0.0.1", hostPort
}

// publishedHost is the remote host named by a tcp or ssh DOCKER_HOST, or "" for a local daemon.
func publishedHost(dockerHost string) string {
	u, err := url.Parse(dockerHost)
	if err != nil || u.Hostname() == "" || u.Scheme == "unix" || u.Scheme == "npipe" {
		return ""
	}
	return u.Hostname()
}

func (s *server) dsn(database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(serverUser, serverPass),
		Host:     net.JoinHostPort(s.host, s.port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}
