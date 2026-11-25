package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

func prepareLegacyPRLinksArchive(t *testing.T, d *DB, version int, dropColumn bool) {
	t.Helper()
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=structured_pr_links`); err != nil {
			return err
		}
		if dropColumn {
			if _, err := tx.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN structured_pr_links`); err != nil {
				return err
			}
		}
		return writeUserVersion(t.Context(), tx, version)
	}))
}

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
		prepareLegacyPRLinksArchive(t, d, promptPRLinksDataVersion-1, true)
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

func TestToolPRLinksPendingProjectIdentityUpsertRetainsUsageOnlyLinks(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/42", "o/r", 42)
	require.True(t, ok)
	s := Session{ID: "identity-update", Project: "old-project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
	require.NoError(t, d.UpsertSession(t.Context(), s))

	updated := s
	updated.Project = "new-project"
	updated.PRLinks = nil
	obs := export.ProjectIdentityObservation{SessionID: s.ID, Project: updated.Project, Machine: s.Machine}
	sourceMissing, err := d.UpsertSessionPendingContentWithProjectIdentity(t.Context(), updated, obs, "")
	require.NoError(t, err)
	assert.False(t, sourceMissing)

	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, []PRLink{link}, got.PRLinks)
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
				prepareLegacyPRLinksArchive(t, source, promptPRLinksDataVersion-1, true)
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

func TestToolPRLinksCopiedArchives(t *testing.T) {
	for _, tc := range []struct {
		name                string
		source, destination config.ArchiveContent
		checkout            bool
		legacy              bool
	}{
		{"legacy full to transcripts", config.ArchiveContentFull, config.ArchiveContentTranscripts, false, true},
		{"full to usage", config.ArchiveContentFull, config.ArchiveContentUsage, false, false},
		{"transcripts to usage", config.ArchiveContentTranscripts, config.ArchiveContentUsage, false, false},
		{"checkout remote", config.ArchiveContentFull, config.ArchiveContentFull, true, false},
		{"full checkout to usage", config.ArchiveContentFull, config.ArchiveContentUsage, true, false},
		{"transcripts checkout to usage", config.ArchiveContentTranscripts, config.ArchiveContentUsage, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testDB(t)
			source.SetArchiveContent(tc.source)
			for _, id := range []string{"orphan", "trashed"} {
				s := Session{ID: id, Project: "project", Machine: "local", Agent: "claude"}
				if tc.checkout {
					s.Cwd = "/work/repo"
				}
				require.NoError(t, source.UpsertSession(t.Context(), s))
				calls := []ToolCall{
					{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`},
					{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/43"}`},
				}
				if tc.checkout {
					obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: id, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://git.example/example/repo.git")
					require.NoError(t, source.UpsertProjectIdentityObservation(t.Context(), obs))
					calls = []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}
				}
				require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: id, Role: "assistant", ToolCalls: calls}}))
			}
			if tc.legacy {
				require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''`); err != nil {
						return err
					}
					if _, err := tx.ExecContext(t.Context(), `ALTER TABLE tool_calls DROP COLUMN pr_links`); err != nil {
						return err
					}
					return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
				}))
			}
			require.NoError(t, source.SoftDeleteSession(t.Context(), "trashed"))
			dest := testDB(t)
			dest.SetArchiveContent(tc.destination)
			_, err := dest.CopyTrashedDataFrom(t.Context(), source.Path())
			require.NoError(t, err)
			_, err = dest.CopyOrphanedDataFrom(t.Context(), source.Path())
			require.NoError(t, err)
			if tc.checkout {
				require.NoError(t, dest.CopySessionMetadataFrom(t.Context(), source.Path()))
			}
			for _, id := range []string{"orphan", "trashed"} {
				if tc.legacy && id == "trashed" {
					continue
				}
				if tc.checkout {
					var encoded string
					require.NoError(t, dest.getReader().QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=?`, id).Scan(&encoded))
					links := DecodePRLinks(encoded)
					require.Len(t, links, 1)
					assert.Equal(t, "https://git.example/example/repo/pull/42", links[0].URL)
				} else if tc.legacy {
					got, err := dest.GetSession(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, got)
					require.Len(t, got.PRLinks, 2)
					assert.Equal(t, 42, got.PRLinks[0].Number)
					assert.Equal(t, 43, got.PRLinks[1].Number)
				} else {
					links, err := dest.GetSessionStructuredPRLinks(t.Context(), id)
					require.NoError(t, err)
					require.Len(t, links, 2)
					assert.Equal(t, 42, links[0].Number)
					assert.Equal(t, 43, links[1].Number)
				}
			}
		})
	}
}

func TestUsageOnlyCopyRetainsTranscriptRepositoryChange(t *testing.T) {
	source := testDB(t)
	source.SetArchiveContent(config.ArchiveContentTranscripts)
	s := Session{ID: "copied-context", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, source.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, source.UpsertProjectIdentityObservation(t.Context(), obs))
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"git remote set-url origin https://git.example/example/repo.git"}`}}}}))
	dest := testDB(t)
	dest.SetArchiveContent(config.ArchiveContentUsage)
	_, err := dest.CopyOrphanedDataFrom(t.Context(), source.Path())
	require.NoError(t, err)
	require.NoError(t, dest.CopySessionMetadataFrom(t.Context(), source.Path()))
	require.NoError(t, dest.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"gh pr view 42 -R example/repo"}`}}}}))
	got, err := dest.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestUsageOnlyBatchKeepsPRInputsAfterProjection(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	s := Session{ID: "projected-pr-inputs", Project: "project", Machine: "local", Agent: "claude"}
	messages := []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/43"}`},
	}}}
	s, messages = ProjectSessionForStoragePolicy(s, messages, config.ArchiveContentUsage)
	_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s, Messages: messages}})
	require.NoError(t, err)
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/43", got.PRLinks[1].URL)
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
	prepareLegacyPRLinksArchive(t, source, promptPRLinksDataVersion-1, true)
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
	t.Run("usage-only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "archive.db")
		d, err := OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		t.Cleanup(func() {
			if d != nil {
				_ = d.Close()
			}
		})
		link, ok := parser.NewPRLink("https://github.com/example/repo/pull/41", "example/repo", 41)
		require.True(t, ok)
		s := Session{ID: "usage-upgrade", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{link}}
		require.NoError(t, d.UpsertSession(t.Context(), s))
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}))
		prepareLegacyPRLinksArchive(t, d, toolPRLinksDataVersion-1, false)
		require.NoError(t, d.Close())
		d, err = OpenIsolatedWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
		require.NoError(t, err)
		got, err := d.GetSession(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Equal(t, []PRLink{link}, got.PRLinks)
	})
	for _, tc := range []struct {
		name, category, field string
		prompt, command       string
		want                  []string
	}{
		{"tool upgrade", "Bash", "command", "review https://github.com/example/repo/pull/41", "gh pr view 42 -R github.com/example/repo", []string{"https://github.com/example/repo/pull/41", "https://github.com/example/repo/pull/42"}},
		{"task pull", "Task", "prompt", "", "https://github.com/example/repo/pull/44", []string{"https://github.com/example/repo/pull/44"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.db")
			d, err := OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() {
				if d != nil {
					_ = d.Close()
				}
			})
			s := Session{ID: "tool-upgrade", Project: "project", Machine: "local", Agent: "claude"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			messages := []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: tc.category, Category: tc.category, InputJSON: `{"` + tc.field + `":"` + tc.command + `"}`}}}}
			if tc.prompt != "" {
				messages = append(messages, Message{SessionID: s.ID, Ordinal: 0, Role: "user", Content: tc.prompt})
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				promptLinks := ""
				if tc.prompt != "" {
					promptLinks = EncodePRLinks(PromptPRLinks(messages))
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=? WHERE id=?`, promptLinks, s.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `ALTER TABLE tool_calls DROP COLUMN pr_links`); err != nil {
					return err
				}
				return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
			}))
			require.NoError(t, d.Close())
			d, err = OpenIsolatedWithArchiveContent(t.Context(), path, config.ArchiveContentFull)
			require.NoError(t, err)
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			var urls []string
			for _, link := range got.PRLinks {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tc.want, urls)
		})
	}
}

func TestToolPRLinksArchiveStorage(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			session := Session{ID: "links", Project: "project", Agent: "claude", Machine: "local"}
			require.NoError(t, d.UpsertSession(t.Context(), session))
			message := func(ordinal int, tool, category, input string) Message {
				return Message{SessionID: session.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: tool, Category: category, InputJSON: input}}}
			}
			check := func(want []int) {
				t.Helper()
				assertLinks := func() {
					stored, err := d.GetSessionFull(t.Context(), session.ID)
					require.NoError(t, err)
					require.NotNil(t, stored)
					numbers := make([]int, len(stored.PRLinks))
					for i, link := range stored.PRLinks {
						numbers[i] = link.Number
					}
					assert.Equal(t, want, numbers)
				}
				assertLinks()
				if policy == config.ArchiveContentTranscripts {
					stored, err := d.GetAllMessages(t.Context(), session.ID)
					require.NoError(t, err)
					require.NotEmpty(t, stored)
					for _, msg := range stored {
						for _, call := range msg.ToolCalls {
							assert.Empty(t, call.InputJSON)
						}
					}
					require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
						return refreshSessionPromptPRLinksTx(t.Context(), tx, session.ID)
					}))
				}
				assertLinks()
			}
			for i, input := range []string{`{"command":"gh pr view 5 -R example/repo"}`, `{"command":"gh pr view 6 -R example/repo"}`} {
				_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: session, Messages: []Message{message(i, "Bash", "Bash", input)}, ReplaceMessages: i == 0}})
				require.NoError(t, err)
			}
			check([]int{5, 6})
			if policy == config.ArchiveContentTranscripts {
				stored, err := d.GetAllMessages(t.Context(), session.ID)
				require.NoError(t, err)
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), session.ID, stored))
				check([]int{5, 6})
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), session.ID, []Message{message(0, "Task", "Task", `{"prompt":"Review https://github.com/example/repo/pull/7"}`)}))
			want := []int{7}
			if policy.UsageOnly() {
				want = []int{5, 6, 7}
			}
			check(want)
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "Bash", "Bash", `{"command":"gh pr view 8 -R example/repo"}`)}))
			want = append(want, 8)
			check(want)
			_, err := d.WriteSessionIncremental(t.Context(), session.ID, []Message{message(2, "Bash", "Bash", `{"command":"gh pr view 9 -R example/repo"}`)}, IncrementalSessionUpdate{})
			require.NoError(t, err)
			want = append(want, 9)
			check(want)
			_, err = d.WriteSessionIncremental(t.Context(), session.ID, []Message{{SessionID: session.ID, Ordinal: 2, Role: "assistant", Content: "Done"}}, IncrementalSessionUpdate{ReplaceFromOrdinal: new(2)})
			require.NoError(t, err)
			if !policy.UsageOnly() {
				want = want[:len(want)-1]
			}
			check(want)
			if policy.UsageOnly() {
				require.NoError(t, d.UpsertSession(t.Context(), session))
				check(want)
			}
		})
	}
}

func TestUsageOnlyPRLinksPreservesRepositoryUncertaintyAcrossWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "usage archive")
	d.SetArchiveContent(config.ArchiveContentUsage)
	s := Session{ID: "usage-context", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, "gh pr view 41 -R github.com/example/repo")}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "git remote set-url origin https://git.example/example/repo.git")}))
	require.NoError(t, d.Close())
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(2, "gh pr view 42 -R example/repo; gh pr view 43 -R git.example/example/repo")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	exported, err := d.GetArtifactExportSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.True(t, exported.PRRepositoryChanged)
	var urls []string
	for _, link := range got.PRLinks {
		urls = append(urls, link.URL)
	}
	assert.Equal(t, []string{"https://github.com/example/repo/pull/41", "https://git.example/example/repo/pull/43"}, urls)
	_, err = d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s, Messages: []Message{message(3, "gh pr view 44 -R example/repo")}}})
	require.NoError(t, err)
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	for _, link := range got.PRLinks {
		assert.NotEqual(t, 44, link.Number)
	}
	require.NoError(t, d.Close())
	dest := testDB(t)
	dest.SetArchiveContent(config.ArchiveContentUsage)
	_, err = dest.CopyOrphanedDataFrom(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, dest.InsertMessages(t.Context(), []Message{message(4, "gh pr view 45 -R example/repo")}))
	got, err = dest.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	for _, link := range got.PRLinks {
		assert.NotEqual(t, 45, link.Number)
	}
}

func TestUsageOnlyPRLinksOversizedInputMatchesSameBatch(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			s := Session{ID: fmt.Sprintf("oversized-%v", separate), Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			message := func(ordinal int, input string) Message {
				return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: input}}}
			}
			first := message(0, `{"cmd":"echo `+strings.Repeat("x", 256*1024)+`"}`)
			second := message(1, `{"cmd":"gh pr view 42 -R example/repo"}`)
			if separate {
				require.NoError(t, d.InsertMessages(t.Context(), []Message{first}))
				require.NoError(t, d.InsertMessages(t.Context(), []Message{second}))
			} else {
				require.NoError(t, d.InsertMessages(t.Context(), []Message{first, second}))
			}
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Empty(t, got.PRLinks)
		})
	}
}

func TestUsageOnlyPRLinksFunctionRepositoryChange(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	s := Session{ID: "function-context", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	for i, input := range []string{`{"cmd":"f(){ git remote set-url origin https://ghe.example/example/repo.git; }; f"}`, `{"cmd":"gh pr view 42 -R example/repo"}`} {
		require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Ordinal: i, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: input}}}}))
	}
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestUsageOnlyPRLinksPreferRecordedCheckoutRemote(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	s := Session{ID: "recorded-context", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	SetUsagePRRemote(&s, "https://ghe.example/example/repo.git")
	message := Message{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"gh pr view 42 -R example/repo"}`}}}
	_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s, Messages: []Message{message}, ReplaceMessages: true}})
	require.NoError(t, err)
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[0].URL)
}

func TestUsageOnlyPRLinksTemporaryEnvironmentDoesNotPersist(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	s := Session{ID: "usage-env", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"` + command + `"}`}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, "export GH_HOST=git.example")}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 42 -R example/repo")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[0].URL)
}

func TestUsageOnlyPRRepositoryChangeQueuesArtifactExport(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	seedArtifactOrigin(t, d)
	s := Session{ID: "usage-export", Project: "project", Machine: "local", Agent: "codex"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	clearArtifactExportQueue(t, d)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return retainUsageToolPRLinksTx(t.Context(), tx, s.ID, []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"git remote set-url origin https://git.example/example/repo.git"}`}}}})
	}))
	queued, err := d.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	assert.Equal(t, s.ID, queued[0].SessionID)
}

func TestToolPRLinksCheckoutHost(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			s := Session{ID: "checkout-host", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://git.example/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			calls := []ToolCall{
				{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 41"}`},
				{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`},
				{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R github.com/example/repo"}`},
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: calls}}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			var urls []string
			for _, link := range got.PRLinks {
				urls = append(urls, link.URL)
			}
			want := []string{"https://git.example/example/repo/pull/42", "https://github.com/example/repo/pull/42"}
			if policy == config.ArchiveContentFull {
				want = append([]string{"https://git.example/example/repo/pull/41"}, want...)
			}
			assert.Equal(t, want, urls)
		})
	}
}

func TestToolPRLinksSharedShellDirectoryAppend(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "shared-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, input string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: input}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, `{"command":"cd /work/other"}`)}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, `{"command":"gh pr view 62"}`)}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestTranscriptToolPRLinksWithoutInputs(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, remote string
		hosts                       []string
		number                      int
	}{
		{"directory", `{"command":"cd /work/other"}`, `{"command":"gh pr view 42; gh pr view 43 -R github.com/example/repo"}`, "https://github.com/example/repo.git", []string{"github.com"}, 43},
		{"host", `{"command":"export GH_HOST=enterprise.example"}`, `{"command":"gh pr view 42 -R example/repo; gh pr view 43 -R git.example/example/repo"}`, "https://git.example/example/repo.git", []string{"git.example"}, 43},
		{"explicit case", `{"command":"echo ready"}`, `{"command":"gh pr view 42 -R example/repo; gh pr view 42 -R github.com/Example/Repo"}`, "https://git.example/example/repo.git", []string{"git.example", "github.com"}, 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, messages := ProjectSessionForStoragePolicy(Session{}, []Message{{ToolCalls: []ToolCall{
				{ToolName: "Bash", Category: "Bash", InputJSON: tc.first}, {ToolName: "Bash", Category: "Bash", InputJSON: tc.second},
			}}}, config.ArchiveContentTranscripts)
			for _, call := range messages[0].ToolCalls {
				assert.Empty(t, call.InputJSON)
			}
			links, err := ToolCallPRLinks(t.Context(), messages, "/work/repo", tc.remote)
			require.NoError(t, err)
			var hosts []string
			for _, link := range links {
				hosts = append(hosts, link.Host)
				assert.Equal(t, tc.number, link.Number)
			}
			assert.ElementsMatch(t, tc.hosts, hosts)
		})
	}
}

func TestCommandPRRecordedRemoteRepair(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "recorded-remote-repair", Project: "project", Machine: "local", Agent: "codex", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{Category: "Bash", ToolName: "exec_command", InputJSON: `{"cmd":"gh pr view 42"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[0].URL)
}
