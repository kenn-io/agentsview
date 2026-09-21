package db_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSkippedFiles_RoundTrip(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	// Initially empty.
	loaded, err := d.LoadSkippedFiles(t.Context())
	require.NoError(t, err, "LoadSkippedFiles")
	require.Empty(t, loaded)

	// Persist some entries.
	entries := map[string]int64{
		"/a/b/c.jsonl": 100,
		"/d/e/f.jsonl": 200,
		"/g/h/i.jsonl": 300,
	}
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), entries))

	// Load them back.
	loaded, err = d.LoadSkippedFiles(t.Context())
	require.NoError(t, err, "LoadSkippedFiles")
	assert.True(t, maps.Equal(loaded, entries),
		"loaded map %v, want %v", loaded, entries)
}

func TestSkippedFiles_ReplaceOverwrites(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	first := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), first))

	// Replace with different entries.
	second := map[string]int64{
		"/c.jsonl": 300,
	}
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), second))

	loaded, err := d.LoadSkippedFiles(t.Context())
	require.NoError(t, err, "LoadSkippedFiles")
	require.Len(t, loaded, 1)
	assert.Equal(t, int64(300), loaded["/c.jsonl"])
}

func TestSkippedFiles_DeleteSingle(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), entries))

	require.NoError(t, d.DeleteSkippedFile(t.Context(), "/a.jsonl"))

	loaded, err := d.LoadSkippedFiles(t.Context())
	require.NoError(t, err, "LoadSkippedFiles")
	require.Len(t, loaded, 1)
	_, ok := loaded["/a.jsonl"]
	assert.False(t, ok, "/a.jsonl should have been deleted")
	assert.Equal(t, int64(200), loaded["/b.jsonl"])
}

func TestSkippedFiles_DeleteNonexistent(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	// Should not error.
	require.NoError(t, d.DeleteSkippedFile(t.Context(), "/nope"))
}

func TestSkippedFiles_EmptyReplace(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{"/a.jsonl": 100}
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), entries))

	// Replace with empty map clears the table.
	require.NoError(t, d.ReplaceSkippedFiles(t.Context(), map[string]int64{}))

	loaded, err := d.LoadSkippedFiles(t.Context())
	require.NoError(t, err, "LoadSkippedFiles")
	require.Empty(t, loaded)
}
