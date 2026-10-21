package pgsandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	envDSN = "PGSANDBOX_DSN"
	// ownerLockSpace is the first key of every suffix lock. The two-key form keeps these locks
	// apart from the one-key blueprint locks.
	ownerLockSpace int32 = 0x70677362
	keptComment          = "pgsandbox: kept on failure"
)

var (
	errNotURL        = errors.New("external server dsn must be a postgres:// URL")
	errMajorMismatch = errors.New("external server runs a different postgres major")
	errNoCreateDB    = errors.New("external server role may not create databases")
)

// attach uses a server that is already running. Test processes from other repos may share it,
// and the same migration fingerprint there does not mean the same schema, so this process names
// its databases with its own suffix and holds a lock on that suffix while it runs.
func attach(ctx context.Context, major int, dsn string) (*server, error) {
	// The url.Parse error repeats the input, password included, so it is not wrapped.
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errNotURL
	}

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("external server: %w", err)
	}
	if err := checkServer(ctx, admin, major); err != nil {
		admin.Close()
		return nil, err
	}

	owner, suffix, err := claimSuffix(ctx, dsn)
	if err != nil {
		admin.Close()
		return nil, err
	}
	if err := sweep(ctx, admin, owner, suffix); err != nil {
		_ = owner.Close(ctx)
		admin.Close()
		return nil, err
	}
	return &server{name: u.Host, base: *u, suffix: "_" + suffix, admin: admin, owner: owner}, nil
}

func checkServer(ctx context.Context, admin *pgxpool.Pool, major int) error {
	var version int
	var createDB bool
	err := admin.QueryRow(ctx, `select current_setting('server_version_num')::int, rolcreatedb or rolsuper
		from pg_roles where rolname = current_user`).Scan(&version, &createDB)
	if err != nil {
		return fmt.Errorf("external server: %w", err)
	}
	if got := version / 10000; got != major {
		return fmt.Errorf("%w: asked for %d, server runs %d", errMajorMismatch, major, got)
	}
	if !createDB {
		return errNoCreateDB
	}
	return nil
}

// claimSuffix picks a random suffix whose lock no running process holds, and takes the lock on
// a connection of its own. The lock is released when the process exits and the server notices
// the connection is gone.
func claimSuffix(ctx context.Context, dsn string) (*pgx.Conn, string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, "", fmt.Errorf("external server: %w", err)
	}
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		suffix := hex.EncodeToString(b[:])
		ok, err := tryLockSuffix(ctx, conn, suffix)
		if err != nil {
			_ = conn.Close(ctx)
			return nil, "", err
		}
		if ok {
			return conn, suffix, nil
		}
	}
}

// sweep drops the databases of processes that ended without dropping their own: a free suffix
// lock means its process is gone. The sweeper holds that lock while it drops, so two sweepers
// never work on the same suffix. Databases kept by KeepOnFailure stay.
func sweep(ctx context.Context, admin *pgxpool.Pool, owner *pgx.Conn, own string) error {
	names, err := leftovers(ctx, admin, "[0-9a-f]{8}")
	if err != nil {
		return err
	}
	var suffixes []string
	for _, name := range names {
		if suffix := name[len(name)-8:]; suffix != own && !slices.Contains(suffixes, suffix) {
			suffixes = append(suffixes, suffix)
		}
	}

	var errs []error
	for _, suffix := range suffixes {
		gone, err := tryLockSuffix(ctx, owner, suffix)
		if err != nil {
			return err
		}
		if !gone {
			continue
		}
		// Listed again under the lock: the owner may have kept a database after the first list.
		names, err := leftovers(ctx, admin, suffix)
		if err != nil {
			errs = append(errs, err)
		}
		for _, name := range names {
			errs = append(errs, dropDatabase(ctx, admin, name))
		}
		errs = append(errs, unlockSuffix(ctx, owner, suffix))
	}
	return errors.Join(errs...)
}

// leftovers lists this role's databases whose suffix matches the regular expression suffix,
// except those kept by KeepOnFailure.
func leftovers(ctx context.Context, admin *pgxpool.Pool, suffix string) ([]string, error) {
	rows, err := admin.Query(ctx, `select datname from pg_database
		where datname ~ ('^(bp|sb)_[0-9a-f]{16}_' || $1 || '$')
		and datdba = (select oid from pg_roles where rolname = current_user)
		and coalesce(shobj_description(oid, 'pg_database'), '') <> $2`, suffix, keptComment)
	if err != nil {
		return nil, fmt.Errorf("list leftover databases: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list leftover databases: %w", err)
	}
	return names, nil
}

func tryLockSuffix(ctx context.Context, conn *pgx.Conn, suffix string) (bool, error) {
	key, err := suffixKey(suffix)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1, $2)", ownerLockSpace, key).Scan(&ok); err != nil {
		return false, fmt.Errorf("lock suffix %s: %w", suffix, err)
	}
	return ok, nil
}

func unlockSuffix(ctx context.Context, conn *pgx.Conn, suffix string) error {
	key, err := suffixKey(suffix)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1, $2)", ownerLockSpace, key); err != nil {
		return fmt.Errorf("unlock suffix %s: %w", suffix, err)
	}
	return nil
}

// suffixKey reads the 8 hex digits of a suffix as the int4 second key of its lock.
func suffixKey(suffix string) (int32, error) {
	n, err := strconv.ParseUint(suffix, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("suffix %q: %w", suffix, err)
	}
	return int32(uint32(n)), nil
}

// markKept tags a database so that sweep leaves it alone after its process exits.
func markKept(ctx context.Context, admin *pgxpool.Pool, name string) error {
	stmt := "comment on database " + pgx.Identifier{name}.Sanitize() + " is '" + keptComment + "'"
	if _, err := admin.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("mark %s as kept: %w", name, err)
	}
	return nil
}
