package pgsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	serverUser   = "sandbox"
	serverPass   = "sandbox"
	adminDB      = "postgres"
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

	addr, err := docker(ctx, "inspect", name, "--format",
		`{{.NetworkSettings.IPAddress}} {{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}`)
	if err != nil {
		return nil, fmt.Errorf("address of %s: %w", name, err)
	}
	containerIP, hostPort, _ := strings.Cut(addr, " ")

	_, inContainer := os.Stat("/.dockerenv")
	host, port := endpoint(os.Getenv("DOCKER_HOST"), inContainer == nil, containerIP, hostPort)

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
		running, err := docker(ctx, "inspect", name, "--format", "{{.State.Running}}")
		switch {
		case err == nil && running == "true":
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
			"--publish", "127.0.0.1::5432",
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
		return containerIP, "5432"
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
