package db

import (
	"database/sql"
	"fmt"
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

func TestToolPRLinksStoredMessages(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "tool-links", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	message := func(ordinal int, category, input string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: category, Category: category, InputJSON: input}}}
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{message(0, "Bash", `{"command":"gh pr view 42 -R example/repo"}`)}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "Task", `{"prompt":"Review https://github.com/example/repo/pull/43"}`)}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, 42, got.PRLinks[0].Number)
	assert.Equal(t, 43, got.PRLinks[1].Number)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{message(0, "Bash", `{"command":"gh pr view 44 -R example/repo"}`)}))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, 44, got.PRLinks[0].Number)
}

func TestPRLinksDeferredDaemonBackfill(t *testing.T) {
	structuredLink, ok := parser.NewPRLink("https://github.com/example/repo/pull/1", "example/repo", 1)
	require.True(t, ok)

	for _, tc := range []struct {
		name        string
		fromVersion int
	}{
		{name: "from pre-prompt version", fromVersion: promptPRLinksDataVersion - 1},
		{name: "from pre-tool version", fromVersion: toolPRLinksDataVersion - 1},
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

			promptSession := Session{ID: "s-prompt", Project: "project", Machine: "local", Agent: "claude", PRLinks: []PRLink{structuredLink}}
			bashSession := Session{ID: "s-bash", Project: "project", Machine: "local", Agent: "claude"}
			taskSession := Session{ID: "s-task", Project: "project", Machine: "local", Agent: "claude"}
			plainSession := Session{ID: "s-plain", Project: "project", Machine: "local", Agent: "claude"}
			for _, s := range []Session{promptSession, bashSession, taskSession, plainSession} {
				require.NoError(t, d.UpsertSession(t.Context(), s))
			}

			require.NoError(t, d.ReplaceSessionMessages(t.Context(), promptSession.ID, []Message{
				{SessionID: promptSession.ID, Ordinal: 0, Role: "user", Content: "review https://github.com/example/repo/pull/2"},
			}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), bashSession.ID, []Message{
				{SessionID: bashSession.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{
					{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 3 -R example/repo"}`},
				}},
			}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), taskSession.ID, []Message{
				{SessionID: taskSession.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{
					{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"check https://github.com/example/repo/pull/4"}`},
				}},
			}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), plainSession.ID, []Message{
				{SessionID: plainSession.ID, Ordinal: 0, Role: "user", Content: "just a normal prompt"},
				{SessionID: plainSession.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{
					{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"git status"}`},
				}},
			}))

			const sentinelModified = "2020-01-01T00:00:00.000Z"
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if tc.fromVersion < promptPRLinksDataVersion {
					if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET structured_pr_links='', pr_links=? WHERE id=?`, EncodePRLinks([]PRLink{structuredLink}), promptSession.ID); err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links='' WHERE id IN (?,?,?)`, bashSession.ID, taskSession.ID, plainSession.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET local_modified_at=? WHERE id=?`, sentinelModified, plainSession.ID); err != nil {
					return err
				}
				return writeUserVersion(t.Context(), tx, tc.fromVersion)
			}))
			require.NoError(t, d.Close())
			d = nil

			// Daemon open advances user_version and copies legacy structured links
			// without blocking on prompt/tool derivation.
			d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
			require.NoError(t, err)
			assert.True(t, d.PRLinksBackfillPending())

			structured, err := d.GetSessionStructuredPRLinks(t.Context(), promptSession.ID)
			require.NoError(t, err)
			assert.Equal(t, []PRLink{structuredLink}, structured)

			gotBash, err := d.GetSession(t.Context(), bashSession.ID)
			require.NoError(t, err)
			assert.Empty(t, gotBash.PRLinks, "daemon open should defer PR link derivation")

			// A second daemon open (e.g. startup worker handoff or restart before
			// backfill completes) keeps the durable pending state intact.
			require.NoError(t, d.Close())
			d = nil
			d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
			require.NoError(t, err)
			assert.True(t, d.PRLinksBackfillPending())

			require.NoError(t, d.BackfillPRLinks(t.Context()))
			assert.False(t, d.PRLinksBackfillPending())

			gotPrompt, err := d.GetSession(t.Context(), promptSession.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{
				"https://github.com/example/repo/pull/1",
				"https://github.com/example/repo/pull/2",
			}, prLinkURLs(gotPrompt.PRLinks))

			gotBash, err = d.GetSession(t.Context(), bashSession.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"https://github.com/example/repo/pull/3"}, prLinkURLs(gotBash.PRLinks))

			gotTask, err := d.GetSession(t.Context(), taskSession.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"https://github.com/example/repo/pull/4"}, prLinkURLs(gotTask.PRLinks))

			gotPlain, err := d.GetSession(t.Context(), plainSession.ID)
			require.NoError(t, err)
			assert.Empty(t, gotPlain.PRLinks)

			var plainModified string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT local_modified_at FROM sessions WHERE id=?`, plainSession.ID).Scan(&plainModified))
			assert.Equal(t, sentinelModified, plainModified, "unchanged session must not be rewritten")

			var markerCount int
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM stats WHERE key=?`, prLinksBackfillStateKey).Scan(&markerCount))
			assert.Zero(t, markerCount)

			// Reopening after completion does not re-queue backfill.
			require.NoError(t, d.Close())
			d = nil
			d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
			require.NoError(t, err)
			assert.False(t, d.PRLinksBackfillPending())
		})
	}
}

func TestPRLinksDeferredBackfillResumesAcrossBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if d != nil {
			_ = d.Close()
		}
	})

	total := prLinksBackfillBatchSize + 10
	firstID := "codex:0000:first"
	boundaryID := fmt.Sprintf("codex:%04d:with:colons", prLinksBackfillBatchSize-1)
	lastID := fmt.Sprintf("codex:%04d:last", total-1)

	for i := range total {
		id := fmt.Sprintf("codex:%04d:item", i)
		switch i {
		case 0:
			id = firstID
		case prLinksBackfillBatchSize - 1:
			id = boundaryID
		case total - 1:
			id = lastID
		}
		require.NoError(t, d.UpsertSession(t.Context(), Session{ID: id, Project: "project", Machine: "local", Agent: "codex"}))
	}
	for _, item := range []struct {
		id  string
		num int
	}{
		{firstID, 10},
		{boundaryID, 20},
		{lastID, 30},
	} {
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), item.id, []Message{
			{SessionID: item.id, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{
				{ToolName: "Bash", Category: "Bash", InputJSON: fmt.Sprintf(`{"command":"gh pr view %d -R example/repo"}`, item.num)},
			}},
		}))
	}
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''`); err != nil {
			return err
		}
		return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
	}))
	require.NoError(t, d.Close())
	d = nil

	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	require.True(t, d.PRLinksBackfillPending())

	// Commit only the first batch, stopping before the tail batch.
	scanned, updated, done, err := d.backfillPRLinksBatch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, prLinksBackfillBatchSize, scanned)
	assert.Equal(t, 2, updated)
	assert.False(t, done)
	assert.True(t, d.PRLinksBackfillPending())

	gotFirst, err := d.GetSession(t.Context(), firstID)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/10"}, prLinkURLs(gotFirst.PRLinks))

	gotBoundary, err := d.GetSession(t.Context(), boundaryID)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/20"}, prLinkURLs(gotBoundary.PRLinks))

	gotLast, err := d.GetSession(t.Context(), lastID)
	require.NoError(t, err)
	assert.Empty(t, gotLast.PRLinks, "second batch should not have run yet")

	// A non-daemon open resumes from the persisted cursor (including colons in
	// the session ID) and finishes the remaining batch synchronously.
	require.NoError(t, d.Close())
	d = nil
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, d.PRLinksBackfillPending())

	gotLast, err = d.GetSession(t.Context(), lastID)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/30"}, prLinkURLs(gotLast.PRLinks))
}

func TestPRLinksBackfillFreshAndUsageOnlyAndConcurrentDelete(t *testing.T) {
	t.Run("fresh archive queues no backfill", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.db")
		d, err := OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, d.Close()) })
		assert.False(t, d.PRLinksBackfillPending())
	})

	t.Run("usage-only archive skips deferred backfill", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "usage.db")
		d, err := OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		s := Session{ID: "usage-s", Project: "project", Machine: "local", Agent: "claude"}
		require.NoError(t, d.UpsertSession(t.Context(), s))
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, []Message{
			{SessionID: s.ID, Ordinal: 0, Role: "user", Content: "review https://github.com/example/repo/pull/50"},
		}))
		require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''`); err != nil {
				return err
			}
			return writeUserVersion(t.Context(), tx, promptPRLinksDataVersion-1)
		}))
		require.NoError(t, d.Close())

		// Queue via full daemon open, then reopen under usage-only policy.
		d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
		require.NoError(t, err)
		require.True(t, d.PRLinksBackfillPending())
		require.NoError(t, d.Close())

		d, err = OpenIsolatedWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, d.Close()) })
		assert.False(t, d.PRLinksBackfillPending())
		got, err := d.GetSession(t.Context(), s.ID)
		require.NoError(t, err)
		assert.Empty(t, got.PRLinks)
	})

	t.Run("deleted session between scan and commit is skipped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "concurrent.db")
		d, err := OpenIsolated(t.Context(), path)
		require.NoError(t, err)
		for _, id := range []string{"a-deleted", "b-kept"} {
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: id, Project: "project", Machine: "local", Agent: "claude"}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []Message{
				{SessionID: id, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{
					{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 99 -R example/repo"}`},
				}},
			}))
		}
		require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links=''`); err != nil {
				return err
			}
			return writeUserVersion(t.Context(), tx, toolPRLinksDataVersion-1)
		}))
		require.NoError(t, d.Close())

		d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, d.Close()) })

		rawState, scope, batch, dirtyIDs, done, err := d.scanPRLinksBackfillBatch(t.Context())
		require.NoError(t, err)
		require.False(t, done)
		require.ElementsMatch(t, []string{"a-deleted", "b-kept"}, dirtyIDs)

		// Delete one session after scan and before commit.
		require.NoError(t, d.DeleteSession(t.Context(), "a-deleted"))

		commitDone, updated, err := d.commitPRLinksBackfillBatch(t.Context(), rawState, scope, batch, dirtyIDs)
		require.NoError(t, err)
		assert.True(t, commitDone)
		assert.Equal(t, 1, updated)

		gotKept, err := d.GetSession(t.Context(), "b-kept")
		require.NoError(t, err)
		assert.Equal(t, []string{"https://github.com/example/repo/pull/99"}, prLinkURLs(gotKept.PRLinks))
	})
}

func prLinkURLs(links []PRLink) []string {
	urls := make([]string, len(links))
	for i, link := range links {
		urls[i] = link.URL
	}
	return urls
}
