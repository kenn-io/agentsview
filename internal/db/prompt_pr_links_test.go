package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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
	t.Run("missing column", func(t *testing.T) {
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
		require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
				return err
			}
			return writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1)
		}))
		require.NoError(t, d.Close())
		d, err = OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		structured, err := d.GetSessionStructuredPRLinks(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Equal(t, []PRLink{link}, structured)
	})
	// An interrupted open can leave the archive at the old version after adding the column.
	t.Run("interrupted upgrade", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "archive.db")
		d, err := OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		t.Cleanup(func() {
			if d != nil {
				_ = d.Close()
			}
		})
		first, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
		require.True(t, ok)
		second, ok := parser.NewPRLink("https://github.com/o/r/pull/2", "o/r", 2)
		require.True(t, ok)
		prompt, ok := parser.NewPRLink("https://github.com/o/r/pull/42", "o/r", 42)
		require.True(t, ok)
		s := Session{ID: "interrupted", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{first}}
		require.NoError(t, d.UpsertSession(t.Context(), s))
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "review " + prompt.URL}}))
		require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
			if err := writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1); err != nil {
				return err
			}
			// A version 183 binary writes pr_links and knows no structured column.
			_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=? WHERE id=?`, EncodePRLinks([]PRLink{first, second}), s.ID)
			return err
		}))
		require.NoError(t, d.Close())

		d, err = OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		got, err := d.GetSession(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Equal(t, []PRLink{first, second, prompt}, got.PRLinks)
		structured, err := d.GetSessionStructuredPRLinks(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Equal(t, []PRLink{first, second}, structured)
	})
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
			msg := Message{SessionID: s.ID, Role: "user", Content: first + " non-dialogue " + second, DialogueText: &first}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{msg}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/o/r/pull/1", got.PRLinks[0].URL)
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
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/2", "o/r", 2)
	require.True(t, ok)
	s := Session{ID: "metadata", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "user", Content: "https://github.com/o/r/pull/1"}}))
	obs := export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: "/tmp/worktree"}
	require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, ""))
	var modified string
	var marker sql.NullString
	require.NoError(t, d.getWriter().QueryRowContext(t.Context(), `SELECT local_modified_at, sync_marker FROM sessions WHERE id=?`, s.ID).Scan(&modified, &marker))
	for _, upsert := range []func() error{
		func() error { return d.UpsertSession(t.Context(), s) },
		func() error { return d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, "") },
	} {
		require.NoError(t, upsert())
		got, err := d.GetSession(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Equal(t, []PRLink{link, {URL: "https://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1}}, got.PRLinks)
		var afterModified string
		var afterMarker sql.NullString
		require.NoError(t, d.getWriter().QueryRowContext(t.Context(), `SELECT local_modified_at, sync_marker FROM sessions WHERE id=?`, s.ID).Scan(&afterModified, &afterMarker))
		assert.Equal(t, modified, afterModified)
		assert.Equal(t, marker, afterMarker)
	}
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
					if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
						return err
					}
					return writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1)
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
		if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1)
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
		{"partial", "https://github.com/o/r/pull/3 https://github.com/o/r/pull/4", new("https://github.com/o/r/pull/3"), "https://github.com/o/r/pull/3"},
		{"empty", "https://github.com/o/r/pull/5", new(""), ""},
	}
	for _, tc := range cases {
		require.NoError(t, d.UpsertSession(t.Context(), Session{ID: tc.id, Project: "project", Machine: "local", Agent: "claude"}))
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), tc.id, []Message{{SessionID: tc.id, Role: "user", Content: tc.content, DialogueText: tc.dialogue}}))
	}
	seedArtifactOrigin(t, d)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1)
	}))
	clearArtifactExportQueue(t, d)
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	pending, err := d.CountPendingArtifactExports(t.Context())
	require.NoError(t, err)
	assert.Zero(t, pending)
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
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/9", "o/r", 9)
	require.True(t, ok)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "fallback", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}))
	pending, err = d.CountPendingArtifactExports(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

func TestToolPRLinksArchiveUpgrade(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.db")
			d, err := OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			s := Session{ID: "tool-archive", Project: "project", Machine: "local", Agent: "claude"}
			structured, ok := parser.NewPRLink("https://github.com/example/repo/pull/1", "example/repo", 1)
			require.True(t, ok)
			s.PRLinks = []PRLink{structured}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=? WHERE id=?`, EncodePRLinks(s.PRLinks), s.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS artifact_sessions_update_queue; ALTER TABLE sessions DROP COLUMN tool_pr_links`); err != nil {
					return err
				}
				return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
			}))
			require.NoError(t, d.Close())
			d, err = OpenIsolatedWithArchiveContent(t.Context(), path, policy)
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			exported, err := d.GetArtifactExportSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, exported.ToolPRLinks, 1)
			assert.Equal(t, "https://github.com/example/repo/pull/42", exported.ToolPRLinks[0].URL)
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 2)
			assert.Equal(t, structured.URL, got.PRLinks[0].URL)
			assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[1].URL)
			recorded, err := d.GetSessionStructuredPRLinks(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Equal(t, []PRLink{structured}, recorded)
		})
	}
}

func TestStagedWriteReplacesToolPRLinks(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "staged-tools", Project: "project", Machine: "local", Agent: "codex"}
	first, ok := parser.NewPRLink("https://github.com/example/repo/pull/1", "example/repo", 1)
	require.True(t, ok)
	second, ok := parser.NewPRLink("https://github.com/example/repo/pull/2", "example/repo", 2)
	require.True(t, ok)
	s.ToolPRLinks = []PRLink{first}
	require.NoError(t, d.WriteSessionBatchStaged(t.Context(), SessionBatchWrite{Session: s, ReplaceMessages: true}, newScratchStagedResults(t), nil, nil))
	s.ToolPRLinks = []PRLink{second}
	require.NoError(t, d.WriteSessionBatchStaged(t.Context(), SessionBatchWrite{Session: s, ReplaceMessages: true}, newScratchStagedResults(t), nil, nil))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, []PRLink{second}, got.PRLinks)
}

func TestArchiveCopyRetainsToolPRLinks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		policy       config.ArchiveContent
		old, trashed bool
	}{
		{"plain", config.ArchiveContentFull, false, false},
		{"usage-old", config.ArchiveContentUsage, true, false},
		{"trashed", config.ArchiveContentFull, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testDB(t)
			structured, ok := parser.NewPRLink("https://github.com/example/repo/pull/41", "example/repo", 41)
			require.True(t, ok)
			tool, ok := parser.NewPRLink("https://github.com/example/repo/pull/42", "example/repo", 42)
			require.True(t, ok)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "copy-links", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{structured}, ToolPRLinks: []PRLink{tool}}))
			require.NoError(t, source.ReplaceSessionMessages(t.Context(), "copy-links", []Message{
				{SessionID: "copy-links", Ordinal: 0, Role: "user", Content: "Review https://github.com/example/repo/pull/44"},
				{SessionID: "copy-links", Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}},
			}))
			if tc.old {
				require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS artifact_sessions_update_queue; ALTER TABLE sessions DROP COLUMN tool_pr_links`); err != nil {
						return err
					}
					return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
				}))
			}
			if tc.trashed {
				require.NoError(t, source.SoftDeleteSession(t.Context(), "copy-links"))
			}
			destination := testDB(t)
			destination.SetArchiveContent(tc.policy)
			var err error
			if tc.trashed {
				_, err = destination.CopyTrashedDataFrom(t.Context(), source.Path())
			} else {
				_, err = destination.CopyOrphanedDataFromExcluding(t.Context(), source.Path(), nil)
			}
			require.NoError(t, err)
			if tc.trashed {
				_, err = destination.RestoreSession(t.Context(), "copy-links")
				require.NoError(t, err)
			}
			got, err := destination.GetSessionFull(t.Context(), "copy-links")
			require.NoError(t, err)
			require.NotNil(t, got)
			want := []PRLink{structured, tool}
			if !tc.policy.UsageOnly() {
				prompt, ok := parser.NewPRLink("https://github.com/example/repo/pull/44", "example/repo", 44)
				require.True(t, ok)
				want = append(want, prompt)
			}
			assert.ElementsMatch(t, want, got.PRLinks)
			recorded, err := destination.GetSessionStructuredPRLinks(t.Context(), "copy-links")
			require.NoError(t, err)
			assert.Equal(t, []PRLink{structured}, recorded)
		})
	}
}

func TestArchiveCopyRecoversRecordedRemote(t *testing.T) {
	for _, tc := range []struct {
		name, recorded string
		want           []string
	}{
		{"recorded", "git@github.com:example/source.git", []string{"https://github.com/example/source/pull/42", "https://github.com/example/explicit/pull/43"}},
		{"unrecorded", "", []string{"https://github.com/example/explicit/pull/43"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testDB(t)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "copy-recorded", Project: "project", Machine: "local", Agent: "codex", Cwd: "/repo"}))
			require.NoError(t, source.ReplaceSessionMessages(t.Context(), "copy-recorded", []Message{{SessionID: "copy-recorded", Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{
				{Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`},
				{Category: "Bash", InputJSON: `{"command":"gh pr view 43 -R example/explicit"}`},
			}}}))
			require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `UPDATE session_project_identity_snapshots SET recorded_git_remote=?, git_remote=?, git_remote_name='origin', remote_candidate_count=1 WHERE session_id='copy-recorded'`, tc.recorded, "https://github.com/example/checkout.git"); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS artifact_sessions_update_queue; ALTER TABLE sessions DROP COLUMN tool_pr_links`); err != nil {
					return err
				}
				return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
			}))
			dest := testDB(t)
			dest.SetArchiveContent(config.ArchiveContentTranscripts)
			_, err := dest.CopyOrphanedDataFromExcluding(t.Context(), source.Path(), nil)
			require.NoError(t, err)
			got, err := dest.GetSession(t.Context(), "copy-recorded")
			require.NoError(t, err)
			var urls []string
			for _, link := range got.PRLinks {
				urls = append(urls, link.URL)
			}
			assert.ElementsMatch(t, tc.want, urls)
		})
	}
}

func TestToolPRLinksPreserveOmittedInputs(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "omitted-tools", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE tool_calls SET input_json=NULL WHERE session_id=?`, s.ID); err != nil {
			return err
		}
		return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID)
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 43 -R example/repo"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, 42, got.PRLinks[0].Number)
	assert.Equal(t, 43, got.PRLinks[1].Number)
}

func TestToolPRLinksAppendTracksSharedShell(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want int
	}{{"Bash", 1}, {"exec_command", 2}} {
		t.Run(tc.tool, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "shared-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: tc.tool, Category: "Bash", InputJSON: `{"command":"cd /elsewhere"}`}}}}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: tc.tool, Category: "Bash", InputJSON: `{"command":"gh pr view 42; gh pr view 43 -R example/other"}`}}}}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, tc.want)
			assert.Equal(t, "https://github.com/example/other/pull/43", got.PRLinks[tc.want-1].URL)
		})
	}
}

func TestToolPRLinksShellContextSurvivesOmittedInputs(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "omitted-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"echo hi"}`}}}}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE tool_calls SET input_json=NULL WHERE session_id=?`, s.ID)
		return err
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, 42, got.PRLinks[0].Number)
}

func TestToolPRLinksShellContextRefreshAndReplacement(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "refresh-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{message(0, "cd /elsewhere")}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 41")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{message(0, "echo hi")}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 42")}))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, 42, got.PRLinks[0].Number)
}

func TestToolPRLinksShellContextReopenAndBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if d != nil {
			_ = d.Close()
		}
	})
	s := Session{ID: "reopen-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, "echo hi")}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN pr_shell_tainted`)
		return err
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 41")}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE tool_calls SET input_json=NULL WHERE session_id=?`, s.ID)
		return err
	}))
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(2, "gh pr view 42")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, 41, got.PRLinks[0].Number)
	assert.Equal(t, 42, got.PRLinks[1].Number)
}

func TestToolPRLinksShellContextCwdChange(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "cwd-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"cd /work/repo"}`}}}}))
	s.Cwd = "/work/other"
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestToolPRLinksShellContextMissingNewInput(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "missing-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash"}}}}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}
