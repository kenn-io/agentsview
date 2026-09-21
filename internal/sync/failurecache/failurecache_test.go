package failurecache

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

func TestCheck(t *testing.T) {
	recorded := Identity{MTimeNS: 1700000000000000000}
	for _, test := range []struct {
		name    string
		current Identity
		want    bool
	}{
		{name: "unchanged source", current: recorded, want: true},
		{name: "modified source", current: Identity{MTimeNS: 1700000001000000000}},
		{name: "source deleted", current: Identity{Missing: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cache Cache
			cache.Record("claude:/s.jsonl", recorded)
			assert.Equal(t, test.want, cache.Check("claude:/s.jsonl", test.current))
			assert.False(t, cache.Check("claude:/other.jsonl", test.current))
		})
	}
}

// A file whose mtime is the Unix epoch is a real file, not a missing one.
func TestCheckDistinguishesEpochMtimeFromMissing(t *testing.T) {
	var cache Cache
	cache.Record("claude:/epoch.jsonl", Identity{MTimeNS: 0})
	assert.False(t, cache.Check("claude:/epoch.jsonl", Identity{Missing: true}))
}

func TestCheckForgetsStaleEntry(t *testing.T) {
	var cache Cache
	old := Identity{MTimeNS: 100}
	cache.Record("claude:/s.jsonl", old)
	require.False(t, cache.Check("claude:/s.jsonl", Identity{MTimeNS: 200}))
	assert.False(t, cache.Check("claude:/s.jsonl", old),
		"a source restored to its old mtime must be parsed again")
}

func TestFlushSurvivesRestart(t *testing.T) {
	database := openDB(t)
	kept := Identity{MTimeNS: 100}
	var cache Cache
	cache.Record("claude:/kept.jsonl", kept)
	cache.Record("claude:/gone.jsonl", Identity{Missing: true})
	cache.Record("claude:/cleared.jsonl", Identity{MTimeNS: 300})
	require.NoError(t, cache.Flush(t.Context(), database))
	cache.Clear("claude:/cleared.jsonl")
	require.NoError(t, cache.Flush(t.Context(), database))

	var restarted Cache
	require.NoError(t, restarted.Load(t.Context(), database))
	assert.True(t, restarted.Check("claude:/kept.jsonl", kept))
	assert.True(t, restarted.Check("claude:/gone.jsonl", Identity{Missing: true}))
	assert.False(t, restarted.Check("claude:/cleared.jsonl", Identity{MTimeNS: 300}))
}

// A resync flushes into the replacement archive first. When that build is
// discarded, the live archive must still receive the entries.
func TestFlushWritesToEachTarget(t *testing.T) {
	replacement, live := openDB(t), openDB(t)
	id := Identity{MTimeNS: 100}
	var cache Cache
	cache.Record("claude:/s.jsonl", id)
	require.NoError(t, cache.Flush(t.Context(), replacement))
	require.NoError(t, cache.Flush(t.Context(), live))

	persisted, err := live.LoadSourceFailures(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]db.SourceFailure{"claude:/s.jsonl": id}, persisted)
}
