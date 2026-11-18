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
		{SessionID: s.ID, Ordinal: 1, Role: "user", IsSystem: true, Content: "https://github.com/o/r/pull/3"},
		{SessionID: s.ID, Ordinal: 2, Role: "user", SourceSubtype: "tool_result", Content: "https://github.com/o/r/pull/4"},
		{SessionID: s.ID, Ordinal: 3, Role: "user", IsCompactBoundary: true, Content: "https://github.com/o/r/pull/5"},
		{SessionID: s.ID, Ordinal: 4, Role: "user", Content: "<task-notification>https://github.com/o/r/pull/6</task-notification>"},
		{SessionID: s.ID, Ordinal: 5, Role: "user", Content: "<bash-stdout>https://github.com/o/r/pull/7</bash-stdout>"},
		{SessionID: s.ID, Ordinal: 6, Role: "user", Content: "review https://github.com/o/r/pull/8"},
		{SessionID: s.ID, Ordinal: 7, Role: "user", Content: "https://github.com/o/r/pull/11 <system-reminder>https://github.com/o/r/pull/10</system-reminder> <system-reminder source=\"workbuddy\">https://github.com/o/r/pull/12</system-reminder> https://github.com/o/r/pull/13"},
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, msgs))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 4)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/8", got.PRLinks[1].URL)
	assert.Equal(t, "https://github.com/o/r/pull/11", got.PRLinks[2].URL)
	assert.Equal(t, "https://github.com/o/r/pull/13", got.PRLinks[3].URL)
}

func TestPromptPRLinksArchiveUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if d != nil {
			_ = d.Close()
		}
	})
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s := Session{ID: "archived", Project: "project", Machine: "local", Agent: "chatgpt", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=structured_pr_links WHERE id=?`, s.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`)
		return err
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, link.URL, got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[1].URL)
}

func TestPromptPRLinksUsagePolicyWithRetainedHistory(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "history", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.UpsertSession(t.Context(), s))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestPromptPRLinksBatchAndStagedDialogueReplacement(t *testing.T) {
	for _, method := range []string{"single", "batch", "staged", "content"} {
		t.Run(method, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "dialogue", Project: "project", Machine: "local", Agent: "claude"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			first, second := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
			msg := Message{SessionID: s.ID, Role: "user", Content: first + " non-dialogue https://github.com/o/r/pull/9", DialogueText: &first}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
			msg.Content = second + " non-dialogue https://github.com/o/r/pull/9"
			msg.DialogueText = &second
			switch method {
			case "single":
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg}))
			case "batch":
				_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s, Messages: []Message{msg}, ReplaceMessages: true}})
				require.NoError(t, err)
			case "staged":
				require.NoError(t, d.WriteSessionBatchStaged(t.Context(), SessionBatchWrite{Session: s, Messages: []Message{msg}, ReplaceMessages: true}, newScratchStagedResults(t), nil, nil))
			default:
				require.NoError(t, d.ReplaceSessionContent(t.Context(), s.ID, []Message{msg}, SessionSignalUpdate{}, nil))
			}
			got, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/o/r/pull/2", got.PRLinks[0].URL)
		})
	}
}

func TestPromptPRLinksMetadataUpsertPreservesLinks(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "metadata", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/1"}}))
	require.NoError(t, d.UpsertSession(t.Context(), s))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/2", "o/r", 2)
	require.True(t, ok)
	s.PRLinks = []PRLink{link}
	obs := export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: "/tmp/worktree"}
	require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, ""))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, []PRLink{link, {URL: "https://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1}}, got.PRLinks)
}

func TestPromptPRLinksUsageOrphanCopy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			source := testDB(t)
			link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
			require.True(t, ok)
			s := Session{ID: "orphan", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
			require.NoError(t, source.UpsertSession(t.Context(), s))
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/2", HasOutputTokens: true, OutputTokens: 1}}))
			if legacy {
				require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=structured_pr_links`); err != nil {
						return err
					}
					if _, err := tx.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL); err != nil {
						return err
					}
					_, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`)
					return err
				}))
			}
			dest := testDB(t)
			dest.SetArchiveContent(config.ArchiveContentUsage)
			_, err := dest.CopyOrphanedDataFrom(t.Context(), source.Path())
			require.NoError(t, err)
			got, err := dest.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Equal(t, []PRLink{link}, got.PRLinks)
			structured, err := dest.GetSessionStructuredPRLinks(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Equal(t, []PRLink{link}, structured)
		})
	}
}

func TestPromptPRLinksStructuredChanges(t *testing.T) {
	d := testDB(t)
	first, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	second, ok := parser.NewPRLink("https://github.com/o/r/pull/2", "o/r", 2)
	require.True(t, ok)
	s := Session{ID: "structured", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{first, second}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/3"}}))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "Different prompt"}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, []PRLink{first, second}, got.PRLinks)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: first.URL}}))
	s.PRLinks = nil
	require.NoError(t, d.UpsertSession(t.Context(), s))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, []PRLink{first}, got.PRLinks)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, nil))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestPromptPRLinksTrashedCopyRestore(t *testing.T) {
	source := testDB(t)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	s := Session{ID: "trashed", Project: "project", Machine: "local", Agent: "claude", MessageCount: 1, PRLinks: []PRLink{link}}
	require.NoError(t, source.UpsertSession(t.Context(), s))
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "Review https://github.com/o/r/pull/2", HasOutputTokens: true, OutputTokens: 1}}))
	require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=structured_pr_links WHERE id=?`, s.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`)
		return err
	}))
	require.NoError(t, source.SoftDeleteSession(t.Context(), s.ID))
	dest := testDB(t)
	ids, err := dest.CopyTrashedDataFrom(t.Context(), source.Path())
	require.NoError(t, err)
	require.Equal(t, []string{s.ID}, ids)
	got, err := dest.GetSessionFull(t.Context(), s.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, link.URL, got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/2", got.PRLinks[1].URL)
}

func TestPromptPRLinksBackfillUsesDialogue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	cases := []struct {
		id, content string
		dialogue    *string
		want        string
	}{
		{"fallback", "HTTP://github.com/o/r/%70ull/1", nil, "http://github.com/o/r/pull/1"},
		{"same", "https://github.com/o/r/pull/2", new("https://github.com/o/r/pull/2"), "https://github.com/o/r/pull/2"},
		{"partial", "https://github.com/o/r/pull/3 https://github.com/o/r/pull/4", new("https://github.com/o/r/pull/3"), "https://github.com/o/r/pull/3"},
		{"empty", "https://github.com/o/r/pull/5", new(""), ""},
	}
	for _, tc := range cases {
		require.NoError(t, d.UpsertSession(t.Context(), Session{ID: tc.id, Project: "project", Machine: "local", Agent: "claude"}))
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), tc.id, []Message{{SessionID: tc.id, Role: "user", Content: tc.content, DialogueText: tc.dialogue}}))
	}
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''; DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`)
		return err
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	for _, tc := range cases {
		got, err := d.GetSession(t.Context(), tc.id)
		require.NoError(t, err)
		if tc.want == "" {
			assert.Empty(t, got.PRLinks, tc.id)
		} else {
			require.Len(t, got.PRLinks, 1, tc.id)
			assert.Equal(t, tc.want, got.PRLinks[0].URL, tc.id)
		}
	}
}
