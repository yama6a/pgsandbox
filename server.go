package pgsandbox

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	serverUser   = "sandbox"
	serverPass   = "sandbox"
	adminDB      = "postgres"
	startTimeout = 90 * time.Second
)

type server struct {
	name  string
	host  string
	port  string
	admin *pgxpool.Pool
	ctr   testcontainers.Container
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
	// Ryuk removes every container carrying its session labels once the process exits, reusable
	// ones included, and the labels cannot be taken off through the API. The container is meant
	// to outlive the run, so the reaper is off for this process unless the caller decided otherwise.
	if _, set := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); !set {
		if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true"); err != nil {
			return nil, fmt.Errorf("disable ryuk: %w", err)
		}
	}

	name := fmt.Sprintf("pgsandbox-%d", major)
	ctr, err := postgres.Run(ctx, fmt.Sprintf("postgres:%d-alpine", major),
		postgres.WithDatabase(adminDB),
		postgres.WithUsername(serverUser),
		postgres.WithPassword(serverPass),
		testcontainers.WithReuseByName(name),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/postgresql": "rw"}),
		testcontainers.WithCmdArgs("-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"),
		testcontainers.WithWaitStrategyAndDeadline(startTimeout,
			// The image restarts Postgres once after init, so the first "ready" line is not the last.
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, fmt.Errorf("container port: %w", err)
	}

	s := &server{name: name, host: host, port: port.Port(), ctr: ctr}
	s.admin, err = pgxpool.New(ctx, s.dsn(adminDB))
	if err != nil {
		return nil, fmt.Errorf("admin pool: %w", err)
	}
	if err := s.admin.Ping(ctx); err != nil {
		return nil, fmt.Errorf("admin ping: %w", err)
	}
	return s, nil
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
