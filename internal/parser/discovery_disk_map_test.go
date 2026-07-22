package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryDiskMapGetDistinguishesAbsenceAndFailure(t *testing.T) {
	index, err := newDiscoveryDiskMap()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Remove(index.path)
		_ = os.Remove(index.path + "-wal")
		_ = os.Remove(index.path + "-shm")
	})

	value, found, err := index.get(t.Context(), "missing")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, value)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = index.get(ctx, "missing")
	assert.ErrorIs(t, err, context.Canceled)

	require.NoError(t, index.db.Close())
	_, _, err = index.get(t.Context(), "missing")
	require.Error(t, err)
	assert.NotErrorIs(t, err, context.Canceled)
}

func TestDiscoveryDiskMapCloseReportsCleanupFailure(t *testing.T) {
	index, err := newDiscoveryDiskMap()
	require.NoError(t, err)
	injected := errors.New("remove discovery index failed")
	index.remove = func(path string) error {
		if path == index.path {
			return injected
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { _ = os.Remove(index.path) })

	err = index.close()

	assert.ErrorIs(t, err, injected)
}

func TestDiscoveryDiskMapAppendDoesNotRewriteAccumulatedValue(t *testing.T) {
	index, err := newDiscoveryDiskMap()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.close()) })
	_, err = index.db.Exec(`
		CREATE TABLE IF NOT EXISTS appended_entries (
			ordinal INTEGER PRIMARY KEY,
			key TEXT NOT NULL,
			value TEXT NOT NULL
		);
		CREATE TABLE append_write_cost (bytes INTEGER NOT NULL);
		CREATE TRIGGER measure_append_insert
		AFTER INSERT ON entries WHEN NEW.key = 'spans'
		BEGIN
			INSERT INTO append_write_cost VALUES (length(NEW.value));
		END;
		CREATE TRIGGER measure_append_update
		AFTER UPDATE OF value ON entries WHEN NEW.key = 'spans'
		BEGIN
			INSERT INTO append_write_cost VALUES (length(NEW.value));
		END;
		CREATE TRIGGER measure_child_append_insert
		AFTER INSERT ON appended_entries WHEN NEW.key = 'spans'
		BEGIN
			INSERT INTO append_write_cost VALUES (length(NEW.value));
		END;
	`)
	require.NoError(t, err)

	const spanCount = 64
	values := make([]string, 0, spanCount)
	var inputBytes int64
	for i := range spanCount {
		value := fmt.Sprintf("%03d:%s", i, strings.Repeat("x", 256))
		values = append(values, value)
		inputBytes += int64(len(value))
		require.NoError(t, index.append(t.Context(), "spans", value))
	}

	got, found, err := index.get(t.Context(), "spans")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, values, strings.Split(got, "\n"),
		"append retrieval must preserve insertion order")
	var writtenValueBytes int64
	require.NoError(t, index.db.QueryRow(
		"SELECT COALESCE(SUM(bytes), 0) FROM append_write_cost",
	).Scan(&writtenValueBytes))
	assert.LessOrEqual(t, writtenValueBytes, inputBytes+spanCount-1,
		"append work must stay linear in newly supplied value bytes")
	assert.GreaterOrEqual(t, writtenValueBytes, inputBytes,
		"the measurement must account for every appended value")
}

func TestDiscoveryDiskMapPutAndAppendPreserveMapSemantics(t *testing.T) {
	newIndex := func(t *testing.T) *discoveryDiskMap {
		t.Helper()
		index, err := newDiscoveryDiskMap()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, index.close()) })
		return index
	}

	t.Run("put ignore keeps appended value", func(t *testing.T) {
		index := newIndex(t)
		require.NoError(t, index.append(t.Context(), "key", "first"))
		require.NoError(t, index.append(t.Context(), "key", "second"))
		require.NoError(t, index.put(t.Context(), "key", "replacement", false))

		value, found, err := index.get(t.Context(), "key")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "first\nsecond", value)
	})

	t.Run("put replace discards appended value", func(t *testing.T) {
		index := newIndex(t)
		require.NoError(t, index.append(t.Context(), "key", "first"))
		require.NoError(t, index.append(t.Context(), "key", "second"))
		require.NoError(t, index.put(t.Context(), "key", "replacement", true))

		value, found, err := index.get(t.Context(), "key")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "replacement", value)
	})

	t.Run("append extends put value", func(t *testing.T) {
		index := newIndex(t)
		require.NoError(t, index.put(t.Context(), "key", "first", true))
		require.NoError(t, index.append(t.Context(), "key", "second"))

		value, found, err := index.get(t.Context(), "key")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "first\nsecond", value)
	})

	t.Run("put if absent sees appended value", func(t *testing.T) {
		index := newIndex(t)
		require.NoError(t, index.append(t.Context(), "key", "first"))
		require.NoError(t, index.append(t.Context(), "key", "second"))
		inserted, err := index.putIfAbsent(t.Context(), "key", "replacement")
		require.NoError(t, err)
		assert.False(t, inserted)

		value, found, err := index.get(t.Context(), "key")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "first\nsecond", value)
	})
}

func TestDiscoveryDiskMapForEachIncludesAppendedValuesInKeyOrder(t *testing.T) {
	index, err := newDiscoveryDiskMap()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.close()) })
	require.NoError(t, index.append(t.Context(), "beta", "first"))
	require.NoError(t, index.put(t.Context(), "alpha", "only", true))
	require.NoError(t, index.append(t.Context(), "beta", "second"))

	var got []string
	require.NoError(t, index.forEach(t.Context(), func(key, value string) error {
		got = append(got, key+"="+value)
		return nil
	}))

	assert.Equal(t, []string{"alpha=only", "beta=first\nsecond"}, got)
}

func TestDiscoveryDiskMapForEachUsesStoredIndexOrder(t *testing.T) {
	index, err := newDiscoveryDiskMap()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.close()) })
	for i := range 256 {
		key := fmt.Sprintf("key-%03d", i)
		require.NoError(t, index.put(t.Context(), key, "base", true))
		require.NoError(t, index.append(t.Context(), key, "appended"))
	}
	_, err = index.db.Exec("ANALYZE")
	require.NoError(t, err)

	rows, err := index.db.Query(
		"EXPLAIN QUERY PLAN " + discoveryDiskMapForEachQuery,
	)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, details)
	assert.NotContains(t, strings.ToUpper(strings.Join(details, "\n")),
		"USE TEMP B-TREE",
		"archive-wide iteration must stream in stored index order")
}
