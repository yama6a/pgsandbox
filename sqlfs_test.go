package pgsandbox

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadScripts(t *testing.T) {
	t.Parallel()

	t.Run("sorted by name, sql only, nested dirs skipped", func(t *testing.T) {
		t.Parallel()
		fsys := fstest.MapFS{
			"010_second.sql":      {Data: []byte("select 2;")},
			"001_first.sql":       {Data: []byte("select 1;")},
			"README.md":           {Data: []byte("not sql")},
			"nested/003_deep.sql": {Data: []byte("select 3;")},
		}

		scripts, err := readScripts(fsys)
		require.NoError(t, err)

		require.Len(t, scripts, 2)
		assert.Equal(t, "001_first.sql", scripts[0].name)
		assert.Equal(t, "select 1;", scripts[0].body)
		assert.Equal(t, "010_second.sql", scripts[1].name)
	})

	t.Run("no sql files is an error", func(t *testing.T) {
		t.Parallel()
		_, err := readScripts(fstest.MapFS{"notes.txt": {Data: []byte("x")}})
		require.ErrorIs(t, err, errNoScripts)
	})
}

func TestFingerprint(t *testing.T) {
	t.Parallel()

	a := []script{{name: "a.sql", body: "1"}, {name: "b.sql", body: "2"}}
	same := []script{{name: "a.sql", body: "1"}, {name: "b.sql", body: "2"}}
	reordered := []script{{name: "b.sql", body: "2"}, {name: "a.sql", body: "1"}}
	edited := []script{{name: "a.sql", body: "1"}, {name: "b.sql", body: "3"}}

	assert.Equal(t, fingerprint(a, nil), fingerprint(same, nil))
	assert.NotEqual(t, fingerprint(a, nil), fingerprint(reordered, nil))
	assert.NotEqual(t, fingerprint(a, nil), fingerprint(edited, nil))
	assert.NotEqual(t, fingerprint(a, nil), fingerprint(a, []string{"goose-v3"}))
	assert.NotEqual(t, fingerprint(a, []string{"x", "y"}), fingerprint(a, []string{"y", "x"}))
	assert.Len(t, fingerprint(a, nil), 16)
}
