//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const reTestSchema = "agentsview_recentedits_test"

func reEnsureSchema(t *testing.T, pgURL string) *sql.DB {
	t.Helper()
	pg, err := Open(pgURL, reTestSchema, true)
	require.NoError(t, err, "Open")
	ctx := context.Background()
	_, err = pg.ExecContext(ctx,
		`DROP SCHEMA IF EXISTS `+reTestSchema+` CASCADE`,
	)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, reTestSchema), "EnsureSchema")
	return pg
}

// reInsertSession inserts a session into the test schema.
func reInsertSession(
	t *testing.T, pg *sql.DB,
	id, project, deletedAt string,
) {
	t.Helper()
	var da any
	if deletedAt != "" {
		da = deletedAt
	}
	_, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent,
			 message_count, user_message_count, deleted_at)
		VALUES ($1, 'test', $2, 'claude', 1, 1, $3::timestamptz)
	`, id, project, da)
	require.NoError(t, err, "insert session %s", id)
}

// reInsertMessage inserts a message and returns its ordinal.
func reInsertMessage(
	t *testing.T, pg *sql.DB,
	sessionID string, ordinal int, ts string,
) {
	t.Helper()
	var tsAny any
	if ts != "" {
		tsAny = ts
	}
	_, err := pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES ($1, $2, 'assistant', '[Edit]', $3::timestamptz, 6)
	`, sessionID, ordinal, tsAny)
	require.NoError(t, err, "insert message %s/%d", sessionID, ordinal)
}

// reInsertToolCall inserts one Edit tool_call row.
func reInsertToolCall(
	t *testing.T, pg *sql.DB,
	sessionID string, msgOrdinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	var fp any
	if filePath != "" {
		fp = filePath
	}
	_, err := pg.Exec(`
		INSERT INTO tool_calls
			(session_id, tool_name, category, call_index,
			 message_ordinal, file_path)
		VALUES ($1, 'Edit', 'Edit', $2, $3, $4)
	`, sessionID, callIndex, msgOrdinal, fp)
	require.NoError(t, err,
		"insert tool_call %s/%d ci=%d fp=%s", sessionID, msgOrdinal, callIndex, filePath)
}

// reSeedEdit seeds one session + message + tool_call in one call.
func reSeedEdit(
	t *testing.T, pg *sql.DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	reInsertSession(t, pg, sessionID, project, "")
	reInsertMessage(t, pg, sessionID, ordinal, ts)
	reInsertToolCall(t, pg, sessionID, ordinal, callIndex, filePath, ts)
}

// reSeedEditTrashed seeds a trashed session.
func reSeedEditTrashed(
	t *testing.T, pg *sql.DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	deletedAt := time.Now().UTC().Format(time.RFC3339)
	reInsertSession(t, pg, sessionID, project, deletedAt)
	reInsertMessage(t, pg, sessionID, ordinal, ts)
	reInsertToolCall(t, pg, sessionID, ordinal, callIndex, filePath, ts)
}

func reNewStore(t *testing.T, pgURL string) *Store {
	t.Helper()
	store, err := NewStore(pgURL, reTestSchema, true)
	require.NoError(t, err, "NewStore")
	return store
}

// reUpsertSession is used when the same sessionID needs multiple messages —
// the INSERT above has no ON CONFLICT, so call this before the second seed.
func reUpsertSession(t *testing.T, pg *sql.DB, id, project string) {
	t.Helper()
	_, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent,
			 message_count, user_message_count)
		VALUES ($1, 'test', $2, 'claude', 1, 1)
		ON CONFLICT (id) DO NOTHING
	`, id, project)
	require.NoError(t, err, "upsert session %s", id)
}

// reAddMessage adds an additional message + tool_call to an already-inserted
// session (used when building multi-edit scenarios).
func reAddEdit(
	t *testing.T, pg *sql.DB,
	sessionID string, ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	reInsertMessage(t, pg, sessionID, ordinal, ts)
	reInsertToolCall(t, pg, sessionID, ordinal, callIndex, filePath, ts)
}

// TestPGRecentEditsGroupingAndOrdering mirrors TestRecentEditsGroupingAndOrdering.
func TestPGRecentEditsGroupingAndOrdering(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	// projA: same session, two messages editing config.go.
	reUpsertSession(t, pg, "sA1", "projA")
	reAddEdit(t, pg, "sA1", 1, 0, "config.go", "2026-06-24T10:00:00Z")
	reAddEdit(t, pg, "sA1", 2, 0, "config.go", "2026-06-24T12:00:00Z")
	// projB: one message editing config.go.
	reSeedEdit(t, pg, "projB", "sB1", 1, 0, "config.go", "2026-06-24T11:00:00Z")

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "same relative path in two projects = 2 rows")
	assert.Equal(t, "projA", res.Files[0].Project)
	assert.Equal(t, "config.go", res.Files[0].FilePath)
	assert.Equal(t, 2, res.Files[0].EditCount)
	assert.Equal(t, "projB", res.Files[1].Project)
	assert.Equal(t, "config.go", res.Files[1].FilePath)
	assert.Equal(t, 1, res.Files[1].EditCount)
	assert.False(t, res.HasMore)
}

// TestPGRecentEditsExcludesTrash mirrors TestRecentEditsExcludesTrash.
func TestPGRecentEditsExcludesTrash(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	reSeedEdit(t, pg, "proj", "sLive", 1, 0, "main.go", "2026-06-24T10:00:00Z")
	reSeedEditTrashed(t, pg, "proj", "sDead", 1, 0, "deleted.go", "2026-06-24T11:00:00Z")

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "trashed session's edits must be excluded")
	assert.Equal(t, "main.go", res.Files[0].FilePath)
}

// TestPGRecentEditsProjectFilter mirrors TestRecentEditsProjectFilter.
func TestPGRecentEditsProjectFilter(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	reSeedEdit(t, pg, "alpha", "sAlpha", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	reSeedEdit(t, pg, "beta", "sBeta", 1, 0, "b.go", "2026-06-24T10:00:00Z")

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Project: "alpha"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project filter should narrow to alpha only")
	assert.Equal(t, "alpha", res.Files[0].Project)
	assert.Equal(t, "a.go", res.Files[0].FilePath)
}

// TestPGRecentEditsSearchFilter mirrors TestRecentEditsSearchFilter.
func TestPGRecentEditsSearchFilter(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	reSeedEdit(t, pg, "proj", "s1", 1, 0, "internal/db/Recent.go", "2026-06-24T10:00:00Z")
	reSeedEdit(t, pg, "proj", "s2", 1, 0, "internal/server/handler.go", "2026-06-24T09:00:00Z")
	reSeedEdit(t, pg, "proj", "s3", 1, 0, "x/a_b.go", "2026-06-24T08:00:00Z")
	reSeedEdit(t, pg, "proj", "s4", 1, 0, "x/axb.go", "2026-06-24T07:00:00Z")
	reSeedEdit(t, pg, "other", "s5", 1, 0, "internal/db/Recent.go", "2026-06-24T06:00:00Z")

	store := reNewStore(t, pgURL)
	defer store.Close()

	// Case-insensitive substring over the full path (ILIKE), across projects.
	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "matches Recent.go in both projects case-insensitively")

	// Matches a directory segment, not just the basename.
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Search: "server/"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	assert.Equal(t, "internal/server/handler.go", res.Files[0].FilePath)

	// Underscores are literal, not wildcards (db.EscapeLikePattern + ESCAPE).
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Search: "a_b"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "escaped underscore matches literally, not as a wildcard")
	assert.Equal(t, "x/a_b.go", res.Files[0].FilePath)

	// Project and search compose.
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Project: "proj", Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project and search both apply")
	assert.Equal(t, "proj", res.Files[0].Project)
}

// TestPGRecentEditsTruncationAndHasMore mirrors TestRecentEditsTruncationAndHasMore.
func TestPGRecentEditsTruncationAndHasMore(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	reSeedEdit(t, pg, "proj", "s1", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	reSeedEdit(t, pg, "proj", "s2", 1, 0, "b.go", "2026-06-24T09:00:00Z")
	reSeedEdit(t, pg, "proj", "s3", 1, 0, "c.go", "2026-06-24T08:00:00Z")

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, res.Files, 2, "only Limit files should be returned")
	assert.True(t, res.HasMore, "HasMore should be true when more files exist")

	// Second edit on a.go => EditsTruncated when MaxEditsPerFile=1.
	reUpsertSession(t, pg, "s1", "proj")
	reAddEdit(t, pg, "s1", 2, 0, "a.go", "2026-06-24T11:00:00Z")

	res2, err := store.RecentEdits(ctx, db.RecentEditsParams{MaxEditsPerFile: 1})
	require.NoError(t, err)
	var found bool
	for _, f := range res2.Files {
		if f.FilePath == "a.go" {
			assert.True(t, f.EditsTruncated, "a.go has 2 edits, MaxEditsPerFile=1 => truncated")
			assert.Len(t, f.Edits, 1, "only 1 edit inlined")
			found = true
		}
	}
	assert.True(t, found, "a.go should appear in result")
}

// TestPGRecentEditsNullTimestampsSortLast mirrors TestRecentEditsNullTimestampsSortLast.
func TestPGRecentEditsNullTimestampsSortLast(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	reSeedEdit(t, pg, "proj", "sReal", 1, 0, "real.go", "2026-06-24T10:00:00Z")
	// Empty ts → NULL timestamp in both message and tool_call.
	reSeedEdit(t, pg, "proj", "sNull", 1, 0, "null.go", "")

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "both files should appear")
	assert.Equal(t, "real.go", res.Files[0].FilePath, "timestamped file first")
	assert.Equal(t, "null.go", res.Files[1].FilePath, "null-timestamp file last")
}

// TestPGRecentEditsTieByCallIndex mirrors TestRecentEditsTieByCallIndex.
func TestPGRecentEditsTieByCallIndex(t *testing.T) {
	pgURL := testPGURL(t)
	pg := reEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(
			`DROP SCHEMA IF EXISTS ` + reTestSchema + ` CASCADE`,
		)
	}()
	ctx := context.Background()

	// Insert session and one message.
	reUpsertSession(t, pg, "sTie", "proj")
	ts := "2026-06-24T10:00:00Z"
	reInsertMessage(t, pg, "sTie", 1, ts)
	// Two tool_calls at same ordinal with different call_index.
	reInsertToolCall(t, pg, "sTie", 1, 0, "tie.go", ts)
	reInsertToolCall(t, pg, "sTie", 1, 1, "tie.go", ts)

	store := reNewStore(t, pgURL)
	defer store.Close()

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{MaxEditsPerFile: 5})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	require.Len(t, res.Files[0].Edits, 2, "both edits inlined")
	assert.Equal(t, 1, res.Files[0].Edits[0].CallIndex,
		"higher call_index ranks first")
	assert.Equal(t, 0, res.Files[0].Edits[1].CallIndex)
}
