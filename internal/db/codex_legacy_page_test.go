package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	legacyThread = "11111111-1111-4111-8111-111111111111"
	legacyRoll1  = "22222222-2222-4222-8222-222222222222"
	legacyRoll2  = "33333333-3333-4333-8333-333333333333"
)

func legacyPageFile(rollout string) *string {
	p := "/sessions/rollout-2026-09-25T12-00-00-" + legacyThread + "_" + rollout + ".jsonl"
	return &p
}

// legacySource builds a 133 archive holding two legacy pages, a child that
// continued from page 1, and curation keyed by the legacy page IDs.
func legacySource(t *testing.T, srcPath string) {
	t.Helper()
	ctx := t.Context()
	src := testDBAtPath(t, srcPath, "src")
	for _, id := range []string{"codex:" + legacyRoll1, "codex:" + legacyRoll2} {
		rollout := id[len("codex:"):]
		insertSession(t, src, id, "project", func(s *Session) {
			s.Agent = "codex"
			s.FilePath = legacyPageFile(rollout)
			if rollout == legacyRoll2 {
				s.ParentSessionID = new("codex:" + legacyRoll1)
			}
		})
		insertMessages(t, src, Message{SessionID: id, Role: "assistant", Content: "page " + rollout})
	}
	insertSession(t, src, "codex:child", "project", func(s *Session) {
		s.Agent = "codex"
		s.ParentSessionID = new("codex:" + legacyRoll1)
	})
	insertSession(t, src, "codex:launcher", "project", func(s *Session) { s.Agent = "codex" })
	for _, q := range []string{
		`INSERT INTO session_labels(session_id,label) VALUES ('codex:` + legacyRoll1 + `','keep')`,
		`INSERT INTO session_external_parents(session_id,parent_session_id) VALUES ('codex:` + legacyRoll2 + `','codex:` + legacyRoll1 + `')`,
		`INSERT INTO session_external_parents(session_id,parent_session_id) VALUES ('codex:launcher','codex:` + legacyRoll1 + `')`,
		`PRAGMA user_version = 133`,
	} {
		_, err := src.getWriter().Exec(ctx, q)
		require.NoError(t, err)
	}
	require.NoError(t, src.Close())
}

func migrateLegacySource(t *testing.T, liveSourceIDs ...string) *DB {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	legacySource(t, srcPath)
	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	t.Cleanup(func() { dst.Close() })
	insertSession(t, dst, "codex:"+legacyThread, "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, dst, "codex:launcher", "project", func(s *Session) { s.Agent = "codex" })
	for _, id := range liveSourceIDs {
		insertSession(t, dst, id, "project", func(s *Session) { s.Agent = "codex" })
	}
	_, err := dst.CopyOrphanedDataFromExcluding(srcPath, nil)
	require.NoError(t, err)
	require.NoError(t, dst.CopySessionMetadataFrom(srcPath))
	return dst
}

func TestLegacyCodexPageMigrationRemapsRelationships(t *testing.T) {
	dst := migrateLegacySource(t)
	ctx := t.Context()
	p1 := "codex:" + legacyThread + "_" + legacyRoll1
	p2 := "codex:" + legacyThread + "_" + legacyRoll2

	for _, id := range []string{p1, p2} {
		s, err := dst.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, s, id)
	}
	gone, err := dst.GetSession(ctx, "codex:"+legacyRoll1)
	require.NoError(t, err)
	assert.Nil(t, gone, "retired ID must not survive")

	child, err := dst.GetSession(ctx, "codex:child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, p1, *child.ParentSessionID)

	p2s, err := dst.GetSession(ctx, "codex:"+legacyThread+"_"+legacyRoll2)
	require.NoError(t, err)
	require.NotNil(t, p2s.ParentSessionID)
	assert.Equal(t, p1, *p2s.ParentSessionID, "page continuing a legacy page")

	var parent string
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT COALESCE(parent_session_id,'') FROM sessions WHERE id = ?`, p1,
	).Scan(&parent))
	assert.Equal(t, "codex:"+legacyThread, parent, "first page continues its thread")
}

func TestLegacyCodexPageMigrationRemapsLabelsAndExternalParents(t *testing.T) {
	ctx := t.Context()
	dst := migrateLegacySource(t)
	p1 := "codex:" + legacyThread + "_" + legacyRoll1
	p2 := "codex:" + legacyThread + "_" + legacyRoll2

	var n int
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT count(*) FROM session_labels WHERE session_id = ? AND label = 'keep'`, p1).Scan(&n))
	assert.Equal(t, 1, n, "label follows the canonical page")
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT count(*) FROM session_labels WHERE session_id = ?`, "codex:"+legacyRoll1).Scan(&n))
	assert.Zero(t, n)

	var got string
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT parent_session_id FROM session_external_parents WHERE session_id = ?`, p2).Scan(&got))
	assert.Equal(t, p1, got, "referencing and referenced side both remapped")
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT parent_session_id FROM session_external_parents WHERE session_id = 'codex:launcher'`).Scan(&got))
	assert.Equal(t, p1, got, "referenced side remapped for a live session")
}

func TestLegacyCodexPageMigrationSkipsPageParserRejected(t *testing.T) {
	// The fresh parse kept codex:R1 as a plain session, so the source ID is
	// live and no canonical duplicate may appear.
	dst := migrateLegacySource(t, "codex:"+legacyRoll1)
	ctx := t.Context()
	dup, err := dst.GetSession(ctx, "codex:"+legacyThread+"_"+legacyRoll1)
	require.NoError(t, err)
	assert.Nil(t, dup)
	var n int
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT count(*) FROM session_labels WHERE session_id = ?`, "codex:"+legacyRoll1).Scan(&n))
	assert.Equal(t, 1, n, "curation stays on the live ID")
	other, err := dst.GetSession(ctx, "codex:"+legacyThread+"_"+legacyRoll2)
	require.NoError(t, err)
	assert.NotNil(t, other, "unaffected page still migrates")
}

// The page lookup behind index renames must search the id index. A scan would
// make every rename cost the size of the archive.
func TestCodexRevertPageLookupSearchesIDIndex(t *testing.T) {
	d := testDBAtPath(t, filepath.Join(t.TempDir(), "plan.db"), "plan")
	defer d.Close()
	rows, err := d.getReader().QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN "+codexRevertPageSessionIDsSQL, "codex:x_", "codex:x`")
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan += detail + "\n"
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan, "SEARCH sessions")
	assert.NotContains(t, plan, "SCAN sessions")
}
