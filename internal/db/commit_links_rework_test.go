package db

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func commitSnapshotCards(t *testing.T, d *DB, projects ...string) []CommitLink {
	t.Helper()
	cards := []CommitLink{}
	require.NoError(t, d.ReadCommitLinkSnapshot(t.Context(), projects, nil, func(s CommitLinkSnapshot) error {
		return s.Each(func(r CommitLinkExport) error {
			var link CommitLink
			if err := json.Unmarshal([]byte(r.Payload), &link); err != nil {
				return err
			}
			cards = append(cards, link)
			return nil
		})
	}))
	return cards
}

func bindCommitFixture(t *testing.T, d *DB, device, native, id string) {
	t.Helper()
	root := RawArchiveRoot{ID: device, DeviceID: device, Machine: "fixture", Provider: "codex", OriginalPath: "/fixture", ConfiguredRootID: device}
	require.NoError(t, d.RegisterRawArchiveRoot(t.Context(), root))
	_, err := d.BindRawArchiveSession(t.Context(), root, "source", native, id)
	require.NoError(t, err)
}

func TestCommitTrailerOwnerLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const id = "one~codex:native"
	link := CommitLink{SessionID: "codex:native", CommitSHA: "abcdef0", Source: "trailer"}
	require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
	bindCommitFixture(t, d, "one", link.SessionID, id)
	require.NoError(t, d.RefreshCommitLinks(ctx, id))
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	require.NoError(t, d.SoftDeleteSession(ctx, id))
	rows, err := d.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	assert.Empty(t, rows, "soft trash hides owned trailer evidence")
	assert.Empty(t, commitSnapshotCards(t, d))
	require.NoError(t, d.DeleteSession(ctx, id))
	rows, err = d.CommitLinks(ctx, nil, id)
	require.NoError(t, err)
	require.Len(t, rows, 1, "retained ownership supports exact canonical ID lookup")
	assert.Equal(t, id, rows[0].SessionID)
	assert.True(t, rows[0].Gone)
	assert.Nil(t, rows[0].Turn)
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	assert.Len(t, commitSnapshotCards(t, d, "demo"), 1, "rescan preserves retained project scope")
}

func TestCommitBindingCachesPreserveCardAmbiguity(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "fixture", Agent: "codex", Project: "demo"}))
	require.NoError(t, d.RefreshCommitLinks(ctx, "fixture"))
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{
		{SessionID: "fixture", CommitSHA: "abcdef0", Source: "trailer"},
		{SessionID: "fixture", CommitSHA: "1234567890123456789012345678901234567890", Resolved: true, Source: "trailer"},
	}))
	// Make scan order deterministic while retaining two different SHA states.
	_, err := d.getWriter().ExecContext(ctx, "UPDATE commit_session_links SET id=CASE WHEN commit_sha='abcdef0' THEN 'a' ELSE 'b' END")
	require.NoError(t, err)
	rows, err := d.CommitLinks(ctx, nil, "fixture")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, !row.Resolved, row.Ambiguous, row.CommitSHA)
	}
	for _, row := range commitSnapshotCards(t, d) {
		assert.Equal(t, !row.Resolved, row.Ambiguous, row.CommitSHA)
	}
}

func TestCommitSnapshotBindingCacheSeparatesSources(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"one~codex:native", "two~codex:native"} {
		require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
	}
	bindCommitFixture(t, d, "one", "codex:native", "one~codex:native")
	bindCommitFixture(t, d, "two", "codex:native", "two~codex:native")
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{
		{SessionID: "codex:native", CommitSHA: "abcdef0000000000000000000000000000000000", Resolved: true, Source: "transcript"},
		{SessionID: "codex:native", CommitSHA: "abcdef1000000000000000000000000000000000", Resolved: true, Source: "trailer"},
		{SessionID: "codex:native", CommitSHA: "abcdef2000000000000000000000000000000000", Resolved: true, Source: "trailer"},
	}))
	_, err := d.getWriter().ExecContext(ctx, "UPDATE commit_session_links SET id=CASE substr(commit_sha,1,7) WHEN 'abcdef0' THEN 'a' WHEN 'abcdef1' THEN 'b' ELSE 'c' END")
	require.NoError(t, err)
	cards := commitSnapshotCards(t, d)
	require.Len(t, cards, 3)
	// Resolve hashes to isolate native ownership ambiguity from SHA ambiguity.
	for _, row := range cards {
		if row.Source == "trailer" {
			assert.True(t, row.Ambiguous)
		}
	}
}

func TestCommitBindingChangeQueuesStoredTurnDerivation(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const id = "one~codex:native"
	require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, id, []Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "Write the parser", Timestamp: "2026-01-01T00:00:00Z"},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "editing", Timestamp: "2026-01-01T00:01:00Z", ToolCalls: []ToolCall{{Category: "Edit", InputJSON: `{"path":"src/parser.go"}`}}},
		{SessionID: id, Ordinal: 2, Role: "user", Content: "Then docs", Timestamp: "2026-01-01T00:02:00Z"},
	}))
	require.NoError(t, d.StoreCommitTrailerLinks(ctx, []CommitLink{{SessionID: "codex:native", CommitSHA: "abcdef0", Source: "trailer", CommittedAt: "2026-01-01T00:03:00Z"}}, []string{"src/parser.go"}))
	_, err := d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	bindCommitFixture(t, d, "one", "codex:native", id)
	n, err := d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	rows, err := d.CommitLinks(ctx, nil, id)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, "wrote", rows[0].Turn.Why)
	assert.Equal(t, "Write the parser", rows[0].Turn.Prompt)
}

func TestCommitTrailerFactsForTrashedOwnerInvalidateTurnState(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const id = "one~codex:native"
	require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, id, []Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "Write the parser", Timestamp: "2026-01-01T00:00:00Z"},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "editing", Timestamp: "2026-01-01T00:01:00Z", ToolCalls: []ToolCall{{Category: "Edit", InputJSON: `{"path":"src/parser.go"}`}}},
		{SessionID: id, Ordinal: 2, Role: "user", Content: "Then docs", Timestamp: "2026-01-01T00:02:00Z"},
	}))
	bindCommitFixture(t, d, "one", "codex:native", id)
	require.NoError(t, d.RefreshCommitLinks(ctx, id))
	require.NoError(t, d.SoftDeleteSession(ctx, id))
	require.NoError(t, d.StoreCommitTrailerFacts(ctx, []CommitLink{{
		SessionID: "codex:native", CommitSHA: "abcdef0", Source: "trailer",
		CommittedAt: "2026-01-01T00:03:00Z",
	}}, []string{"src/parser.go"}, true))
	_, err := d.RestoreSession(ctx, id)
	require.NoError(t, err)
	n, err := d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "facts discovered in trash invalidate the prior turn derivation")

	rows, err := d.CommitLinks(ctx, nil, id)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, "Write the parser", rows[0].Turn.Prompt)
}

func TestCommitTrailerRescanPreservesDeletedProjectHint(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	link := CommitLink{SessionID: "fixture", CommitSHA: "abcdef0", Source: "trailer"}
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "fixture", Agent: "claude", Project: "demo"}))
	require.NoError(t, d.RefreshCommitLinks(ctx, "fixture"))
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	require.NoError(t, d.DeleteSession(ctx, "fixture"))
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	cards := commitSnapshotCards(t, d, "demo")
	require.Len(t, cards, 1)
	assert.True(t, cards[0].Gone)
	assert.Empty(t, cards[0].Project)
}

func TestCommitTrailerNativeOwnerDeletionPreservesUnhydratedProjectHint(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const id = "one~codex:native"
	link := CommitLink{SessionID: "codex:native", CommitSHA: "abcdef0", Source: "trailer"}
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
	bindCommitFixture(t, d, "one", link.SessionID, id)
	// Simulate an existing archive with the pre-fix deletion trigger, then run
	// the writable-open trigger migration before deleting the owner.
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TRIGGER trg_commit_links_session_delete;
CREATE TRIGGER trg_commit_links_session_delete AFTER DELETE ON sessions BEGIN
    UPDATE commit_session_links SET project_hint=OLD.project,payload=json_set(payload,
        '$.session',json('null'),'$.gone',json('true'),'$.turn',json('null'),
        '$.agent','','$.project','','$.session_started_at','','$.web_url','')
    WHERE session_id=OLD.id OR json_extract(payload,'$.session.id')=OLD.id;
    DELETE FROM commit_link_state WHERE session_id=OLD.id;
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;`)
		return err
	}))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, commitLinksMigratedTriggerSQL)
		return err
	}))
	require.NoError(t, d.DeleteSession(ctx, id))

	cards := commitSnapshotCards(t, d, "demo")
	require.Len(t, cards, 1, "the unique native owner leaves a project-scoped gone card")
	assert.True(t, cards[0].Gone)
	assert.Empty(t, cards[0].Project)
}

func TestCommitTrailerUnavailableOwnerStillMakesIdentityAmbiguous(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "trashed", true: "deleted"}[missing], func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			for _, device := range []string{"one", "two"} {
				id := device + "~codex:native"
				require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
				bindCommitFixture(t, d, device, "codex:native", id)
			}
			if missing {
				require.NoError(t, d.DeleteSession(ctx, "one~codex:native"))
			} else {
				require.NoError(t, d.SoftDeleteSession(ctx, "one~codex:native"))
			}
			_, err := d.BackfillCommitLinks(ctx, 100)
			require.NoError(t, err)
			require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{{SessionID: "codex:native", CommitSHA: "1234567890123456789012345678901234567890", Resolved: true, Source: "trailer"}}))
			rows, err := d.CommitLinks(ctx, []string{"1234567"}, "")
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "codex:native", rows[0].SessionID)
			assert.True(t, rows[0].Gone)
			assert.True(t, rows[0].Ambiguous)
		})
	}
}

func TestCommitFreshArchiveTracksSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	d, err := OpenFreshIsolatedContext(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "fixture", Agent: "claude", Project: "demo"}))
	before, err := d.CommitLinkRevision(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "fixture", Agent: "claude", Project: "renamed"}))
	after, err := d.CommitLinkRevision(t.Context())
	require.NoError(t, err)
	assert.Greater(t, after, before)
}
