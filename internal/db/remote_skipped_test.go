package db_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRemoteSkippedFiles(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	t.Run("initially empty", func(t *testing.T) {
		loaded, err := d.LoadRemoteSkippedFiles("empty-host")
		require.NoError(t, err, "LoadRemoteSkippedFiles")
		require.Empty(t, loaded)
	})

	t.Run("round trip", func(t *testing.T) {
		entries := map[string]int64{
			"/home/user/.claude/sessions/a.jsonl": 1000,
			"/home/user/.claude/sessions/b.jsonl": 2000,
			"/home/user/.claude/sessions/c.jsonl": 3000,
		}
		require.NoError(t,
			d.ReplaceRemoteSkippedFiles("roundtrip-host", entries))

		loaded, err := d.LoadRemoteSkippedFiles("roundtrip-host")
		require.NoError(t, err, "LoadRemoteSkippedFiles")
		assert.True(t, maps.Equal(loaded, entries),
			"loaded %v, want %v", loaded, entries)
	})

	t.Run("host isolation", func(t *testing.T) {
		require := require.New(t)

		entries := map[string]int64{
			"/a.jsonl": 100,
			"/b.jsonl": 200,
		}
		require.NoError(d.ReplaceRemoteSkippedFiles("isolation-host-1", entries))

		// Different host should return empty.
		loaded, err := d.LoadRemoteSkippedFiles("isolation-host-2")
		require.NoError(err, "LoadRemoteSkippedFiles isolation-host-2")
		require.Empty(loaded, "isolation-host-2 should be empty")

		// Original host still has its entries.
		loaded, err = d.LoadRemoteSkippedFiles("isolation-host-1")
		require.NoError(err, "LoadRemoteSkippedFiles isolation-host-1")
		assert.True(t, maps.Equal(loaded, entries),
			"isolation-host-1: loaded %v, want %v", loaded, entries)
	})

	t.Run("replace overwrites", func(t *testing.T) {
		require := require.New(t)

		first := map[string]int64{
			"/a.jsonl": 100,
			"/b.jsonl": 200,
		}
		require.NoError(d.ReplaceRemoteSkippedFiles("replace-host", first))

		// Replace with different entries.
		second := map[string]int64{
			"/c.jsonl": 300,
		}
		require.NoError(d.ReplaceRemoteSkippedFiles("replace-host", second))

		loaded, err := d.LoadRemoteSkippedFiles("replace-host")
		require.NoError(err, "LoadRemoteSkippedFiles")
		require.Len(loaded, 1)
		assert.Equal(t, int64(300), loaded["/c.jsonl"])
	})

	t.Run("replace does not affect other hosts", func(t *testing.T) {
		require := require.New(t)

		host1 := map[string]int64{"/a.jsonl": 100}
		host2 := map[string]int64{"/b.jsonl": 200}

		require.NoError(d.ReplaceRemoteSkippedFiles("replace-other-1", host1))
		require.NoError(d.ReplaceRemoteSkippedFiles("replace-other-2", host2))

		// Replace replace-other-1 with empty; replace-other-2 unaffected.
		require.NoError(d.ReplaceRemoteSkippedFiles("replace-other-1", map[string]int64{}))

		loaded1, err := d.LoadRemoteSkippedFiles("replace-other-1")
		require.NoError(err, "LoadRemoteSkippedFiles replace-other-1")
		require.Empty(loaded1, "replace-other-1 should be empty")

		loaded2, err := d.LoadRemoteSkippedFiles("replace-other-2")
		require.NoError(err, "LoadRemoteSkippedFiles replace-other-2")
		assert.True(t, maps.Equal(loaded2, host2),
			"replace-other-2: loaded %v, want %v", loaded2, host2)
	})

	t.Run("scoped load excludes unrelated rows", func(t *testing.T) {
		entries := map[string]int64{
			"/sessions/changed.jsonl":                     10,
			"/sessions/changed.jsonl?agent=claude":        11,
			"/sessions/changed.jsonl-other":               12,
			"/sessions/fallback/one.jsonl":                20,
			"/sessions/fallback/nested/two.jsonl?agent=x": 21,
			"/sessions/unrelated.jsonl":                   30,
		}
		require.NoError(t,
			d.ReplaceRemoteSkippedFiles("scoped-host", entries))

		loaded, err := d.LoadRemoteSkippedFilesForScopes(
			t.Context(), "scoped-host",
			[]string{"/sessions/changed.jsonl"},
			[]string{"/sessions/fallback"},
		)
		require.NoError(t, err)
		assert.Equal(t, map[string]int64{
			"/sessions/changed.jsonl":                     10,
			"/sessions/changed.jsonl?agent=claude":        11,
			"/sessions/fallback/one.jsonl":                20,
			"/sessions/fallback/nested/two.jsonl?agent=x": 21,
		}, loaded)
	})

	t.Run("scoped mutation preserves unrelated rows", func(t *testing.T) {
		require := require.New(t)

		require.NoError(d.ReplaceRemoteSkippedFiles(
			"mutate-host", map[string]int64{
				"/changed.jsonl":   10,
				"/unchanged.jsonl": 20,
			},
		))
		require.NoError(d.ApplyRemoteSkippedFileChanges(
			"mutate-host", []string{"/changed.jsonl"}, map[string]int64{
				"/new.jsonl": 30,
			},
		))

		loaded, err := d.LoadRemoteSkippedFiles("mutate-host")
		require.NoError(err)
		assert.Equal(t, map[string]int64{
			"/unchanged.jsonl": 20,
			"/new.jsonl":       30,
		}, loaded)
	})
}

func TestClearRemoteSkippedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := dbtest.OpenTestDB(t)

	require.NoError(d.ReplaceRemoteSkippedFiles(
		"host-a", map[string]int64{"/sessions/a.jsonl": 101},
	))
	require.NoError(d.ReplaceRemoteSkippedFiles(
		"host-b", map[string]int64{"/sessions/b.jsonl": 202},
	))

	require.NoError(d.ClearRemoteSkippedFiles("host-a"))

	hostA, err := d.LoadRemoteSkippedFiles("host-a")
	require.NoError(err, "LoadRemoteSkippedFiles host-a")
	assert.Empty(hostA)

	hostB, err := d.LoadRemoteSkippedFiles("host-b")
	require.NoError(err, "LoadRemoteSkippedFiles host-b")
	assert.Equal(map[string]int64{"/sessions/b.jsonl": 202}, hostB)
}
