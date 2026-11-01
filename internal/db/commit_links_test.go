package db

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestCommitLinksExtractAndRetainGone(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "codex:fixture", Agent: "codex", Project: "demo", Cwd: t.TempDir()}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, "codex:fixture", []Message{
		{SessionID: "codex:fixture", Ordinal: 0, Role: "user", Content: "Fix the parser", Timestamp: "2026-01-01T12:00:00Z"},
		{SessionID: "codex:fixture", Ordinal: 1, Role: "assistant", Content: "working", Timestamp: "2026-01-01T12:01:00Z", ToolCalls: []ToolCall{{CallIndex: 0, ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"git commit -am fix"}`, ResultContent: "[main abcdef1] Fix parser"}}},
	}))
	require.NoError(t, d.RefreshCommitLinks(ctx, "codex:fixture"))
	rows, err := d.CommitLinks(ctx, []string{"abcdef1"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "abcdef1", rows[0].CommitSHA)
	assert.True(t, rows[0].Ambiguous)
	require.NotNil(t, rows[0].MessageOrdinal)
	assert.Equal(t, 1, *rows[0].MessageOrdinal)
	require.NotNil(t, rows[0].CallIndex)
	assert.Zero(t, *rows[0].CallIndex)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, CommitTurn{Ordinal: 0, At: "2026-01-01T12:00:00Z", Prompt: "Fix the parser", Why: "commit-call"}, *rows[0].Turn)
	require.NoError(t, d.RefreshCommitLinks(ctx, "codex:fixture"))
	again, err := d.CommitLinks(ctx, nil, "codex:fixture")
	require.NoError(t, err)
	assert.Len(t, again, 1)
	require.NoError(t, d.DeleteSession(ctx, "codex:fixture"))
	gone, err := d.CommitLinks(ctx, nil, "codex:fixture")
	require.NoError(t, err)
	require.Len(t, gone, 1)
	assert.True(t, gone[0].Gone)
	assert.Nil(t, gone[0].Session)
	assert.Nil(t, gone[0].Turn)
	assert.Empty(t, gone[0].Project)
}

func TestCommitLinksKeepPrefixRelatedSummariesDistinct(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "codex:fixture", Agent: "codex", Project: "demo", Cwd: t.TempDir()}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, "codex:fixture", []Message{
		{SessionID: "codex:fixture", Ordinal: 0, Role: "user", Content: "Fix both commits", Timestamp: "2026-01-01T12:00:00Z"},
		{SessionID: "codex:fixture", Ordinal: 1, Role: "assistant", Content: "working", ToolCalls: []ToolCall{{CallIndex: 0, ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"git commit -am fix"}`, ResultContent: "[main abcdef0] First\n[main abcdef01] Second"}}},
	}))
	require.NoError(t, d.RefreshCommitLinks(ctx, "codex:fixture"))

	rows, err := d.CommitLinks(ctx, []string{"abcdef0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 2, "each successful Git summary is distinct evidence even when one printed SHA prefixes another")
	assert.Equal(t, "abcdef0", rows[0].CommitSHA)
	assert.Equal(t, "abcdef01", rows[1].CommitSHA)
	for _, row := range rows {
		assert.True(t, row.Ambiguous, "both summaries share the requested prefix")
	}
	var count int
	require.NoError(t, d.getWriter().QueryRowContext(ctx, "SELECT COUNT(DISTINCT id) FROM commit_session_links WHERE source='transcript'").Scan(&count))
	assert.Equal(t, 2, count, "prefix-related summaries must not upsert over one another")
}

func TestCommitLinksReplaceChangedTranscript(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "s", Agent: "claude", Project: "demo"}))
	messages := []Message{{SessionID: "s", Ordinal: 0, Role: "user", Content: "original"}, {SessionID: "s", Ordinal: 1, Role: "assistant", Content: "commit", ToolCalls: []ToolCall{{Category: "Bash", InputJSON: `{"command":"git commit -m original"}`, ResultContent: "[main abcdef1] Original"}}}}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s", messages))
	require.NoError(t, d.RefreshCommitLinks(ctx, "s"))
	messages[0].Content = "different prompt"
	messages[1].ToolCalls[0].ResultContent = "nothing to commit"
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s", messages))
	stale, err := d.CommitLinks(ctx, nil, "s")
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Nil(t, stale[0].Turn)
	require.NoError(t, d.RefreshCommitLinks(ctx, "s"))
	rows, err := d.CommitLinks(ctx, nil, "s")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCommitLinkPrefixCompatibility(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, link := range []CommitLink{
		{SessionID: "unknown-one", CommitSHA: "abcdef0123456789012345678901234567890123", Resolved: true, Source: "trailer"},
		{SessionID: "unknown-two", CommitSHA: "abcdef0999999999999999999999999999999999", Resolved: true, Source: "trailer"},
		{SessionID: "unknown-short", CommitSHA: "abcdef0", Source: "transcript"},
	} {
		require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{link}))
	}
	rows, err := d.CommitLinks(ctx, []string{"ABCDEF0"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, row := range rows {
		assert.True(t, row.Ambiguous)
		assert.True(t, row.Gone)
		assert.Nil(t, row.Turn)
	}
	rows, err = d.CommitLinks(ctx, []string{"abcdef0123"}, "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	_, err = d.CommitLinks(ctx, []string{"abc%'"}, "")
	assert.ErrorIs(t, err, ErrInvalidCommitQuery)
}

func TestCommitLinksRejectOversizedResults(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	links := make([]CommitLink, MaxCommitLinks+1)
	for i := range links {
		links[i] = CommitLink{SessionID: "unknown-" + strconv.Itoa(i), CommitSHA: "abcdef0", Source: "trailer"}
	}
	require.NoError(t, d.StoreCommitLinks(ctx, links))
	_, err := d.CommitLinks(ctx, []string{"abcdef0"}, "")
	assert.ErrorIs(t, err, ErrCommitLookupTooLarge)
}

func TestCommitLinkSourceRevisionAdvancesPublicationAfterMigration(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer"}}))
	var payload string
	require.NoError(t, d.getWriter().QueryRowContext(ctx, "SELECT payload FROM commit_session_links").Scan(&payload))
	before, err := d.CommitLinkRevision(ctx)
	require.NoError(t, err)

	// Reinstall the old definition to exercise the migration replacement path.
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TRIGGER trg_commit_links_update;
CREATE TRIGGER trg_commit_links_update AFTER UPDATE ON commit_session_links
WHEN OLD.payload IS NOT NEW.payload OR OLD.project_hint IS NOT NEW.project_hint BEGIN
    UPDATE commit_link_publication_state SET revision=revision+1 WHERE singleton=1;
END;`)
		return err
	}))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, commitLinksMigratedTriggerSQL)
		return err
	}))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE commit_session_links SET source_revision=source_revision||'-updated'")
		return err
	}))

	after, err := d.CommitLinkRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before, "source-revision-only reindexing must invalidate replica publication")
	var unchanged string
	require.NoError(t, d.getWriter().QueryRowContext(ctx, "SELECT payload FROM commit_session_links").Scan(&unchanged))
	assert.Equal(t, payload, unchanged, "the regression changes only source_revision")
}

func TestCommitTrailerUsesBoundedRevisionAndSourcePriority(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "trailer-fixture", Agent: "claude", Project: "demo"}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, "trailer-fixture", []Message{
		{SessionID: "trailer-fixture", Ordinal: 0, Role: "user", Content: "Change the parser", Timestamp: "2026-01-01T12:00:00Z"},
		{SessionID: "trailer-fixture", Ordinal: 1, Role: "assistant", Content: "working", Timestamp: "2026-01-01T12:01:00Z", ToolCalls: []ToolCall{{CallIndex: 0, Category: "Edit", InputJSON: `{"path":"internal/parser/file.go"}`}}},
	}))
	require.NoError(t, d.RefreshCommitLinks(ctx, "trailer-fixture"))
	link := CommitLink{SessionID: "trailer-fixture", CommitSHA: "abcdef0123456789012345678901234567890123", Resolved: true, Source: "trailer", CommittedAt: "2026-01-01T12:02:00Z"}
	require.NoError(t, d.StoreCommitTrailerLinks(ctx, []CommitLink{link}, []string{"internal/parser/file.go"}))
	rows, err := d.CommitLinks(ctx, nil, "trailer-fixture")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, "wrote", rows[0].Turn.Why)
	assert.Equal(t, "Change the parser", rows[0].Turn.Prompt)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "trailer-fixture", []Message{{SessionID: "trailer-fixture", Ordinal: 0, Role: "user", Content: "replacement prompt"}}))
	rows, err = d.CommitLinks(ctx, nil, "trailer-fixture")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Turn)
	require.NoError(t, d.DeleteSession(ctx, "trailer-fixture"))
	rows, err = d.CommitLinks(ctx, nil, "trailer-fixture")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Turn)
	assert.True(t, rows[0].Gone)
}

func TestCommitOrderUsesInstantsAndTranscriptPriority(t *testing.T) {
	rows := []CommitLink{{CommitSHA: "a", Source: "trailer", CommittedAt: "2026-01-01T13:00:00+01:00"}, {CommitSHA: "a", Source: "transcript", CommittedAt: "2026-01-01T12:00:00Z"}, {CommitSHA: "b", CommittedAt: "2026-01-01T12:00:00.001Z"}}
	SortCommitLinks(rows)
	assert.Equal(t, "transcript", rows[0].Source)
	assert.Equal(t, "trailer", rows[1].Source)
	assert.Equal(t, "b", rows[2].CommitSHA)
}

func TestCommitOrderIsPermutationIndependent(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		order := hegel.Draw(ht, hegel.SampledFrom([][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}))
		seconds := hegel.Draw(ht, hegel.Integers[int64](-1000000, 1000000))
		offset := hegel.Draw(ht, hegel.Integers[int](-12, 14))
		invalid := hegel.Draw(ht, hegel.Text()) + "!"
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
		fixture := []CommitLink{{CommitSHA: "a", CommittedAt: at.In(time.FixedZone("generated", offset*3600)).Format(time.RFC3339Nano)}, {CommitSHA: "b", CommittedAt: at.Add(time.Hour).Format(time.RFC3339Nano)}, {CommitSHA: "c", CommittedAt: invalid}}
		rows := []CommitLink{fixture[order[0]], fixture[order[1]], fixture[order[2]]}
		SortCommitLinks(rows)
		got := []string{rows[0].CommitSHA, rows[1].CommitSHA, rows[2].CommitSHA}
		assert.Equal(ht, []string{"c", "a", "b"}, got)
	}, hegel.WithTestCases(2000))
}

func TestCommitPrefixDirections(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sha := hegel.Draw(ht, hegel.Text().Alphabet("0123456789abcdef").MinSize(40).MaxSize(40))
		query := sha[:7]
		assert.True(ht, CommitSHAMatches(CommitLink{CommitSHA: sha, Resolved: true}, strings.ToUpper(query)))
		assert.True(ht, CommitSHAMatches(CommitLink{CommitSHA: query}, sha))
		assert.False(ht, CommitSHAMatches(CommitLink{CommitSHA: query, Resolved: true}, sha))
	}, hegel.WithTestCases(2000))
}

func TestCommitTrailerNamespaceBindingRequiresUniqueOwner(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	native := "codex:native"
	sha := "abcdef0"
	require.NoError(t, d.StoreCommitLinks(ctx, []CommitLink{{SessionID: native, CommitSHA: sha, Source: "trailer"}}))
	add := func(device, id string) {
		t.Helper()
		root := RawArchiveRoot{ID: device, DeviceID: device, Machine: "fixture", Provider: "codex", OriginalPath: "/fixture", ConfiguredRootID: device}
		require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
		require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "demo"}))
		_, err := d.BindRawArchiveSession(ctx, root, "source", native, id)
		require.NoError(t, err)
	}
	add("one", "one~codex:native")
	rows, err := d.CommitLinks(ctx, nil, "one~codex:native")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "one~codex:native", rows[0].SessionID)
	assert.False(t, rows[0].Gone)
	require.NoError(t, d.StoreCommitTrailerLinks(ctx, []CommitLink{{SessionID: native, CommitSHA: sha, Source: "trailer"}}, nil))
	add("two", "two~codex:native")
	rows, err = d.CommitLinks(ctx, []string{sha}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, native, rows[0].SessionID)
	assert.True(t, rows[0].Gone)
	assert.True(t, rows[0].Ambiguous)
	rows, err = d.CommitLinks(ctx, nil, "one~codex:native")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCommitTrailerRetainsPathsAcrossRevision(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	id := "paths-fixture"
	require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Agent: "claude", Project: "a"}))
	msgs := []Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "Write parser", Timestamp: "2026-01-01T00:00:00Z"}, {SessionID: id, Ordinal: 1, Role: "assistant", Content: "editing", Timestamp: "2026-01-01T00:01:00Z", ToolCalls: []ToolCall{{InputJSON: `{"path":"src/parser.go"}`, Category: "Edit"}}}, {SessionID: id, Ordinal: 2, Role: "user", Content: "Then documentation", Timestamp: "2026-01-01T00:02:00Z"}}
	require.NoError(t, d.ReplaceSessionMessages(ctx, id, msgs))
	require.NoError(t, d.RefreshCommitLinks(ctx, id))
	require.NoError(t, d.StoreCommitTrailerLinks(ctx, []CommitLink{{SessionID: id, CommitSHA: "abcdef0", Source: "trailer", CommittedAt: "2026-01-01T00:03:00Z"}}, []string{"src/parser.go"}))
	msgs = append(msgs, Message{SessionID: id, Ordinal: 3, Role: "assistant", Content: "finished", Timestamp: "2026-01-01T00:04:00Z"})
	require.NoError(t, d.ReplaceSessionMessages(ctx, id, msgs))
	require.NoError(t, d.RefreshCommitLinks(ctx, id))
	rows, err := d.CommitLinks(ctx, nil, id)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, "Write parser", rows[0].Turn.Prompt)
	assert.Equal(t, "wrote", rows[0].Turn.Why)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE sessions SET project='b' WHERE id=?", id)
		return err
	}))
	require.NoError(t, d.DeleteSession(ctx, id))
	var hint string
	require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT project_hint FROM commit_session_links WHERE session_id=?", id).Scan(&hint))
	assert.Equal(t, "b", hint)
}
