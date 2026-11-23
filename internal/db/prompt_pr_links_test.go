package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
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
	for _, tc := range []struct {
		name            string
		version         int
		prompt, command string
		want            []string
		wantState       bool
	}{
		{"prompt upgrade", promptPRLinksDataVersion - 1, "review https://github.com/example/repo/pull/41", "gh pr view 42 -R example/repo", []string{"https://github.com/example/repo/pull/41", "https://github.com/example/repo/pull/42"}, true},
		{"tool upgrade", toolPRLinksDataVersion - 1, "review https://github.com/example/repo/pull/41", "gh pr view 42 -R example/repo", []string{"https://github.com/example/repo/pull/41", "https://github.com/example/repo/pull/42"}, true},
		{"quoted command", toolPRLinksDataVersion - 1, "", "g''h pr view 43 -R example/repo", []string{"https://github.com/example/repo/pull/43"}, true},
		{"unrelated command", toolPRLinksDataVersion - 1, "", "echo high", nil, false},
		{"unrelated HTTP command", toolPRLinksDataVersion - 1, "", "curl https://example.com/api", nil, false},
		{"task pull", toolPRLinksDataVersion - 1, "", "https://github.com/example/repo/pull/44", []string{"https://github.com/example/repo/pull/44"}, true},
		{"task pulls", toolPRLinksDataVersion - 1, "", "https://code.example/example/repo/pulls/45", []string{"https://code.example/example/repo/pulls/45"}, true},
		{"task merge request", toolPRLinksDataVersion - 1, "", "https://git.example/example/repo/-/merge_requests/46", []string{"https://git.example/example/repo/-/merge_requests/46"}, true},
		{"task encoded route", toolPRLinksDataVersion - 1, "", "https://github.com/example/repo/%70ull/47", []string{"https://github.com/example/repo/pull/47"}, true},
		{"task JSON escaped route", toolPRLinksDataVersion - 1, "", `https://github.com/example/repo/\u0070ull/48`, []string{"https://github.com/example/repo/pull/48"}, true},
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
			category, field := "Bash", "command"
			if strings.HasPrefix(tc.name, "task ") {
				category, field = "Task", "prompt"
			}
			messages := []Message{{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: category, Category: category, InputJSON: `{"` + field + `":"` + tc.command + `"}`}}}}
			if tc.prompt != "" {
				messages = append(messages, Message{SessionID: s.ID, Ordinal: 0, Role: "user", Content: tc.prompt})
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_links='', pr_shell_tainted=NULL WHERE id=?`, s.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN pr_shell_tainted`); err != nil {
					return err
				}
				return writeUserVersion(t.Context(), tx, tc.version)
			}))
			require.NoError(t, d.Close())
			d, err = OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			var urls []string
			for _, link := range got.PRLinks {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tc.want, urls)
			var state sql.NullInt64
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT pr_shell_tainted FROM sessions WHERE id=?`, s.ID).Scan(&state))
			assert.Equal(t, tc.wantState, state.Valid)
		})
	}
}

func TestToolCallPRLinksSkipsSimpleCommands(t *testing.T) {
	const input = `{"command":"rg -n 'example' src"}`
	d := testDB(t)
	s := Session{ID: "simple-commands", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	messages := []Message{{SessionID: s.ID, Role: "assistant"}}
	for range 100 {
		messages[0].ToolCalls = append(messages[0].ToolCalls, ToolCall{ToolName: "Bash", Category: "Bash", InputJSON: input})
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages))
	tx, err := d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	full := testing.AllocsPerRun(2, func() {
		for range 100 {
			_, _, err := callPRLinks(t.Context(), "Bash", "Bash", input, "/work/repo", "", parser.PRShellContext{})
			require.NoError(t, err)
		}
	})
	optimized := testing.AllocsPerRun(2, func() {
		links, state, err := storedToolCallPRLinksTx(t.Context(), contextTransaction{ctx: t.Context(), tx: tx}, s.ID, "/work/repo", "", nil)
		require.NoError(t, err)
		assert.Empty(t, links)
		assert.Equal(t, parser.PRShellContext{}, state)
	})
	assert.Less(t, optimized, full*0.85)
}

func TestCodeModeWithoutShellAvoidsParser(t *testing.T) {
	const input = `text(await tools.clock__curr_time({}));`
	full := testing.AllocsPerRun(2, func() {
		for range 100 {
			_, _, err := codeModePRLinks(t.Context(), input, "", "", parser.PRShellContext{})
			require.NoError(t, err)
		}
	})
	optimized := testing.AllocsPerRun(2, func() {
		for range 100 {
			links, state, err := replayPRLinks(t.Context(), "Bash", "exec", input, "", "", parser.PRShellContext{})
			require.NoError(t, err)
			assert.Empty(t, links)
			assert.Equal(t, parser.PRShellContext{}, state)
		}
	})
	assert.Less(t, optimized, full*0.5)
	for _, input := range []string{
		`text(await tools.clock__curr_time({}));`,
		`await tools.wait({"duration_ms":1000})`,
		`tools.wait({"text":"exec_command"})`,
	} {
		require.True(t, simpleCodeModeWithoutShell(input), input)
		for bits := range 8 {
			before := parser.PRShellContext{CheckoutUncertain: bits&1 != 0, HostUncertain: bits&2 != 0, RepoUncertain: bits&4 != 0}
			links, after, err := codeModePRLinks(t.Context(), input, "", "", before)
			require.NoError(t, err)
			assert.Empty(t, links)
			assert.Equal(t, before, after)
		}
	}
	for _, input := range []string{
		`tools.exec_command({"cmd":"gh pr view 42"})`,
		`text(await tools.wait({"value":tools.exec_command({cmd:'pwd'})}))`,
		`tools.wait({`,
		`text(await tools.wait({})); broken`,
		strings.Repeat(" ", 256*1024) + `tools.wait({})`,
	} {
		assert.False(t, simpleCodeModeWithoutShell(input), input)
	}
}

func TestSimplePRInertCommandParity(t *testing.T) {
	for _, command := range []string{"pwd", "ls -al /work/repo", "cat README.md", "head -n 10 README.md", "tail -n 10 README.md", "wc -l README.md", "rg pattern src", "echo hello", "go version", "npm install", "curl https://example.com/api", "/usr/bin/python3 script.py", "echo gh pr view 42", "rg -n 'gh pr view 42' src", "echo '$(gh pr view 42)'", `rg "hello world" src`, `echo "a\"b"`} {
		t.Run(command, func(t *testing.T) {
			input := `{"command":` + strconv.Quote(command) + `}`
			require.True(t, simplePRInertCommand("Bash", input))
			for state := range 8 {
				before := parser.PRShellContext{CheckoutUncertain: state&1 != 0, HostUncertain: state&2 != 0, RepoUncertain: state&4 != 0}
				links, after, err := callPRLinks(t.Context(), "Bash", "Bash", input, "/work/repo", "https://github.com/example/repo.git", before)
				require.NoError(t, err)
				assert.Empty(t, links)
				assert.Equal(t, before, after)
			}
		})
	}
	for _, input := range []string{`{"command":"echo $(gh pr view 42)"}`, `{"command":"echo 'unclosed"}`, `{"command":"echo \"$HOME\""}`, `{"command":"pwd; cd /other"}`, `{"command":"git remote set-url origin example"}`, `{"command":"/usr/bin/GH.exe pr view 42"}`, `{"command":"claude review"}`, `{"command":"codex exec review"}`, `{"command":"sh -c script"}`, `{"command":"env GH_REPO=x gh pr view 42"}`, `{"command":"GH_REPO=example/repo"}`, `{"command":"export GH_HOST=example"}`, `{"command":"if true"}`, `{"command":"for"}`, `{"command":"pwd`, ""} {
		assert.False(t, simplePRInertCommand("Bash", input), input)
	}
	assert.False(t, simplePRInertCommand("Bash", `{"command":"echo `+strings.Repeat("x", 256*1024)+`"}`))
	assert.False(t, simplePRInertCommand("exec", `{"command":"pwd"}`))
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

func TestShellTaintHistory(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "shell-history", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, "cd /other")}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_shell_tainted=NULL WHERE id=?`, s.ID)
		return err
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 42")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestToolPRLinksRecordedRemote(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "recorded-remote", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/recorded.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE session_project_identity_snapshots SET git_remote=?, remote_candidate_count=1 WHERE session_id=?`, "https://github.com/example/checkout.git", s.ID)
		return err
	}))
	remote, err := d.SessionPRLinkRemote(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/example/recorded.git", remote)
	message := Message{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`}}}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/recorded/pull/42", got.PRLinks[0].URL)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/recorded/pull/42", got.PRLinks[0].URL)
}

func TestToolPRLinksRemoteEvidenceChanges(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "remote-refresh", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`}}}}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", got.PRLinks[0].URL)
	obs = export.WithRecordedGitRemote(obs, "https://github.com/example/other.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://github.com/example/other/pull/42", got.PRLinks[0].URL)
}

func TestToolPRLinksSharedHostState(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "host-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
	message := func(ordinal int, command string) Message {
		return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
	}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, "export GH_HOST=git.example; cd /other")}))
	var state int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT pr_shell_tainted FROM sessions WHERE id=?`, s.ID).Scan(&state))
	assert.Equal(t, 3, state)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 42 -R example/repo; gh pr view 43 -R git.example/example/repo")}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://git.example/example/repo/pull/43", got.PRLinks[0].URL)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, "https://git.example/example/repo/pull/43", got.PRLinks[0].URL)
}

func TestToolPRLinksIncrementalReplacementRemovesLastCall(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "replace-tools", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	message := Message{SessionID: s.ID, Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	replacement := Message{SessionID: s.ID, Ordinal: 0, Role: "assistant", Content: "Done"}
	_, err = d.WriteSessionIncremental(t.Context(), s.ID, []Message{replacement}, IncrementalSessionUpdate{ReplaceFromOrdinal: new(0)})
	require.NoError(t, err)
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestToolPRLinksUsageCleanupAndReplacement(t *testing.T) {
	d := testDB(t)
	s := Session{ID: "cleanup-tools", Project: "project", Machine: "local", Agent: "claude"}
	require.NoError(t, d.UpsertSession(t.Context(), s))
	messages := []Message{
		{SessionID: s.ID, Ordinal: 0, Role: "user", Content: "https://github.com/example/repo/pull/1"},
		{SessionID: s.ID, Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"gh pr view 2 -R example/repo"}`}}},
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages[:1]))
	got, err := d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, 1, got.PRLinks[0].Number)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), s.ID, messages))
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.UpsertSession(t.Context(), s))
	got, err = d.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestCwdChangeRefreshesToolPRLinks(t *testing.T) {
	for _, method := range []string{"session", "path", "upsert", "identity", "batch"} {
		t.Run(method, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "cwd-rederive", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo", FilePath: new("/sessions/session.jsonl")}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: s.ID, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"cd /work/repo; gh pr view 42"}`}}}}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			switch method {
			case "session":
				require.NoError(t, d.UpdateSessionCwd(t.Context(), s.ID, "/work/other"))
			case "path":
				n, err := d.UpdateCwdByAgentPathCount(t.Context(), *s.FilePath, s.Agent, "/work/other")
				require.NoError(t, err)
				require.Equal(t, 1, n)
			case "upsert":
				s.Cwd = "/work/other"
				require.NoError(t, d.UpsertSession(t.Context(), s))
			case "identity":
				s.Cwd = "/work/other"
				require.NoError(t, d.UpsertSessionWithProjectIdentity(t.Context(), s, obs, ""))
			case "batch":
				s.Cwd = "/work/other"
				_, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: s}})
				require.NoError(t, err)
			}
			got, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Empty(t, got.PRLinks)
		})
	}
}

func TestToolPRLinksMissingShellInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
		want  int
	}{
		{"missing", nil, 0}, {"empty", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "missing-shell", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			message := func(ordinal int, input string) Message {
				return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: input}}}
			}
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, `{"command":"echo hi"}`)}))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `UPDATE tool_calls SET input_json=? WHERE session_id=?`, tc.input, s.ID); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET pr_shell_tainted=NULL WHERE id=?`, s.ID)
				return err
			}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, `{"command":"gh pr view 42"}`)}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, tc.want)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
			got, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Len(t, got.PRLinks, tc.want)
		})
	}
}

func TestToolPRLinksAppendRefreshParity(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{
		{"cd /other", 0}, {"export GH_REPO=example/other", 0},
		{"(cd /other)", 1}, {"(export GH_REPO=example/other)", 1},
	} {
		t.Run(tc.command, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "shell-parity", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			message := func(ordinal int, command string) Message {
				return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`}}}
			}
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, tc.command)}))
			codeMode := Message{SessionID: s.ID, Ordinal: 2, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: `text(await tools.clock__curr_time({}));`}}}
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "pwd"), codeMode, message(3, "gh pr view 42"), message(4, "ls -al")}))
			before, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, before.PRLinks, tc.want)
			var oldState, newState sql.NullInt64
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT pr_shell_tainted FROM sessions WHERE id=?`, s.ID).Scan(&oldState))
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT pr_shell_tainted FROM sessions WHERE id=?`, s.ID).Scan(&newState))
			assert.Equal(t, oldState, newState)
			after, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Equal(t, before.PRLinks, after.PRLinks)
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(5, "gh pr view 43")}))
			appended, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, appended.PRLinks, tc.want*2)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
			after, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			assert.Equal(t, appended.PRLinks, after.PRLinks)
		})
	}
}

func TestRemoteChangeInvalidatesAppendedPRLinks(t *testing.T) {
	for _, tc := range []struct{ name, tool, key, command string }{
		{"shared", "Bash", "command", "git remote set-url origin https://github.com/example/other.git"},
		{"independent", "exec_command", "cmd", "git remote set-url origin https://github.com/example/other.git"},
		{"default repo", "exec_command", "cmd", "gh repo set-default example/other"},
		{"function", "exec_command", "cmd", "update() { git remote set-url origin https://github.com/example/other.git; }; update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			s := Session{ID: "remote-change", Project: "project", Machine: "local", Agent: "claude", Cwd: "/work/repo"}
			require.NoError(t, d.UpsertSession(t.Context(), s))
			obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC()}, "https://github.com/example/repo.git")
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), obs))
			message := func(ordinal int, command string) Message {
				return Message{SessionID: s.ID, Ordinal: ordinal, Role: "assistant", ToolCalls: []ToolCall{{ToolName: tc.tool, Category: "Bash", InputJSON: `{"` + tc.key + `":"` + command + `"}`}}}
			}
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(0, tc.command)}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(1, "gh pr view 42")}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{message(2, "gh pr view 43 -R example/other")}))
			got, err := d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/example/other/pull/43", got.PRLinks[0].URL)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return refreshSessionPromptPRLinksTx(t.Context(), tx, s.ID) }))
			got, err = d.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Len(t, got.PRLinks, 1)
			assert.Equal(t, "https://github.com/example/other/pull/43", got.PRLinks[0].URL)
		})
	}
}
