package db

import (
	"net/url"
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

func migrateLegacySource(t *testing.T, liveSourceIDs ...string) *DB {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	for _, rollout := range []string{legacyRoll1, legacyRoll2} {
		insertSession(t, src, "codex:"+rollout, "project", func(s *Session) {
			s.Agent = "codex"
			s.FilePath = legacyPageFile(rollout)
		})
	}
	for _, q := range []string{
		`INSERT INTO session_labels(session_id,label) VALUES ('codex:` + legacyRoll1 + `','keep')`,
		`PRAGMA user_version = 133`,
	} {
		_, err := src.getWriter().Exec(t.Context(), q)
		require.NoError(t, err)
	}
	require.NoError(t, src.Close())

	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	t.Cleanup(func() { dst.Close() })
	insertSession(t, dst, "codex:"+legacyThread, "project", func(s *Session) { s.Agent = "codex" })
	for _, id := range liveSourceIDs {
		insertSession(t, dst, id, "project", func(s *Session) { s.Agent = "codex" })
	}
	_, err := dst.CopyOrphanedDataFromExcluding(srcPath, nil)
	require.NoError(t, err)
	require.NoError(t, dst.CopySessionMetadataFrom(srcPath))
	return dst
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

func TestLegacyCodexPageMigrationKeepsCurationAndRawArchiveBinding(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	canonical2 := "codex:" + legacyThread + "_" + legacyRoll2
	// Roll1 is a foreign raw-archive page; Roll2 has a legacy and a canonical row.
	archivePath := "archive://root1/" + url.PathEscape("2026/09/rollout-2026-09-25T12-00-00-"+legacyThread+"_"+legacyRoll1+".jsonl")
	insertSession(t, src, "dev~codex:"+legacyRoll1, "project", func(s *Session) {
		s.Agent = "codex"
		s.Machine = "dev"
		s.FilePath = &archivePath
	})
	for _, id := range []string{"codex:" + legacyRoll2, canonical2} {
		insertSession(t, src, id, "project", func(s *Session) {
			s.Agent = "codex"
			s.FilePath = legacyPageFile(legacyRoll2)
		})
	}
	for _, q := range []string{
		`INSERT INTO raw_archive_roots VALUES ('root1','dev','dev','codex','/x','cfg')`,
		`INSERT INTO raw_archive_sessions VALUES ('dev','codex','codex:` + legacyRoll1 + `','dev~codex:` + legacyRoll1 + `','root1','k')`,
		`INSERT INTO starred_sessions(session_id) VALUES ('codex:` + legacyRoll2 + `')`,
		`INSERT INTO session_labels(session_id,label) VALUES ('` + canonical2 + `','keep')`,
		`PRAGMA user_version = 133`,
	} {
		_, err := src.getWriter().Exec(t.Context(), q)
		require.NoError(t, err)
	}
	require.NoError(t, src.Close())

	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	t.Cleanup(func() { dst.Close() })
	insertSession(t, dst, "codex:"+legacyThread, "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, dst, canonical2, "project", func(s *Session) { s.Agent = "codex" })
	require.NoError(t, dst.CopySyncStateFrom(srcPath))
	_, err := dst.CopyOrphanedDataFromExcluding(srcPath, nil)
	require.NoError(t, err)
	require.NoError(t, dst.CopySessionMetadataFrom(srcPath))

	ctx := t.Context()
	var count int
	for query, args := range map[string][]any{
		`SELECT count(*) FROM starred_sessions WHERE session_id = ?`: {canonical2},
		`SELECT count(*) FROM session_labels WHERE session_id = ?`:   {canonical2},
		`SELECT count(*) FROM sessions WHERE id = ?`:                 {"dev~codex:" + legacyThread + "_" + legacyRoll1},
		`SELECT count(*) FROM raw_archive_sessions WHERE parser_id = ? AND session_id = ?`: {
			"codex:" + legacyThread + "_" + legacyRoll1, "dev~codex:" + legacyThread + "_" + legacyRoll1,
		},
	} {
		require.NoError(t, dst.getReader().QueryRow(ctx, query, args...).Scan(&count))
		assert.Equal(t, 1, count, query)
	}
}

func TestLegacyCodexTrashedPageLinksToThreadParsedAfterCopy(t *testing.T) {
	// A trashed page whose rollout file is gone is copied before the parse and
	// never reparsed, so its thread link must not wait for the thread row.
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	insertSession(t, src, "codex:"+legacyRoll1, "project", func(s *Session) {
		s.Agent = "codex"
		s.FilePath = legacyPageFile(legacyRoll1)
	})
	ctx := t.Context()
	require.NoError(t, src.SoftDeleteSession(ctx, "codex:"+legacyRoll1))
	_, err := src.getWriter().Exec(ctx, `PRAGMA user_version = 133`)
	require.NoError(t, err)
	require.NoError(t, src.Close())

	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	t.Cleanup(func() { dst.Close() })
	_, err = dst.CopyTrashedDataFrom(srcPath)
	require.NoError(t, err)
	insertSession(t, dst, "codex:"+legacyThread, "project", func(s *Session) { s.Agent = "codex" })

	var parent, relationship string
	require.NoError(t, dst.getReader().QueryRow(ctx,
		`SELECT COALESCE(parent_session_id, ''), relationship_type FROM sessions WHERE id = ?`,
		"codex:"+legacyThread+"_"+legacyRoll1).Scan(&parent, &relationship))
	assert.Equal(t, "codex:"+legacyThread, parent)
	assert.Equal(t, "continuation", relationship)
}

func TestLegacyCodexTrashedPageDropsStaleSubagentParentKeepsLauncher(t *testing.T) {
	// The old parser read a reverted subagent thread's page header as a
	// subagent of its launcher. The page continues its thread instead, but a
	// launcher override with no parser parent behind it stays.
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	launcher := "codex:44444444-4444-4444-8444-444444444444"
	ctx := t.Context()
	for _, rollout := range []string{legacyRoll1, legacyRoll2} {
		insertSession(t, src, "codex:"+rollout, "project", func(s *Session) {
			s.Agent = "codex"
			s.FilePath = legacyPageFile(rollout)
		})
		require.NoError(t, src.SoftDeleteSession(ctx, "codex:"+rollout))
	}
	for _, q := range []string{
		`UPDATE sessions SET parent_session_id = '` + launcher + `', parser_parent_session_id = '` + launcher +
			`', relationship_type = 'subagent' WHERE id = 'codex:` + legacyRoll1 + `'`,
		`UPDATE sessions SET parent_session_id = '` + launcher + `', parser_parent_session_id = NULL ` +
			`WHERE id = 'codex:` + legacyRoll2 + `'`,
		`PRAGMA user_version = 133`,
	} {
		_, err := src.getWriter().Exec(ctx, q)
		require.NoError(t, err)
	}
	require.NoError(t, src.Close())

	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	t.Cleanup(func() { dst.Close() })
	_, err := dst.CopyTrashedDataFrom(srcPath)
	require.NoError(t, err)

	read := func(rollout string) (parent, parserParent, relationship string) {
		require.NoError(t, dst.getReader().QueryRow(ctx,
			`SELECT COALESCE(parent_session_id, ''), COALESCE(parser_parent_session_id, ''), relationship_type
			 FROM sessions WHERE id = ?`, "codex:"+legacyThread+"_"+rollout).Scan(&parent, &parserParent, &relationship))
		return
	}
	parent, parserParent, relationship := read(legacyRoll1)
	assert.Equal(t, "codex:"+legacyThread, parent)
	assert.Equal(t, "codex:"+legacyThread, parserParent)
	assert.Equal(t, "continuation", relationship)
	parent, parserParent, _ = read(legacyRoll2)
	assert.Equal(t, launcher, parent, "launcher override stays")
	assert.Empty(t, parserParent)
}

func TestLegacyBareRolloutExclusionWithoutFileNameBlocksPage(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.getWriter().Exec(ctx,
		`INSERT INTO excluded_sessions (id, file_path) VALUES (?, NULL)`,
		"codex:"+legacyRoll1)
	require.NoError(t, err)

	page := Session{ID: "codex:" + legacyThread + "_" + legacyRoll1, Agent: "codex", Project: "sample", Machine: "local"}
	assert.True(t, d.IsSessionExcluded(ctx, page.ID))
	assert.False(t, d.IsSessionExcluded(ctx, "codex:"+legacyThread+"_"+legacyRoll2))
	assert.False(t, d.IsSessionExcluded(ctx, "codex:"+legacyThread))
	require.ErrorIs(t, d.UpsertSession(ctx, page), ErrSessionExcluded)
	got, err := d.GetSession(ctx, page.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "the returned page is not re-admitted")
}
