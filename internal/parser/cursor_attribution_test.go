package parser

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCursorAttribution_Happy(t *testing.T) {
	path := seedCursorAttributionDBTest(t)
	t.Setenv("AGENTSVIEW_CURSOR_ATTRIBUTION_DB", path)

	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	got, err := LoadCursorAttribution(from, to)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, int64(2), got.ScoredCommits)
	assert.Equal(t, int64(18), got.LinesAdded)
	assert.Equal(t, int64(6), got.LinesDeleted)
	assert.Equal(t, int64(6), got.TabLinesAdded)
	assert.Equal(t, int64(4), got.ComposerLinesAdded)
	assert.Equal(t, int64(5), got.HumanLinesAdded)
	assert.Equal(t, int64(3), got.BlankLinesAdded)
	assert.InDelta(t, 10.0/18.0, got.AIAuthoredPct, 1e-9)
	require.Len(t, got.ConversationCounts, 2)
	assert.Equal(t, "model-a", got.ConversationCounts[0].Model)
	assert.Equal(t, "composer", got.ConversationCounts[0].Mode)
	assert.Equal(t, int64(3), got.ConversationCounts[0].Count)
}

func TestLoadCursorAttribution_MissingDB(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	t.Setenv("AGENTSVIEW_CURSOR_ATTRIBUTION_DB", missing)

	got, err := LoadCursorAttribution(
		time.Unix(0, 0).UTC(),
		time.Unix(0, 1).UTC(),
	)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLoadCursorAttribution_NormalizesEmptyConversationKeys(t *testing.T) {
	path := seedCursorAttributionDBTest(t)
	t.Setenv("AGENTSVIEW_CURSOR_ATTRIBUTION_DB", path)

	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ts := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC).UnixMilli()
	_, err = conn.Exec(`
		INSERT INTO conversation_summaries (model, mode, updatedAt) VALUES
			(NULL, NULL, ?),
			('', '', ?)
	`, ts, ts)
	require.NoError(t, err)

	got, err := LoadCursorAttribution(
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.NotNil(t, got)

	emptyRows := 0
	for _, entry := range got.ConversationCounts {
		if entry.Model == "" && entry.Mode == "" {
			emptyRows++
			assert.Equal(t, int64(2), entry.Count)
		}
	}
	assert.Equal(t, 1, emptyRows)
}

func seedCursorAttributionDBTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ai-code-tracking.db")
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Exec(`
		CREATE TABLE scored_commits (
			commitHash TEXT PRIMARY KEY,
			scoredAt INTEGER NOT NULL,
			commitDate INTEGER NOT NULL,
			linesAdded INTEGER NOT NULL DEFAULT 0,
			linesDeleted INTEGER NOT NULL DEFAULT 0,
			tabLinesAdded INTEGER NOT NULL DEFAULT 0,
			tabLinesDeleted INTEGER NOT NULL DEFAULT 0,
			composerLinesAdded INTEGER NOT NULL DEFAULT 0,
			composerLinesDeleted INTEGER NOT NULL DEFAULT 0,
			humanLinesAdded INTEGER NOT NULL DEFAULT 0,
			humanLinesDeleted INTEGER NOT NULL DEFAULT 0,
			blankLinesAdded INTEGER NOT NULL DEFAULT 0,
			blankLinesDeleted INTEGER NOT NULL DEFAULT 0
		)
	`)
	require.NoError(t, err)
	_, err = conn.Exec(`
		CREATE TABLE conversation_summaries (
			model TEXT,
			mode TEXT,
			updatedAt INTEGER NOT NULL
		)
	`)
	require.NoError(t, err)

	first := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	second := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC).UnixMilli()

	_, err = conn.Exec(`
		INSERT INTO scored_commits (
			commitHash, scoredAt, commitDate,
			linesAdded, linesDeleted,
			tabLinesAdded, tabLinesDeleted,
			composerLinesAdded, composerLinesDeleted,
			humanLinesAdded, humanLinesDeleted,
			blankLinesAdded, blankLinesDeleted
		) VALUES
			('c1', ?, ?, 12, 4, 6, 1, 3, 1, 1, 2, 2, 0),
			('c2', ?, ?, 6, 2, 0, 0, 1, 0, 4, 1, 1, 0),
			('c3', ?, ?, 99, 1, 50, 0, 40, 0, 9, 0, 0, 0)
	`,
		time.Date(2026, 5, 31, 23, 0, 0, 0, time.UTC).UnixMilli(),
		first,
		time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC).UnixMilli(),
		second,
		first,
		time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC).UnixMilli(),
	)
	require.NoError(t, err)
	_, err = conn.Exec(`
		INSERT INTO conversation_summaries (model, mode, updatedAt) VALUES
			('model-a', 'composer', ?),
			('model-a', 'composer', ?),
			('model-a', 'composer', ?),
			('model-a', 'tab', ?)
	`, first, second, second, second)
	require.NoError(t, err)
	return path
}
