package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// cbcSyncHome lays out a CodeBuddy CLI/IDE 5.x home with one project.
func cbcSyncHome(t *testing.T) (home, project string) {
	t.Helper()
	home = t.TempDir()
	project = filepath.Join(home, "projects", "-work-demo")
	require.NoError(t, os.MkdirAll(project, 0o755))
	return home, project
}

func writeCBCSync(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func newCBCEngine(t *testing.T, database *db.DB, roots ...string) *Engine {
	t.Helper()
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodeBuddy: roots},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)
	return engine
}

func cbcDailyTotals(t *testing.T, database *db.DB) db.UsageTotals {
	t.Helper()
	daily, err := database.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	return daily.Totals
}

// A forked transcript repeats the responses it inherited with their native
// identities; usage counts them once across both files.
func TestSyncCodeBuddyForkedTranscriptsCountUsageOnce(t *testing.T) {
	home, project := cbcSyncHome(t)
	shared := []string{
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"start"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"shared reply"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":100,"completion_tokens":10}}}`,
	}
	writeCBCSync(t, filepath.Join(project, "parent.jsonl"), append(shared,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225602000,"content":[{"type":"input_text","text":"parent only"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"parent reply"}],"providerData":{"conversationRequestId":"r2","messageId":"m2","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":200,"completion_tokens":20}}}`,
	)...)
	writeCBCSync(t, filepath.Join(project, "child.jsonl"), append(shared,
		`{"id":"u3","parentId":"a1","type":"message","role":"user","timestamp":1767225604000,"content":[{"type":"input_text","text":"child only"}]}`,
		// A real retry with the same text is a new request and counts again.
		`{"id":"a3","parentId":"u3","type":"message","role":"assistant","timestamp":1767225605000,"content":[{"type":"output_text","text":"parent reply"}],"providerData":{"conversationRequestId":"r3","messageId":"m3","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":300,"completion_tokens":30}}}`,
	)...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	totals := cbcDailyTotals(t, database)
	assert.Equal(t, 60, totals.OutputTokens, "10 once, plus 20 and 30")
	assert.Equal(t, 600, totals.InputTokens, "100 once, plus 200 and 300")
}

// A converted 4.x conversation is read from its CBC transcript, which holds
// later turns; the frozen 4.x manifest does not add a second copy.
func TestSyncCodeBuddyConvertedConversationKeepsOneSession(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"u1","role":"user"},{"id":"a1","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt"}]}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40,"cachedInputTokens":200}},"message":{"content":[{"type":"text","text":"old reply"}]}}`), 0o600))
	writeCBCSync(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)

	sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, filepath.Join(project, "conv1.jsonl"), *sess.FilePath)
	assert.Equal(t, 2, sess.UserMessageCount)
	totals := cbcDailyTotals(t, database)
	assert.Equal(t, 40, totals.OutputTokens, "the 4.x request totals count once")

	// A later sync, after either copy changes, keeps the CBC owner.
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt!"}]}}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	sess, err = database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, filepath.Join(project, "conv1.jsonl"), *sess.FilePath)
	assert.Equal(t, 2, sess.UserMessageCount)
}

// A CodeBuddy transcript that holds no turn of the conversation, such as an
// empty file, a converter's meta line alone, or unreadable text, leaves the
// 4.x copy of the same conversation in place until the transcript gains a
// turn.
func TestSyncCodeBuddyEmptyTranscriptKeepsLegacyCopy(t *testing.T) {
	meta := `{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"meta line only", meta + "\n"},
		{"unreadable line", "not json\n"},
		{"cut-off first line", `{"id":"u1","type":"mess`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"),
				[]byte(`{"messages":[{"id":"u1","role":"user"},{"id":"u2","role":"user"}]}`), 0o600))
			for id, text := range map[string]string{"u1": "first", "u2": "second"} {
				require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", id+".json"),
					[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"`+text+`"}]}}`), 0o600))
			}
			transcript := filepath.Join(project, "conv1.jsonl")
			require.NoError(t, os.WriteFile(transcript, []byte(tc.content), 0o600))
			database := openTestDB(t)
			engine := newCBCEngine(t, database, home, legacyRoot)
			engine.SyncAll(t.Context(), nil)

			sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
			require.NoError(t, err)
			require.NotNil(t, sess, "the 4.x conversation stays visible")
			require.NotNil(t, sess.FilePath)
			assert.Equal(t, filepath.Join(convDir, "index.json"), *sess.FilePath)
			assert.Equal(t, 2, sess.UserMessageCount)

			// Once the transcript holds a turn, it owns the conversation.
			writeCBCSync(t, transcript, meta,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"first"}]}`,
				`{"id":"u2","parentId":"u1","type":"message","role":"user","timestamp":1767225602000,"content":[{"type":"input_text","text":"second"}]}`,
				`{"id":"u3","parentId":"u2","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
			)
			engine.SyncAll(t.Context(), nil)
			sess, err = database.GetSessionFull(t.Context(), "codebuddy:conv1")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.NotNil(t, sess.FilePath)
			assert.Equal(t, transcript, *sess.FilePath)
			assert.Equal(t, 3, sess.UserMessageCount)
		})
	}
}

// Renaming a conversation in the IDE changes only codebuddy.db. A catalog
// event reaches the renamed conversation.
func TestSyncCodeBuddyCatalogRenameUpdatesTitle(t *testing.T) {
	home, project := cbcSyncHome(t)
	writeCBCSync(t, filepath.Join(project, "run-a.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-a","content":[{"type":"input_text","text":"hello"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"hi"}]}`,
	)
	catalog := filepath.Join(home, parser.CodeBuddyCatalogDBName)
	conn, err := sql.Open("sqlite3", catalog)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT);
		INSERT INTO conversations VALUES ('conv-a', 'Before', NULL, 'run-a')`)
	require.NoError(t, err)

	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:run-a")
	require.NoError(t, err)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Before", *sess.SessionName)

	_, err = conn.ExecContext(t.Context(), `UPDATE conversations SET custom_title = 'Renamed in IDE' WHERE id = 'conv-a'`)
	require.NoError(t, err)
	engine.SyncPaths([]string{catalog})
	sess, err = database.GetSessionFull(t.Context(), "codebuddy:run-a")
	require.NoError(t, err)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Renamed in IDE", *sess.SessionName)
}

// A persisted output can be written after its transcript line, or removed by
// CodeBuddy's cleanup later. The archive keeps the full text it once read.
func TestSyncCodeBuddyPersistedOutputLateAndRemoved(t *testing.T) {
	home, project := cbcSyncHome(t)
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	writeCBCSync(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1767225601000,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1767225602000,"name":"Bash","callId":"c1","output":"<persisted-output>\nOutput too large (1KB). Full output saved to: `+filepath.ToSlash(side)+`\n</persisted-output>"}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	resultText := func() string {
		msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
		require.NoError(t, err)
		for _, m := range msgs {
			for _, call := range m.ToolCalls {
				if call.ToolUseID == "c1" {
					return call.ResultContent
				}
			}
		}
		return ""
	}
	assert.Contains(t, resultText(), "persisted-output")

	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
	engine.SyncPaths([]string{side})
	assert.Equal(t, "the full output", resultText())

	require.NoError(t, os.Remove(side))
	engine.SyncPaths([]string{side})
	assert.Equal(t, "the full output", resultText(), "cleanup of the side file keeps the archived text")
}

// cbcToolResult returns the result text of one tool call in a stored session.
func cbcToolResult(t *testing.T, database *db.DB, sessionID, callID string) string {
	t.Helper()
	msgs, err := database.GetMessages(t.Context(), sessionID, 0, 100, true)
	require.NoError(t, err)
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			if call.ToolUseID == callID {
				return call.ResultContent
			}
		}
	}
	return ""
}

// Rewinding a turn after CodeBuddy's cleanup removed one of its saved tool
// outputs moves the call to a new branch. The branch shows the full output
// the main conversation archived before the rewind.
func TestSyncCodeBuddyRewoundTurnKeepsRemovedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*Engine, string)
	}{
		{"full sync", func(e *Engine, _ string) { e.SyncAll(t.Context(), nil) }},
		{"changed path", func(e *Engine, path string) { e.SyncPaths([]string{path}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCodeBuddyRewoundTurnKeepsRemovedOutput(t, tc.sync)
		})
	}
}

func testCodeBuddyRewoundTurnKeepsRemovedOutput(t *testing.T, sync func(*Engine, string)) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
	turn := []string{
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1767225601000,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1767225602000,"name":"Bash","callId":"c1","output":"<persisted-output>\nOutput too large (1KB). Full output saved to: ` + filepath.ToSlash(side) + `\n</persisted-output>"}`,
		`{"id":"a1","parentId":"r1","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"done"}]}`,
	}
	writeCBCSync(t, transcript, turn...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"))

	// CodeBuddy's cleanup removes the saved output, then the user rewinds
	// the turn and sends a new prompt.
	require.NoError(t, os.Remove(side))
	writeCBCSync(t, transcript, append(turn,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1767225604000}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225605000,"content":[{"type":"input_text","text":"again"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225606000,"content":[{"type":"output_text","text":"ok"}]}`,
	)...)
	sync(engine, transcript)

	branch, err := database.GetSessionFull(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "the rewound turn becomes a branch")
	assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1-u1", "c1"),
		"the branch keeps the output the main conversation archived")
	assert.Empty(t, cbcToolResult(t, database, "codebuddy:s1", "c1"),
		"the main conversation no longer holds the rewound call")
}

// Deleting a conversation in CodeBuddy removes its transcript; the archive
// keeps it, and a removed branch keeps its archived copy too.
func TestSyncCodeBuddySourceDeletionKeepsArchive(t *testing.T) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	writeCBCSync(t, transcript,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"old"}]}`,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1767225602000}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"second"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"new"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	for _, id := range []string{"codebuddy:s1", "codebuddy:s1-u1"} {
		sess, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess, id)
	}

	// The producer compacts the file without the old branch.
	writeCBCSync(t, transcript,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"second"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"new"}]}`,
	)
	engine.SyncAll(t.Context(), nil)
	branch, err := database.GetSessionFull(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "a branch missing from the source stays archived")

	require.NoError(t, os.Remove(transcript))
	engine.SyncAll(t.Context(), nil)
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:s1")
	require.NoError(t, err)
	require.NotNil(t, sess, "a deleted transcript keeps its archived session")
	msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
}

// CodeBuddy's cleanup deletes a converted conversation's transcript after its
// retention period. Reconciliation marks the source missing and the archive
// keeps the turns added after conversion; the frozen 4.x copy, even after it
// changes, does not replace them on a sync, a forced sync, or a rebuild.
func TestSyncCodeBuddyDeletedConvertedTranscriptKeepsArchive(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*Engine)
	}{
		{"sync", func(e *Engine) { e.SyncAll(t.Context(), nil) }},
		{"forced sync", func(e *Engine) { e.SyncAllForceParse(t.Context(), nil) }},
		{"full resync", func(e *Engine) {
			st := e.ResyncAll(t.Context(), nil)
			assert.False(t, st.Aborted, "%v", st.Warnings)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"),
				[]byte(`{"messages":[{"id":"u1","role":"user"}]}`), 0o600))
			legacyMessage := filepath.Join(convDir, "messages", "u1.json")
			require.NoError(t, os.WriteFile(legacyMessage,
				[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"first"}]}}`), 0o600))
			transcript := filepath.Join(project, "conv1.jsonl")
			writeCBCSync(t, transcript,
				`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"first"}]}`,
				`{"id":"u2","parentId":"u1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
			)
			// An unrelated live conversation lets a rebuild install.
			writeCBCSync(t, filepath.Join(project, "other.jsonl"),
				`{"id":"o1","type":"message","role":"user","timestamp":1767225800000,"content":[{"type":"input_text","text":"other"}]}`,
			)
			database := openTestDB(t)
			engine := newCBCEngine(t, database, home, legacyRoot)
			engine.SyncAll(t.Context(), nil)
			assertConverted := func(msg string) *db.Session {
				t.Helper()
				sess, err := engine.db.GetSessionFull(t.Context(), "codebuddy:conv1")
				require.NoError(t, err)
				require.NotNil(t, sess, msg)
				require.NotNil(t, sess.FilePath, msg)
				assert.Equal(t, transcript, *sess.FilePath, msg)
				assert.Equal(t, 2, sess.UserMessageCount, msg)
				return sess
			}
			assertConverted("the transcript owns the converted conversation")

			require.NoError(t, os.Remove(transcript))
			require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{home, legacyRoot}, false))
			assertSourceMissingState(t, assertConverted("reconciliation keeps the archived turns"))
			tc.sync(engine)
			assertSourceMissingState(t, assertConverted("the converted turns survive the transcript's deletion"))

			// A later edit to the 4.x copy does not replace them either.
			require.NoError(t, os.WriteFile(legacyMessage,
				[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"first!"}]}}`), 0o600))
			tc.sync(engine)
			assertSourceMissingState(t, assertConverted("a changed 4.x copy does not replace the converted turns"))
		})
	}
}

// The same conversation can sit under two configured CodeBuddy homes, such as
// a backup beside the live home. An older copy, and its agent transcript, do
// not replace the newer turns the archive holds, while the newer copy keeps
// updating the session.
func TestSyncCodeBuddyOlderCopyKeepsNewerTurns(t *testing.T) {
	newerHome, newerProject := cbcSyncHome(t)
	olderHome, olderProject := cbcSyncHome(t)
	turn := func(id, parent, text string, ts int) string {
		parentField := ""
		if parent != "" {
			parentField = `"parentId":"` + parent + `",`
		}
		return `{"id":"` + id + `",` + parentField + `"type":"message","role":"user","timestamp":` +
			strconv.Itoa(ts) + `,"content":[{"type":"input_text","text":"` + text + `"}]}`
	}
	transcripts := func(project string) (main, agent string) {
		return filepath.Join(project, "conv1.jsonl"),
			filepath.Join(project, "conv1", "subagents", "agent-a.jsonl")
	}
	newer, newerAgent := transcripts(newerProject)
	writeCBCSync(t, newer, turn("u1", "", "first", 1767225600000), turn("u2", "u1", "second", 1767225700000))
	writeCBCSync(t, newerAgent, turn("s1", "", "look", 1767225600000), turn("s2", "s1", "look again", 1767225700000))
	database := openTestDB(t)
	engine := newCBCEngine(t, database, newerHome, olderHome)
	engine.SyncAll(t.Context(), nil)
	assertTurns := func(id, path string, turns int, msg string) {
		t.Helper()
		sess, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess, msg)
		require.NotNil(t, sess.FilePath, msg)
		assert.Equal(t, path, *sess.FilePath, msg)
		assert.Equal(t, turns, sess.UserMessageCount, msg)
	}
	assertNewer := func(msg string) {
		t.Helper()
		assertTurns("codebuddy:conv1", newer, 2, msg)
		assertTurns("codebuddy:conv1:subagent:agent-a", newerAgent, 2, msg)
	}
	assertNewer("the only copy owns the conversation")

	older, olderAgent := transcripts(olderProject)
	writeOlder := func(text string) {
		t.Helper()
		writeCBCSync(t, older, turn("u1", "", text, 1767225600000))
		writeCBCSync(t, olderAgent, turn("s1", "", text, 1767225600000))
		stale := time.Now().Add(-time.Hour)
		for _, path := range []string{older, olderAgent} {
			require.NoError(t, os.Chtimes(path, stale, stale))
		}
	}
	writeOlder("first")
	engine.SyncAll(t.Context(), nil)
	assertNewer("a discovered older copy does not replace the newer turns")

	writeOlder("first, edited")
	engine.SyncPaths([]string{older, olderAgent})
	assertNewer("a changed older copy does not replace the newer turns")
	engine.SyncAllForceParse(t.Context(), nil)
	assertNewer("a forced sync keeps the newer turns")

	writeCBCSync(t, newer, turn("u1", "", "first", 1767225600000), turn("u2", "u1", "second", 1767225700000),
		turn("u3", "u2", "third", 1767225800000))
	engine.SyncAll(t.Context(), nil)
	assertTurns("codebuddy:conv1", newer, 3, "the newer copy still updates the session")
}

// Two configured homes can hold the same conversation, such as a backup
// beside the live home. On a first import into an empty archive, the copy
// with the later turns owns the session whichever home lists first, and the
// older copy cannot overwrite it by being written last.
func TestSyncCodeBuddyFirstImportKeepsNewerCopy(t *testing.T) {
	turn := func(id, parent, text string, ts int) string {
		parentField := ""
		if parent != "" {
			parentField = `"parentId":"` + parent + `",`
		}
		return `{"id":"` + id + `",` + parentField + `"type":"message","role":"user","timestamp":` +
			strconv.Itoa(ts) + `,"content":[{"type":"input_text","text":"` + text + `"}]}`
	}
	for _, newerFirst := range []bool{true, false} {
		for _, run := range []struct {
			name string
			sync func(*Engine)
		}{
			{"full sync", func(e *Engine) { e.SyncAll(t.Context(), nil) }},
			{"first import", func(e *Engine) {
				_, _, err := e.RunFirstImport(t.Context(), nil)
				require.NoError(t, err)
			}},
		} {
			t.Run(fmt.Sprintf("%s newer listed first %v", run.name, newerFirst), func(t *testing.T) {
				firstHome, firstProject := cbcSyncHome(t)
				secondHome, secondProject := cbcSyncHome(t)
				newerProject, olderProject := firstProject, secondProject
				if !newerFirst {
					newerProject, olderProject = secondProject, firstProject
				}
				newer := filepath.Join(newerProject, "conv1.jsonl")
				older := filepath.Join(olderProject, "conv1.jsonl")
				writeCBCSync(t, newer, turn("u1", "", "first", 1767225600000),
					turn("u2", "u1", "second", 1767225700000), turn("u3", "u2", "third", 1767225800000))
				writeCBCSync(t, older, turn("u1", "", "first", 1767225600000))
				stale := time.Now().Add(-time.Hour)
				require.NoError(t, os.Chtimes(older, stale, stale))

				database := openTestDB(t)
				run.sync(newCBCEngine(t, database, firstHome, secondHome))
				sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
				require.NoError(t, err)
				require.NotNil(t, sess)
				require.NotNil(t, sess.FilePath)
				assert.Equal(t, newer, *sess.FilePath)
				assert.Equal(t, 3, sess.UserMessageCount)
			})
		}
	}
}
