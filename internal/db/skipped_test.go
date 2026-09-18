package db_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSkippedFiles_RoundTrip(t *testing.T) {
	require := require.New(t)

	d := dbtest.OpenTestDB(t)

	// Initially empty.
	loaded, err := d.LoadSkippedFiles()
	require.NoError(err, "LoadSkippedFiles")
	require.Empty(loaded)

	// Persist some entries.
	entries := map[string]int64{
		"/a/b/c.jsonl": 100,
		"/d/e/f.jsonl": 200,
		"/g/h/i.jsonl": 300,
	}
	require.NoError(d.ReplaceSkippedFiles(entries))

	// Load them back.
	loaded, err = d.LoadSkippedFiles()
	require.NoError(err, "LoadSkippedFiles")
	assert.True(t, maps.Equal(loaded, entries),
		"loaded map %v, want %v", loaded, entries)
}

func TestSkippedFiles_ReplaceOverwrites(t *testing.T) {
	require := require.New(t)

	d := dbtest.OpenTestDB(t)

	first := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(d.ReplaceSkippedFiles(first))

	// Replace with different entries.
	second := map[string]int64{
		"/c.jsonl": 300,
	}
	require.NoError(d.ReplaceSkippedFiles(second))

	loaded, err := d.LoadSkippedFiles()
	require.NoError(err, "LoadSkippedFiles")
	require.Len(loaded, 1)
	assert.Equal(t, int64(300), loaded["/c.jsonl"])
}

func TestSkippedFiles_DeleteSingle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	require.NoError(d.ReplaceSkippedFiles(entries))

	require.NoError(d.DeleteSkippedFile("/a.jsonl"))

	loaded, err := d.LoadSkippedFiles()
	require.NoError(err, "LoadSkippedFiles")
	require.Len(loaded, 1)
	_, ok := loaded["/a.jsonl"]
	assert.False(ok, "/a.jsonl should have been deleted")
	assert.Equal(int64(200), loaded["/b.jsonl"])
}

func TestSkippedFiles_DeleteNonexistent(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	// Should not error.
	require.NoError(t, d.DeleteSkippedFile("/nope"))
}

func TestSkippedFiles_EmptyReplace(t *testing.T) {
	require := require.New(t)

	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{"/a.jsonl": 100}
	require.NoError(d.ReplaceSkippedFiles(entries))

	// Replace with empty map clears the table.
	require.NoError(d.ReplaceSkippedFiles(map[string]int64{}))

	loaded, err := d.LoadSkippedFiles()
	require.NoError(err, "LoadSkippedFiles")
	require.Empty(loaded)
}
