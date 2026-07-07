package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDevinDBPath(t *testing.T) {
	root := t.TempDir()
	cliDir := filepath.Join(root, "cli")
	require.NoError(t, os.MkdirAll(cliDir, 0o755))
	dbPath := filepath.Join(cliDir, devinDBFilename)
	require.NoError(t, os.WriteFile(dbPath, []byte("synthetic"), 0o644))

	assert.Equal(t, dbPath, devinDBPath(root))
	assert.Empty(t, devinDBPath(filepath.Join(root, "missing")))
	assert.Empty(t, devinDBPath(""))

	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Mkdir(dbPath, 0o755))
	assert.Empty(t, devinDBPath(root))
}

func TestListDevinSessionMeta(t *testing.T) {
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: "session-hidden", Title: "Hidden", WorkingDirectory: "/cwd/hidden", Model: "model-hidden", CreatedAtMillis: new(int64(1_700_000_010_000)), LastActivityMillis: new(int64(1_700_000_090_000)), Hidden: true},
		devinSessionRow{ID: "session-fallback", Title: "Fallback title", WorkingDirectory: "/cwd/fallback", Model: "model-fallback", CreatedAtMillis: new(int64(1_700_000_020_000))},
		devinSessionRow{ID: "session-active", Title: "Active title", WorkingDirectory: "/cwd/active", Model: "model-active", CreatedAtMillis: new(int64(1_700_000_030_000)), LastActivityMillis: new(int64(1_700_000_080_000))},
		devinSessionRow{ID: "session-newest", Title: "Newest title", WorkingDirectory: "/cwd/newest", Model: "model-newest", CreatedAtMillis: new(int64(1_700_000_040_000)), LastActivityMillis: new(int64(1_700_000_095_000))},
	)

	metas, err := ListDevinSessionMeta(fixture.DBPath)
	require.NoError(t, err)
	require.Len(t, metas, 3)

	assert.Equal(t, []string{"session-newest", "session-active", "session-fallback"}, devinMetaIDs(metas))

	assert.Equal(t, fixture.sessionVirtualPath("session-newest"), metas[0].VirtualPath)
	assert.Equal(t, "Newest title", metas[0].Title)
	assert.Equal(t, "/cwd/newest", metas[0].CWD)
	assert.Equal(t, "model-newest", metas[0].Model)
	assert.Equal(t, time.UnixMilli(1_700_000_095_000).UTC(), metas[0].UpdatedAt)
	assert.Equal(t, int64(1_700_000_095_000_000_000), metas[0].FileMtime)

	assert.Equal(t, time.UnixMilli(1_700_000_020_000).UTC(), metas[2].UpdatedAt)
	assert.Equal(t, int64(1_700_000_020_000_000_000), metas[2].FileMtime)

	for _, meta := range metas {
		assert.NotEqual(t, "session-hidden", meta.RawSessionID)
	}
}

func TestListDevinSessionMetaAllowsMissingTimestamps(t *testing.T) {
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: "session-missing-times", Title: "Partial row", WorkingDirectory: "/cwd/partial", Model: "model-partial"},
	)

	metas, err := ListDevinSessionMeta(fixture.DBPath)
	require.NoError(t, err)
	require.Len(t, metas, 1)

	assert.Equal(t, "session-missing-times", metas[0].RawSessionID)
	assert.True(t, metas[0].CreatedAt.IsZero())
	assert.True(t, metas[0].UpdatedAt.IsZero())
	assert.Zero(t, metas[0].FileMtime)
}

func TestListDevinSessionMetaMissingDB(t *testing.T) {
	metas, err := ListDevinSessionMeta(filepath.Join(t.TempDir(), "cli", devinDBFilename))
	require.NoError(t, err)
	assert.Nil(t, metas)
}

func TestListDevinSessionMetaMalformedSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), devinDBFilename)
	initDevinTestDB(t, dbPath)
	execDevinTestSQL(t, dbPath, `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			payload TEXT
		);
	`)
	execDevinTestSQL(t, dbPath, `INSERT INTO sessions (id, payload) VALUES ('secret-session', 'top-secret-row-content')`)

	metas, err := ListDevinSessionMeta(dbPath)
	assert.Nil(t, metas)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listing devin sessions")
	assert.NotContains(t, err.Error(), "top-secret-row-content")
	assert.NotContains(t, err.Error(), "secret-session")
}

func TestOpenDevinDBUsesReadOnlyMode(t *testing.T) {
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: "session-readonly", Title: "Read only", WorkingDirectory: "/tmp/readonly", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
	)

	db, err := openDevinDB(fixture.DBPath)
	require.NoError(t, err)
	defer db.Close()

	var journalMode string
	require.NoError(t, db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode))

	_, err = db.Exec(`INSERT INTO sessions (id) VALUES ('write-should-fail')`)
	require.Error(t, err)
	assert.ErrorContains(t, err, "readonly")
	assert.ErrorContains(t, err, "attempt to write")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)

	metas, err := ListDevinSessionMeta(fixture.DBPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"session-readonly"}, devinMetaIDs(metas))
	assert.NotEmpty(t, journalMode)
}

func TestOpenDevinDBWithSpecialCharPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pro#ject %41")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dbPath := filepath.Join(dir, devinDBFilename)

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec("CREATE TABLE sessions (id TEXT); INSERT INTO sessions VALUES ('session-1')")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	db, err := openDevinDB(dbPath)
	require.NoError(t, err)
	defer db.Close()

	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sessions").Scan(&count))
	assert.Equal(t, 1, count)

	_, err = db.Exec("INSERT INTO sessions VALUES ('session-2')")
	require.Error(t, err, "mode=ro must survive special characters in the path")
}

func TestParseDevinSession(t *testing.T) {
	const sessionID = "session-123"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB title wins",
		WorkingDirectory:   "/Users/alice/code/my-app",
		Model:              "db-model",
		CreatedAtMillis:    new(int64(1704103199000)),
		LastActivityMillis: new(int64(1704103265000)),
		WorkspaceJSON:      `{"root_path":"/Users/alice/code/my-app"}`,
		MetadataJSON:       `{"source":"synthetic"}`,
	}, `{
		"title":"Transcript title loses",
		"cwd":"/Users/alice/code/transcript-cwd",
		"created_at":"2024-01-01T10:00:00Z",
		"updated_at":"2024-01-01T10:01:05Z",
		"agent":{"model_name":"devin-1"},
		"final_metrics":{
			"output_tokens":222,
			"input_tokens":100,
			"cache_read_input_tokens":300,
			"cost_usd":99,
			"mystery_tokens":444
		},
		"steps":[
			{"step_id":100,"source":"system","timestamp":"2024-01-01T10:00:00Z","message":"Session booted"},
			{"step_id":"step-1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"Fix the login bug"},
			{"step_id":"step-skip","source":"user","timestamp":"2024-01-01T10:00:02Z","message":[{"type":"text","text":""},{"type":"unknown","value":"ignored"}]},
			{"step_id":"step-3","source":"agent","timestamp":"2024-01-01T10:00:05Z","message":[{"type":"thinking","thinking":"Check auth flow"},{"type":"text","text":"Inspecting files."},{"type":"tool_use","id":"tool-msg","name":"read_file","input":{"file_path":"README.md"}}],"tool_use":[{"id":"tool-top-1","name":"shell_command","input":{"command":"ls -la"}},{"id":"tool-top-2","name":"edit_file","input":{"path":"main.go"}}]},
			{"step_id":"step-4","source":"user","timestamp":"2024-01-01T10:01:00Z","tool_result":[{"tool_use_id":"tool-top-1","content":"file1\nfile2"}]},
			{"step_id":"step-5","source":"agent","timestamp":"2024-01-01T10:01:05Z","message":[{"type":"unknown","value":"ignored"}],"tool_result":[{"tool_use_id":"tool-top-2","content":[{"type":"text","text":"patch applied"}]}]}
		]
	}`)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	assertSessionMeta(t, sess, "devin:"+sessionID, "my_app", AgentDevin)
	require.Len(t, msgs, 5)
	assert.Equal(t, VirtualSourcePath(dbPath, sessionID), sess.File.Path)
	assert.Equal(t, transcriptPath, filepath.Join(filepath.Dir(dbPath), "transcripts", sessionID+".json"))
	assert.Equal(t, "DB title wins", sess.SessionName)
	assert.Equal(t, "/Users/alice/code/my-app", sess.Cwd)
	assert.Equal(t, "Fix the login bug", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
	assertTimestamp(t, sess.StartedAt, time.UnixMilli(1_704_103_199_000).UTC())
	assertTimestamp(t, sess.EndedAt, time.UnixMilli(1_704_103_265_000).UTC())
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 222, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 400, sess.PeakContextTokens)
	hasTotal, hasPeak := sess.AggregateTokenPresence()
	assert.True(t, hasTotal)
	assert.True(t, hasPeak)

	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
	assert.Equal(t, 3, msgs[2].Ordinal)
	assert.Equal(t, 4, msgs[3].Ordinal)
	assert.Equal(t, 5, msgs[4].Ordinal)

	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, "100", msgs[0].SourceUUID)

	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.False(t, msgs[1].IsSystem)
	assert.Equal(t, "Fix the login bug", msgs[1].Content)

	assistant := msgs[2]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.Equal(t, "devin-1", assistant.Model)
	assert.True(t, assistant.HasThinking)
	assert.True(t, assistant.HasToolUse)
	assert.Equal(t, "Check auth flow", assistant.ThinkingText)
	assert.Contains(t, assistant.Content, "[Thinking]\nCheck auth flow\n[/Thinking]")
	assert.Contains(t, assistant.Content, "Inspecting files.")
	assert.Contains(t, assistant.Content, "[Read: README.md]")
	assert.Contains(t, assistant.Content, "[Bash]\n$ ls -la")
	assert.Contains(t, assistant.Content, "[Edit: main.go]")
	require.Len(t, assistant.ToolCalls, 3)
	assert.Equal(t, ParsedToolCall{ToolUseID: "tool-msg", ToolName: "read_file", Category: "Read", InputJSON: `{"file_path":"README.md"}`}, assistant.ToolCalls[0])
	assert.Equal(t, ParsedToolCall{ToolUseID: "tool-top-1", ToolName: "shell_command", Category: "Bash", InputJSON: `{"command":"ls -la"}`}, assistant.ToolCalls[1])
	assert.Equal(t, ParsedToolCall{ToolUseID: "tool-top-2", ToolName: "edit_file", Category: "Edit", InputJSON: `{"path":"main.go"}`}, assistant.ToolCalls[2])

	carrier := msgs[3]
	assert.Equal(t, RoleUser, carrier.Role)
	assert.Empty(t, carrier.Content)
	require.Len(t, carrier.ToolResults, 1)
	assert.Equal(t, ParsedToolResult{ToolUseID: "tool-top-1", ContentLength: len("file1\nfile2"), ContentRaw: `"file1\nfile2"`}, carrier.ToolResults[0])

	standalone := msgs[4]
	assert.Equal(t, RoleTool, standalone.Role)
	assert.Empty(t, standalone.Content)
	require.Len(t, standalone.ToolResults, 1)
	assert.Equal(t, ParsedToolResult{ToolUseID: "tool-top-2", ContentLength: len("patch applied"), ContentRaw: `[{"type":"text","text":"patch applied"}]`}, standalone.ToolResults[0])
}

func TestParseDevinSessionStepMetricsPopulateTokenUsage(t *testing.T) {
	const sessionID = "session-step-metrics"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "Step metrics",
		WorkingDirectory:   "/tmp/devin-pricing",
		Model:              "adaptive",
		CreatedAtMillis:    new(int64(1704103199000)),
		LastActivityMillis: new(int64(1704103265000)),
	}, `{
		"agent":{"model_name":"Adaptive"},
		"final_metrics":{
			"total_completion_tokens":15,
			"total_prompt_tokens":300,
			"total_cached_tokens":20
		},
		"steps":[
			{"step_id":"u1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"price this"},
			{"step_id":"a1","source":"agent","timestamp":"2024-01-01T10:00:02Z","model_name":"Adaptive","extra":{"generation_model":"glm-5-2"},"metrics":{"prompt_tokens":100,"completion_tokens":10,"cached_tokens":20},"message":"first answer"},
			{"step_id":"a2","source":"agent","timestamp":"2024-01-01T10:00:03Z","model_name":"Adaptive","extra":{"generation_model":"kimi-k2-7"},"metrics":{"prompt_tokens":80,"completion_tokens":5,"cached_tokens":0},"message":"second answer"}
		]
	}`)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	first := msgs[1]
	assert.Equal(t, "glm-5-2", first.Model)
	assert.True(t, first.HasContextTokens)
	assert.True(t, first.HasOutputTokens)
	assert.Equal(t, 100, first.ContextTokens)
	assert.Equal(t, 10, first.OutputTokens)
	require.NotEmpty(t, first.TokenUsage)
	assert.Equal(t, int64(80), gjson.GetBytes(first.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(10), gjson.GetBytes(first.TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(20), gjson.GetBytes(first.TokenUsage, "cache_read_input_tokens").Int())

	second := msgs[2]
	assert.Equal(t, "kimi-k2-7", second.Model)
	assert.Equal(t, 80, second.ContextTokens)
	assert.Equal(t, 5, second.OutputTokens)
	require.NotEmpty(t, second.TokenUsage)
	assert.Equal(t, int64(80), gjson.GetBytes(second.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(5), gjson.GetBytes(second.TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(0), gjson.GetBytes(second.TokenUsage, "cache_read_input_tokens").Int())

	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 15, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 100, sess.PeakContextTokens)
}

func TestParseDevinSessionFinalMetricsTotalKeys(t *testing.T) {
	const sessionID = "session-final-total-metrics"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "Final metrics",
		WorkingDirectory:   "/tmp/devin-final-metrics",
		Model:              "glm-5-2",
		CreatedAtMillis:    new(int64(1704103199000)),
		LastActivityMillis: new(int64(1704103265000)),
	}, `{
		"agent":{"model_name":"Adaptive"},
		"final_metrics":{
			"total_completion_tokens":15,
			"total_prompt_tokens":300,
			"total_cached_tokens":20
		},
		"steps":[
			{"step_id":"u1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"summarize"},
			{"step_id":"a1","source":"agent","timestamp":"2024-01-01T10:00:02Z","message":"done"}
		]
	}`)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[1].TokenUsage)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 15, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 300, sess.PeakContextTokens)
}

func TestParseDevinSessionTranscriptFallbacks(t *testing.T) {
	const sessionID = "session-fallbacks"
	worktree := filepath.Join(t.TempDir(), "fallback-app")
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".git"), 0o755))
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Model:              "db-model",
		CreatedAtMillis:    new(int64(0)),
		WorkspaceJSON:      fmt.Sprintf(`[{"root_path":%q}]`, worktree),
		MetadataJSON:       `{"mode":"fallback"}`,
		LastActivityMillis: nil,
	}, fmt.Sprintf(`{
		"agent":{"model_name":""},
		"workspace_dirs":[{"root_path":%q}],
		"final_metrics":{
			"output_tokens":0,
			"context_tokens":0,
			"total_cost_usd":123
		},
		"steps":[
			{"step_id":"a","source":"user","createdAt":"2024-01-01T10:00:01Z","message":"hi from fallback"},
			{"step_id":"b","source":"agent","updatedAt":"2024-01-01T10:00:05Z","message":[{"type":"text","text":"hello"}]}
		]
	}`, worktree))

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "hi from fallback", sess.SessionName)
	assert.Equal(t, "hi from fallback", sess.FirstMessage)
	assert.Equal(t, worktree, sess.Cwd)
	assert.Equal(t, "fallback_app", sess.Project)
	assert.Equal(t, "db-model", msgs[0].Model)
	assert.Equal(t, "db-model", msgs[1].Model)
	assertTimestamp(t, msgs[0].Timestamp, parseTimestamp(tsEarlyS1))
	assertTimestamp(t, msgs[1].Timestamp, parseTimestamp(tsEarlyS5))
	assertTimestamp(t, sess.StartedAt, parseTimestamp(tsEarlyS1))
	assertTimestamp(t, sess.EndedAt, parseTimestamp(tsEarlyS5))
	hasTotal, hasPeak := sess.AggregateTokenPresence()
	assert.False(t, hasTotal)
	assert.False(t, hasPeak)
	assert.False(t, sess.HasTotalOutputTokens)
	assert.False(t, sess.HasPeakContextTokens)
}

func TestParseDevinSessionAllowsMissingMetadataTimestamps(t *testing.T) {
	const sessionID = "session-missing-times"
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Partial metadata",
		WorkingDirectory: "/tmp/partial",
		Model:            "db-model",
	}, `{
		"steps":[
			{"step_id":"step-1","source":"user","message":"hello"}
		]
	}`)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)

	assert.Equal(t, "devin:"+sessionID, sess.ID)
	assert.True(t, sess.StartedAt.IsZero())
	assert.True(t, sess.EndedAt.IsZero())
}

func TestParseDevinSessionEmptyTranscriptUsesDBMetadata(t *testing.T) {
	const sessionID = "session-empty"
	worktree := filepath.Join(t.TempDir(), "db-only-project")
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ".git"), 0o755))
	dbPath, _ := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB only session",
		WorkingDirectory:   worktree,
		Model:              "db-only-model",
		CreatedAtMillis:    new(int64(1704103200000)),
		LastActivityMillis: new(int64(1704103209000)),
	}, `{
		"agent":{"model_name":"transcript-model"},
		"steps":[]
	}`)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, msgs)
	assert.Equal(t, "DB only session", sess.SessionName)
	assert.Equal(t, worktree, sess.Cwd)
	assert.Equal(t, "db_only_project", sess.Project)
	assert.Equal(t, 0, sess.MessageCount)
	assert.Equal(t, 0, sess.UserMessageCount)
	assertTimestamp(t, sess.StartedAt, time.UnixMilli(1_704_103_200_000).UTC())
	assertTimestamp(t, sess.EndedAt, time.UnixMilli(1_704_103_209_000).UTC())
}

func TestParseDevinSessionMissingTranscriptFallsBackToMessageNodes(t *testing.T) {
	const sessionID = "session-db-only"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB only session",
		WorkingDirectory:   "/tmp/db-only-project",
		Model:              "db-only-model",
		CreatedAtMillis:    new(int64(1704103200000)),
		LastActivityMillis: new(int64(1704103209000)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"Recover from SQLite fallback"}`, CreatedAtMillis: 1704103201000},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"I'll use the database transcript fallback.","thinking":"checking message_nodes","tool_calls":[{"id":"call-1","function":{"name":"read_file","arguments":"{\"file_path\":\"main.go\"}"}}]}`, CreatedAtMillis: 1704103205000},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 3, ChatMessage: `{"role":"tool","content":"package main\n","tool_call_id":"call-1"}`, CreatedAtMillis: 1704103207000},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 3)
	assert.Equal(t, "DB only session", sess.SessionName)
	assert.Equal(t, "Recover from SQLite fallback", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, "db-only-model", msgs[0].Model)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Recover from SQLite fallback", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]\nchecking message_nodes\n[/Thinking]")
	assert.Contains(t, msgs[1].Content, "[Read: main.go]")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, ParsedToolCall{ToolUseID: "call-1", ToolName: "read_file", Category: "Read", InputJSON: `{"file_path":"main.go"}`}, msgs[1].ToolCalls[0])
	assert.Equal(t, RoleTool, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, ParsedToolResult{ToolUseID: "call-1", ContentLength: len("package main\n"), ContentRaw: `"package main\n"`}, msgs[2].ToolResults[0])
}

func TestParseDevinSessionTranscriptStillWinsOverMessageNodes(t *testing.T) {
	const sessionID = "session-transcript-wins"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "Transcript wins",
		WorkingDirectory:   "/tmp/transcript-wins",
		Model:              "db-model",
		CreatedAtMillis:    new(int64(1704103200000)),
		LastActivityMillis: new(int64(1704103209000)),
	})
	fixture.writeTranscript(t, sessionID, `{
		"agent":{"model_name":"transcript-model"},
		"steps":[
			{"step_id":"step-1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"Use transcript"},
			{"step_id":"step-2","source":"agent","timestamp":"2024-01-01T10:00:05Z","message":"Transcript answer"}
		]
	}`)
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"Use fallback instead"}`, CreatedAtMillis: 1704103201000},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"Fallback answer"}`, CreatedAtMillis: 1704103205000},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Use transcript", sess.FirstMessage)
	assert.Equal(t, "transcript-model", msgs[0].Model)
	assert.Equal(t, "Use transcript", msgs[0].Content)
	assert.Equal(t, "Transcript answer", msgs[1].Content)
}

func TestParseDevinSessionMissingTranscriptWithoutDBMessagesReturnsRedactedError(t *testing.T) {
	const sessionID = "session-db-only-empty"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB only session",
		WorkingDirectory:   "/tmp/db-only-project",
		Model:              "db-only-model",
		CreatedAtMillis:    new(int64(1704103200000)),
		LastActivityMillis: new(int64(1704103209000)),
	})

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.Nil(t, sess)
	assert.Nil(t, msgs)
	require.Error(t, err)
	assert.ErrorContains(t, err, "missing devin transcript")
	assert.ErrorContains(t, err, devinRedactedTranscriptPath())
	assert.ErrorContains(t, err, devinRedactedSessionID())
	assert.NotContains(t, err.Error(), sessionID)
}

func TestParseDevinSessionFallbackErrorsStayRedacted(t *testing.T) {
	const (
		sessionID      = "secret-session"
		secretSentinel = "oauth-token-SYNTHETIC-SECRET-SENTINEL"
	)
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "DB only session",
		WorkingDirectory:   "/tmp/db-only-project",
		Model:              "db-only-model",
		CreatedAtMillis:    new(int64(1704103200000)),
		LastActivityMillis: new(int64(1704103209000)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"content":"` + secretSentinel, CreatedAtMillis: 1704103201000},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.Nil(t, sess)
	assert.Nil(t, msgs)
	require.Error(t, err)
	assert.ErrorContains(t, err, "missing devin transcript")
	assert.ErrorContains(t, err, devinRedactedTranscriptPath())
	assert.ErrorContains(t, err, devinRedactedSessionID())
	assert.NotContains(t, err.Error(), sessionID)
	assert.NotContains(t, err.Error(), secretSentinel)
}

func TestParseDevinSessionCorruptTranscriptReturnsRedactedError(t *testing.T) {
	const sessionID = "secret-session"
	dbPath, transcriptPath := newDevinSessionFixture(t, devinSessionRow{
		ID:                 sessionID,
		Title:              "Corrupt transcript",
		WorkingDirectory:   "/tmp/app",
		Model:              "db-model",
		CreatedAtMillis:    new(int64(1704103199000)),
		LastActivityMillis: new(int64(1704103265000)),
	}, `{"steps":[]}`)
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"apiKey":"secret-value","steps":[`), 0o644))

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.Nil(t, sess)
	assert.Nil(t, msgs)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid devin transcript")
	assert.ErrorContains(t, err, devinRedactedTranscriptPath())
	assert.ErrorContains(t, err, devinRedactedSessionID())
	assert.NotContains(t, err.Error(), transcriptPath)
	assert.NotContains(t, err.Error(), sessionID)
	assert.NotContains(t, err.Error(), "secret-value")
}

func TestDevinTranscriptPathErrorStaysRedacted(t *testing.T) {
	const sessionID = "secret-session-id"
	secretPath := filepath.Join(t.TempDir(), "cli", "transcripts", sessionID+".json")

	err := newDevinTranscriptError("read", &os.PathError{
		Op:   "open",
		Path: secretPath,
		Err:  os.ErrPermission,
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "read devin transcript")
	assert.ErrorContains(t, err, devinRedactedTranscriptPath())
	assert.ErrorContains(t, err, devinRedactedSessionID())
	assert.ErrorContains(t, err, "permission denied")
	assert.NotContains(t, err.Error(), secretPath)
	assert.NotContains(t, err.Error(), sessionID)
}

func TestParseDevinSessionRedactsCredentialPathsAndTokenLikeValues(t *testing.T) {
	const (
		sessionID      = "session-privacy"
		secretSentinel = "oauth-token-SYNTHETIC-SECRET-SENTINEL"
	)
	fixture := newDevinTestFixture(t,
		devinSessionRow{ID: sessionID, Title: "Privacy", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAtMillis: new(int64(1704103199000)), LastActivityMillis: new(int64(1704103265000))},
	)
	transcriptPath := fixture.writeTranscript(t, sessionID, `{"access_token":"oauth-token-SYNTHETIC-SECRET-SENTINEL","steps":[`)

	secretRoot := filepath.Join(t.TempDir(), secretSentinel, "config", "mcp", "oauth", "devin-root")
	require.NoError(t, os.MkdirAll(filepath.Dir(secretRoot), 0o755))
	require.NoError(t, os.Rename(fixture.Root, secretRoot))
	dbPath := filepath.Join(secretRoot, "cli", devinDBFilename)

	sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
	require.Nil(t, sess)
	assert.Nil(t, msgs)
	assert.ErrorContains(t, err, "invalid devin transcript")
	assert.ErrorContains(t, err, devinRedactedTranscriptPath())
	assert.ErrorContains(t, err, devinRedactedSessionID())
	assertDevinErrorRedacted(t, err,
		secretSentinel,
		"mcp/oauth",
		"config",
		"access_token",
		transcriptPath,
	)
}
