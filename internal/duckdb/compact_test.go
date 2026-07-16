package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompareCompactRowCountsMatch(t *testing.T) {
	src := map[string]int64{}
	dst := map[string]int64{}
	for i, table := range mirrorTables {
		src[table.name] = int64(i)
		dst[table.name] = int64(i)
	}
	require.NoError(t, compareCompactRowCounts(src, dst))
}

func TestCompareCompactRowCountsDetectsMismatch(t *testing.T) {
	require.NotEmpty(t, mirrorTables, "mirror tables must be defined")
	src := map[string]int64{}
	dst := map[string]int64{}
	for _, table := range mirrorTables {
		src[table.name] = 7
		dst[table.name] = 7
	}
	// Drop a row from one table in the compacted copy.
	target := mirrorTables[0].name
	dst[target] = 6

	err := compareCompactRowCounts(src, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compaction verification failed")
	assert.Contains(t, err.Error(), target)
	assert.Contains(t, err.Error(), "original has 7 rows, compacted has 6")
}

func TestIsDuckDBFileLockError(t *testing.T) {
	assert.False(t, isDuckDBFileLockError(nil))
	assert.True(t, isDuckDBFileLockError(
		assertError("IO Error: Could not set lock on file"),
	))
	assert.True(t, isDuckDBFileLockError(
		assertError("Conflicting lock is held in /proc/123"),
	))
	assert.False(t, isDuckDBFileLockError(
		assertError("some unrelated error"),
	))
}

type stringError string

func (e stringError) Error() string { return string(e) }

func assertError(msg string) error { return stringError(msg) }
