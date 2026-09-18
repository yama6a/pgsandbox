# pgsandbox

A migrated Postgres database per Go test, in one call.

```sh
go get github.com/yama6a/pgsandbox
```

```go
import "github.com/yama6a/pgsandbox"

func TestOrders(t *testing.T) {
    t.Parallel()
    db := pgsandbox.New(t, 18, pgsandbox.Migrations(os.DirFS("../../migrations")))

    _, err := db.Pool.Exec(t.Context(), "insert into orders (id) values (1)")
    // ...
}
```

`New` starts Postgres 18 in Docker the first time it runs, migrates a blueprint database once for
that migration set, clones the blueprint for this test, and drops the clone when the test ends.
Tests can run in parallel; each one sees only its own rows.

## Requirements

- Docker reachable by [testcontainers-go](https://golang.testcontainers.org/)
- Go 1.27

## API

```go
func New(tb testing.TB, major int, opts ...Option) *DB
```

`major` is the Postgres major version; the image is `postgres:<major>-alpine`. There is no
default. `New` fails the test on any error.

| option | what it does |
|---|---|
| `Migrations(fsys fs.FS)` | run every `.sql` file at the root of `fsys`, in name order, when the blueprint is built. Works with `os.DirFS` and `embed.FS` |
| `Migrate(key string, fn func(context.Context, *pgx.Conn) error)` | run `fn` on the blueprint after the scripts, for goose, sql-migrate or any other tool. `key` must change whenever the migrations change |
| `KeepOnFailure()` | when the test fails, keep the database and log its DSN so you can open it with psql |

`DB` has `Pool` (a `*pgxpool.Pool`, closed on cleanup), `DSN` (a `postgres://` URL) and `Name`.

## How it stays fast

- One container per major, named `pgsandbox-<major>`, shared by every test process and every
  later `go test` run. Data lives on tmpfs with fsync off.
- The blueprint is named after a hash of the scripts and `Migrate` keys, so a changed migration
  gets a new blueprint and an unchanged one is found again on the next run, container restarts
  aside.
- Cloning is `CREATE DATABASE ... TEMPLATE`, a file copy. A Postgres advisory lock makes parallel
  tests with the same migrations wait for one build instead of racing.

The container is meant to outlive the run, so pgsandbox sets `TESTCONTAINERS_RYUK_DISABLED=true`
in the test process unless that variable is already set. Remove the container yourself:

```sh
docker ps -aq -f name='^pgsandbox-' | xargs docker rm -f    # or: make clean
```

## Development

```sh
make ci     # tidy, fmt, lint, vet, test, govulncheck; the same checks CI runs
make test
make clean  # remove the shared containers
```

Every merge to main is a release. The version bump comes from the PR label: `release:major`,
`release:minor`, else patch.
