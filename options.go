package pgsandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
)

var errBadMajor = errors.New("postgres major version must be positive")

// Option adjusts how New builds a database.
type Option func(*settings)

// Migrations runs every .sql file at the root of fsys, in name order, when the blueprint for
// this set is first built. Use os.DirFS or an embed.FS.
func Migrations(fsys fs.FS) Option {
	return func(s *settings) {
		scripts, err := readScripts(fsys)
		if err != nil {
			s.err = errors.Join(s.err, err)
			return
		}
		s.scripts = append(s.scripts, scripts...)
	}
}

// Migrate runs fn on the blueprint after any Migrations scripts, so any migration tool can be
// plugged in. The key must change whenever the migrations change, or a stale blueprint is reused.
func Migrate(key string, fn func(context.Context, *pgx.Conn) error) Option {
	return func(s *settings) {
		s.migrators = append(s.migrators, migrator{key: key, run: fn})
	}
}

// KeepOnFailure leaves the database in place when the test fails and logs its DSN, so it can be
// inspected with psql. Drop it by hand, or remove the container with `make clean`.
func KeepOnFailure() Option {
	return func(s *settings) { s.keep = true }
}

type migrator struct {
	key string
	run func(context.Context, *pgx.Conn) error
}

type settings struct {
	major     int
	scripts   []script
	migrators []migrator
	keep      bool
	err       error
}

func newSettings(major int, opts ...Option) (*settings, error) {
	if major <= 0 {
		return nil, fmt.Errorf("%w, got %d", errBadMajor, major)
	}
	s := &settings{major: major}
	for _, opt := range opts {
		opt(s)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func (s *settings) image() string {
	return fmt.Sprintf("postgres:%d-alpine", s.major)
}

func (s *settings) blueprint() string {
	keys := make([]string, 0, len(s.migrators))
	for _, m := range s.migrators {
		keys = append(keys, m.key)
	}
	return "bp_" + fingerprint(s.scripts, keys)
}
