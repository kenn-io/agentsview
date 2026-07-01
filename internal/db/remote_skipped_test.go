package db_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRemoteSkippedFiles_InitiallyEmpty(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	require.NoError(t, err, "LoadRemoteSkippedFiles")
	require.Empty(t, loaded)
}

func TestRemoteSkippedFiles_RoundTrip(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/home/user/.claude/sessions/a.jsonl": 1000,
		"/home/user/.claude/sessions/b.jsonl": 2000,
		"/home/user/.claude/sessions/c.jsonl": 3000,
	}
	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox1", entries))

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	require.NoError(t, err, "LoadRemoteSkippedFiles")
	assert.True(t, maps.Equal(loaded, entries),
		"loaded %v, want %v", loaded, entries)
}

func TestRemoteSkippedFiles_HostIsolation(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox1", entries))

	// Different host should return empty.
	loaded, err := d.LoadRemoteSkippedFiles("devbox2")
	require.NoError(t, err, "LoadRemoteSkippedFiles devbox2")
	require.Empty(t, loaded, "devbox2 should be empty")

	// Original host still has its entries.
	loaded, err = d.LoadRemoteSkippedFiles("devbox1")
	require.NoError(t, err, "LoadRemoteSkippedFiles devbox1")
	assert.True(t, maps.Equal(loaded, entries),
		"devbox1: loaded %v, want %v", loaded, entries)
}

func TestRemoteSkippedFiles_ReplaceOverwrites(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)

	first := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox1", first))

	// Replace with different entries.
	second := map[string]int64{
		"/c.jsonl": 300,
	}
	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox1", second))

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	require.NoError(t, err, "LoadRemoteSkippedFiles")
	require.Len(t, loaded, 1)
	assert.Equal(t, int64(300), loaded["/c.jsonl"])
}

func TestRemoteSkippedFiles_ReplaceDoesNotAffectOtherHosts(
	t *testing.T,
) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)

	host1 := map[string]int64{"/a.jsonl": 100}
	host2 := map[string]int64{"/b.jsonl": 200}

	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox1", host1))
	require.NoError(t, d.ReplaceRemoteSkippedFiles("devbox2", host2))

	// Replace devbox1 with empty — devbox2 unaffected.
	require.NoError(t,
		d.ReplaceRemoteSkippedFiles("devbox1", map[string]int64{}))

	loaded1, err := d.LoadRemoteSkippedFiles("devbox1")
	require.NoError(t, err, "LoadRemoteSkippedFiles devbox1")
	require.Empty(t, loaded1, "devbox1 should be empty")

	loaded2, err := d.LoadRemoteSkippedFiles("devbox2")
	require.NoError(t, err, "LoadRemoteSkippedFiles devbox2")
	assert.True(t, maps.Equal(loaded2, host2),
		"devbox2: loaded %v, want %v", loaded2, host2)
}
