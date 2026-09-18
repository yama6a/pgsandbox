package pgsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

var errNoScripts = errors.New("no .sql files found")

type script struct {
	name string
	body string
}

func readScripts(fsys fs.FS) ([]script, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("list migration files: %w", err)
	}

	var scripts []script
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		scripts = append(scripts, script{name: e.Name(), body: string(body)})
	}
	if len(scripts) == 0 {
		return nil, errNoScripts
	}

	slices.SortFunc(scripts, func(a, b script) int { return strings.Compare(a.name, b.name) })
	return scripts, nil
}

// fingerprint names a blueprint: the same scripts and migrator keys in the same order always
// map to the same 16 hex characters, so a later process finds the blueprint an earlier one built.
func fingerprint(scripts []script, keys []string) string {
	h := sha256.New()
	for _, s := range scripts {
		h.Write([]byte(s.name))
		h.Write([]byte{0})
		h.Write([]byte(s.body))
		h.Write([]byte{0})
	}
	for _, k := range keys {
		h.Write([]byte{1})
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
