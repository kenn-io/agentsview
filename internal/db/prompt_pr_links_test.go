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
		{SessionID: s.ID, Ordinal: 7, Role: "user", Content: "ordinary <system-reminder>https://github.com/o/r/pull/10</system-reminder>"},
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, msgs))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/o/r/pull/8", got.PRLinks[1].URL)
}

func TestPromptPRLinksArchiveUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		legacy, rebuild, completed bool
		version                    int
	}{{name: "backfill", version: 183}, {name: "reserved version", version: 184}, {name: "completed backfill", version: 184, completed: true}, {name: "rebuild", rebuild: true}, {name: "legacy", legacy: true, version: 183}} {
		t.Run(tc.name, func(t *testing.T) {
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
			s := Session{ID: "archived", Project: "project", Machine: "local", Agent: "chatgpt"}
			if tc.legacy {
				s.PRLinks = []PRLink{link}
			}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			dialogue := "https://github.com/o/r/%70ull/42"
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: dialogue + " https://github.com/o/r/pull/3", DialogueText: &dialogue}}))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if !tc.completed {
					if _, err := tx.Exec(`UPDATE sessions SET pr_links=structured_pr_links WHERE id=?`, s.ID); err != nil {
						return err
					}
				}
				if tc.legacy {
					if _, err := tx.Exec(artifactSessionQueueTriggerDropsSQL); err != nil {
						return err
					}
					if _, err := tx.Exec(`ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
						return err
					}
				}
				if !tc.completed {
					if _, err := tx.Exec(`DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`); err != nil {
						return err
					}
				}
				version := tc.version
				if tc.rebuild {
					version = fullRebuildDataVersion() - 1
				}
				return writeUserVersion(t.Context(), tx, version)
			}))
			require.NoError(t, d.Close())
			d, err = OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			if tc.rebuild {
				assert.True(t, d.NeedsResync())
				version, _, err := readUserVersion(t.Context(), d.getReader())
				require.NoError(t, err)
				assert.Less(t, version, fullRebuildDataVersion())
				got, err := d.GetSession(t.Context(), s.ID)
				require.NoError(t, err)
				assert.Empty(t, got.PRLinks)
				require.NoError(t, d.Close())
				d, err = OpenIsolated(t.Context(), filepath.Join(t.TempDir(), "rebuilt.db"))
				require.NoError(t, err)
				_, err = d.CopyOrphanedDataFrom(t.Context(), path)
				require.NoError(t, err)
			} else {
				version, _, err := readUserVersion(t.Context(), d.getReader())
				require.NoError(t, err)
				assert.Equal(t, 185, version)
				assert.False(t, d.NeedsResync())
			}
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			if tc.legacy {
				require.Len(t, got.PRLinks, 2)
				assert.Equal(t, link.URL, got.PRLinks[0].URL)
				assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[1].URL)
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/4"}}))
				got, err = d.GetSession(t.Context(), s.ID)
				require.NoError(t, err)
				require.Len(t, got.PRLinks, 2)
				assert.Equal(t, link.URL, got.PRLinks[0].URL)
				assert.Equal(t, "https://github.com/o/r/pull/4", got.PRLinks[1].URL)
			} else {
				require.Len(t, got.PRLinks, 1)
				assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[0].URL)
			}
			require.NoError(t, d.Close())
			if !tc.legacy && !tc.rebuild {
				d, err = OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				got, err = d.GetSession(t.Context(), s.ID)
				require.NoError(t, err)
				require.Len(t, got.PRLinks, 1)
				assert.Equal(t, "https://github.com/o/r/pull/42", got.PRLinks[0].URL)
				require.NoError(t, d.Close())
			}
		})
	}
}

func TestPromptPRLinksUsagePolicyWithRetainedHistory(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "history", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/42"}}))
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", Content: "reply", HasOutputTokens: true, OutputTokens: 1}}))
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
			first, second := "https://github.com/o/r/%70ull/1", "https://github.com/o/r/%70ull/2"
			msg := Message{SessionID: s.ID, Role: "user", Content: "same", DialogueText: &first}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
			msg.DialogueText = &second
			if method == "single" {
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg}))
			} else if method == "batch" {
				_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s, Messages: []Message{msg}, ReplaceMessages: true}})
				require.NoError(t, err)
			} else if method == "staged" {
				require.NoError(t, d.WriteSessionBatchStaged(t.Context(), SessionBatchWrite{Session: s, Messages: []Message{msg}, ReplaceMessages: true}, newScratchStagedResults(t), nil, nil))
			} else {
				require.NoError(t, d.ReplaceSessionContent(t.Context(), s.ID, []Message{msg}, SessionSignalUpdate{}, nil))
			}
			got, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/o/r/pull/2", got.PRLinks[0].URL)
			if method == "single" {
				require.Error(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg, msg}))
				got, err = d.GetSession(t.Context(), s.ID)
				require.NoError(t, err)
				require.Len(t, got.PRLinks, 1)
				assert.Equal(t, "https://github.com/o/r/pull/2", got.PRLinks[0].URL)
			}
		})
	}
}

func TestPromptPRLinksMetadataUpsertPreservesLinks(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "metadata", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/1"}}))
	var modified, marker string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT local_modified_at, sync_marker FROM sessions WHERE id=?`, s.ID).Scan(&modified, &marker))
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: "/tmp/worktree"}
	require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, ""))
	var afterModified, afterMarker string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT local_modified_at, sync_marker FROM sessions WHERE id=?`, s.ID).Scan(&afterModified, &afterMarker))
	assert.Equal(t, modified, afterModified)
	assert.Equal(t, marker, afterMarker)
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
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
					if _, err := tx.Exec(`UPDATE sessions SET pr_links=structured_pr_links`); err != nil {
						return err
					}
					if _, err := tx.Exec(artifactSessionQueueTriggerDropsSQL); err != nil {
						return err
					}
					_, err := tx.Exec(`ALTER TABLE sessions DROP COLUMN structured_pr_links`)
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
			msgs, err := dest.GetAllMessages(t.Context(), s.ID)
			require.NoError(t, err)
			for _, msg := range msgs {
				assert.Empty(t, msg.Content)
			}
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
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: first.URL}}))
	s.PRLinks = nil
	require.NoError(t, d.UpsertSession(t.Context(), s))
	got, err := d.GetSession(t.Context(), s.ID)
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
		_, err := tx.Exec(`UPDATE sessions SET pr_links=structured_pr_links WHERE id=?`, s.ID)
		return err
	}))
	require.NoError(t, source.SoftDeleteSession(t.Context(), s.ID))
	dest := testDB(t)
	ids, err := dest.CopyTrashedDataFrom(t.Context(), source.Path())
	require.NoError(t, err)
	require.Equal(t, []string{s.ID}, ids)
	_, err = dest.RestoreSession(t.Context(), s.ID)
	require.NoError(t, err)
	filter, err := ParsePRFilter("o/r#2")
	require.NoError(t, err)
	page, err := dest.ListSessions(t.Context(), SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, s.ID, page.Sessions[0].ID)
}
