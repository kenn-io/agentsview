package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

func TestPromptPRLinksStoredMessages(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "session", Project: "project", Machine: "local", Agent: "claude"}
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s.PRLinks = []PRLink{link}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	msgs := []Message{
		{SessionID: s.ID, Ordinal: 0, Role: "assistant", Content: "https://github.com/o/r/pull/2"},
		{SessionID: s.ID, Ordinal: 1, Role: "user", IsSystem: true, Content: "https://github.com/o/r/pull/3"},
		{SessionID: s.ID, Ordinal: 2, Role: "user", SourceSubtype: "tool_result", Content: "https://github.com/o/r/pull/4"},
		{SessionID: s.ID, Ordinal: 3, Role: "user", IsCompactBoundary: true, Content: "https://github.com/o/r/pull/5"},
		{SessionID: s.ID, Ordinal: 4, Role: "user", Content: "<task-notification>https://github.com/o/r/pull/6</task-notification>"},
		{SessionID: s.ID, Ordinal: 5, Role: "user", Content: "<bash-stdout>https://github.com/o/r/pull/7</bash-stdout>"},
		{SessionID: s.ID, Ordinal: 6, Role: "user", Content: "<system-reminder>https://github.com/o/r/pull/9</system-reminder> review https://github.com/o/r/pull/8"},
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, msgs))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/8", got.PRLinks[1].URL)
	obs := export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: "/tmp/worktree"}
	require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, ""))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/o/r/pull/8", got.PRLinks[1].URL)
}

func TestPromptPRLinksUsagePolicy(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s := Session{ID: "usage", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/2"}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, link.URL, got.PRLinks[0].URL)
}

func TestPromptPRLinksBackfillAndVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	s := Session{ID: "imported", Project: "project", Machine: "local", Agent: "chatgpt"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sessions SET pr_links='' WHERE id=?`, s.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, 183)
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[0].URL)
	version, _, err := readUserVersion(t.Context(), d.getReader())
	require.NoError(t, err)
	assert.Equal(t, 184, version)
	providers := ScopedDataVersionProviders(183, 184)
	assert.NotNil(t, providers)
	assert.Empty(t, providers)
	assert.False(t, d.NeedsResync())
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
}

func TestPromptPRLinksOldArchiveRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	s := Session{ID: "orphan", Project: "project", Machine: "local", Agent: "chatgpt"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sessions SET pr_links='' WHERE id=?`, s.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, fullRebuildDataVersion()-1)
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, d.NeedsResync())
	version, _, err := readUserVersion(t.Context(), d.getReader())
	require.NoError(t, err)
	assert.Less(t, version, fullRebuildDataVersion())
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
	require.NoError(t, d.Close())
	rebuilt, err := OpenIsolated(t.Context(), filepath.Join(t.TempDir(), "rebuilt.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rebuilt.Close()) })
	_, err = rebuilt.CopyOrphanedDataFrom(t.Context(), path)
	require.NoError(t, err)
	got, err = rebuilt.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[0].URL)
	version, _, err = readUserVersion(t.Context(), rebuilt.getReader())
	require.NoError(t, err)
	assert.Equal(t, 184, version)
}

func TestPromptPRLinksUsagePolicyWithRetainedHistory(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "history", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET pr_links='' WHERE id=?`, s.ID)
		return err
	}))
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", Content: "reply", HasOutputTokens: true, OutputTokens: 1}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestPromptPRLinksReplaceKeepsStructured(t *testing.T) {
	d := testDB(t)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s := Session{ID: "replace", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/2"}}))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/3"}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/3", got.PRLinks[1].URL)
}

func TestPromptPRLinksUsesDialogue(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "dialogue", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	dialogue := "Review https://github.com/o/r/pull/1"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: dialogue + " tool: https://github.com/o/r/pull/2", DialogueText: &dialogue}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
}

func TestPromptPRLinksMigratesStructuredLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s := Session{ID: "legacy", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	dialogue := "https://github.com/o/r/pull/2"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: dialogue + " https://github.com/o/r/pull/3", DialogueText: &dialogue}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sessions SET pr_links=structured_pr_links WHERE id=?`, s.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(artifactSessionQueueTriggerDropsSQL); err != nil {
			return err
		}
		if _, err := tx.Exec(`ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, 183)
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/o/r/pull/2", got.PRLinks[1].URL)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/4"}}))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, link.URL, got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/4", got.PRLinks[1].URL)
}
