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
		devinSessionRow{ID: "session-hidden", Title: "Hidden", WorkingDirectory: "/cwd/hidden", Model: "model-hidden", CreatedAt: new(int64(1_700_000_010)), LastActivityAt: new(int64(1_700_000_090)), Hidden: true},
		devinSessionRow{ID: "session-fallback", Title: "Fallback title", WorkingDirectory: "/cwd/fallback", Model: "model-fallback", CreatedAt: new(int64(1_700_000_020))},
		devinSessionRow{ID: "session-active", Title: "Active title", WorkingDirectory: "/cwd/active", Model: "model-active", CreatedAt: new(int64(1_700_000_030)), LastActivityAt: new(int64(1_700_000_080))},
		devinSessionRow{ID: "session-newest", Title: "Newest title", WorkingDirectory: "/cwd/newest", Model: "model-newest", CreatedAt: new(int64(1_700_000_040)), LastActivityAt: new(int64(1_700_000_095))},
	)

	metas, err := ListDevinSessionMeta(fixture.DBPath)
	require.NoError(t, err)
	require.Len(t, metas, 3)

	assert.Equal(t, []string{"session-newest", "session-active", "session-fallback"}, devinMetaIDs(metas))

	assert.Equal(t, fixture.sessionVirtualPath("session-newest"), metas[0].VirtualPath)
	assert.Equal(t, "Newest title", metas[0].Title)
	assert.Equal(t, "/cwd/newest", metas[0].CWD)
	assert.Equal(t, "model-newest", metas[0].Model)
	assert.Equal(t, time.Unix(1_700_000_095, 0).UTC(), metas[0].UpdatedAt)
	assert.Equal(t, int64(1_700_000_095_000_000_000), metas[0].FileMtime)

	assert.Equal(t, time.Unix(1_700_000_020, 0).UTC(), metas[2].UpdatedAt)
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

// Devin stores epoch seconds. A row carrying some other unit (milliseconds, in
// practice) must not be converted anyway: FileMtime is seconds*1e9, which
// overflows int64 above year 2262 and wraps to a far-future nanosecond value.
// devinApplyFileInfoTimes only ever raises Mtime, so a wrapped value can never
// be superseded by a real file mtime and the session stops resyncing.
func TestListDevinSessionMetaRejectsImplausibleTimestamps(t *testing.T) {
	tests := []struct {
		name           string
		lastActivityAt int64
		wantUpdatedAt  time.Time
		wantFileMtime  int64
	}{
		{
			name:           "epoch seconds are accepted",
			lastActivityAt: 1_700_000_095,
			wantUpdatedAt:  time.Unix(1_700_000_095, 0).UTC(),
			wantFileMtime:  1_700_000_095_000_000_000,
		},
		{
			name:           "largest nanosecond-representable second is accepted",
			lastActivityAt: 9_223_372_036,
			wantUpdatedAt:  time.Unix(9_223_372_036, 0).UTC(),
			wantFileMtime:  9_223_372_036_000_000_000,
		},
		{
			name:           "one second past the nanosecond range is rejected",
			lastActivityAt: 9_223_372_037,
		},
		{
			name:           "millisecond value is rejected instead of overflowing",
			lastActivityAt: 1_700_000_095_000,
		},
		{
			name:           "negative value is rejected",
			lastActivityAt: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDevinTestFixture(t,
				devinSessionRow{
					ID:               "session-units",
					Title:            "Units",
					WorkingDirectory: "/cwd/units",
					Model:            "model-units",
					CreatedAt:        new(tc.lastActivityAt),
					LastActivityAt:   new(tc.lastActivityAt),
				},
			)

			metas, err := ListDevinSessionMeta(fixture.DBPath)
			require.NoError(t, err)
			require.Len(t, metas, 1)

			assert.Equal(t, tc.wantUpdatedAt, metas[0].UpdatedAt)
			assert.Equal(t, tc.wantUpdatedAt, metas[0].CreatedAt)
			assert.Equal(t, tc.wantUpdatedAt, metas[0].LastActivity)
			assert.Equal(t, tc.wantFileMtime, metas[0].FileMtime)
		})
	}
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
		devinSessionRow{ID: "session-readonly", Title: "Read only", WorkingDirectory: "/tmp/readonly", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
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
		ID:               sessionID,
		Title:            "DB title wins",
		WorkingDirectory: "/Users/alice/code/my-app",
		Model:            "db-model",
		CreatedAt:        new(int64(1704103199)),
		LastActivityAt:   new(int64(1704103265)),
		WorkspaceJSON:    `{"root_path":"/Users/alice/code/my-app"}`,
		MetadataJSON:     `{"source":"synthetic"}`,
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
	assertTimestamp(t, sess.StartedAt, time.Unix(1_704_103_199, 0).UTC())
	assertTimestamp(t, sess.EndedAt, time.Unix(1_704_103_265, 0).UTC())
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
		ID:               sessionID,
		Title:            "Step metrics",
		WorkingDirectory: "/tmp/devin-pricing",
		Model:            "adaptive",
		CreatedAt:        new(int64(1704103199)),
		LastActivityAt:   new(int64(1704103265)),
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
		ID:               sessionID,
		Title:            "Final metrics",
		WorkingDirectory: "/tmp/devin-final-metrics",
		Model:            "glm-5-2",
		CreatedAt:        new(int64(1704103199)),
		LastActivityAt:   new(int64(1704103265)),
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
		ID:             sessionID,
		Model:          "db-model",
		CreatedAt:      new(int64(0)),
		WorkspaceJSON:  fmt.Sprintf(`[{"root_path":%q}]`, worktree),
		MetadataJSON:   `{"mode":"fallback"}`,
		LastActivityAt: nil,
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
		ID:               sessionID,
		Title:            "DB only session",
		WorkingDirectory: worktree,
		Model:            "db-only-model",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103209)),
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
	assertTimestamp(t, sess.StartedAt, time.Unix(1_704_103_200, 0).UTC())
	assertTimestamp(t, sess.EndedAt, time.Unix(1_704_103_209, 0).UTC())
}

func TestParseDevinSessionMissingTranscriptFallsBackToMessageNodes(t *testing.T) {
	const sessionID = "session-db-only"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "DB only session",
		WorkingDirectory: "/tmp/db-only-project",
		Model:            "db-only-model",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103209)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"Recover from SQLite fallback"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"I'll use the database transcript fallback.","thinking":"checking message_nodes","tool_calls":[{"id":"call-1","function":{"name":"read_file","arguments":"{\"file_path\":\"main.go\"}"}}]}`, CreatedAt: 1704103205},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 3, ChatMessage: `{"role":"tool","content":"package main\n","tool_call_id":"call-1"}`, CreatedAt: 1704103207},
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

func TestParseDevinSessionSupportsSessionsTableWithoutMainChainID(t *testing.T) {
	tests := []struct {
		name       string
		transcript string
		wantFirst  string
	}{
		{
			name: "exported transcript",
			transcript: `{
				"steps":[
					{"source":"user","timestamp":"2024-01-01T10:00:01Z","message":"legacy transcript"},
					{"source":"agent","timestamp":"2024-01-01T10:00:02Z","message":"answer"}
				]
			}`,
			wantFirst: "legacy transcript",
		},
		{
			name:      "message nodes fallback",
			wantFirst: "legacy message nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const sessionID = "legacy-session"
			root := t.TempDir()
			cliDir := filepath.Join(root, "cli")
			transcriptsDir := filepath.Join(cliDir, "transcripts")
			require.NoError(t, os.MkdirAll(transcriptsDir, 0o755))
			dbPath := filepath.Join(cliDir, devinDBFilename)

			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = db.Exec(`
				CREATE TABLE sessions (
					id TEXT PRIMARY KEY,
					title TEXT,
					working_directory TEXT,
					model TEXT,
					created_at INTEGER,
					last_activity_at INTEGER,
					hidden INTEGER NOT NULL DEFAULT 0
				);
				CREATE TABLE message_nodes (
					row_id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL,
					node_id INTEGER NOT NULL,
					parent_node_id INTEGER,
					chat_message TEXT NOT NULL,
					created_at INTEGER NOT NULL
				);
				INSERT INTO sessions (
					id, title, working_directory, model,
					created_at, last_activity_at
				) VALUES (
					'legacy-session', 'Legacy session', '/tmp/legacy',
					'legacy-model', 1704103200, 1704103202
				);
			`)
			require.NoError(t, err)
			if tt.transcript == "" {
				_, err = db.Exec(`
					INSERT INTO message_nodes (
						session_id, node_id, chat_message, created_at
					) VALUES
						('legacy-session', 1, '{"role":"user","content":"legacy message nodes"}', 1704103201),
						('legacy-session', 2, '{"role":"assistant","content":"answer"}', 1704103202)
				`)
				require.NoError(t, err)
			} else {
				require.NoError(t, os.WriteFile(
					filepath.Join(transcriptsDir, sessionID+".json"),
					[]byte(tt.transcript), 0o644,
				))
			}
			require.NoError(t, db.Close())

			sess, msgs, err := parseDevinSession(dbPath, sessionID, "local")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 2)
			assert.Equal(t, tt.wantFirst, sess.FirstMessage)
		})
	}
}

func TestParseDevinSessionMessageNodesSumTokenMetricsAlongMainChain(t *testing.T) {
	const sessionID = "session-node-metrics"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Node metrics",
		WorkingDirectory: "/tmp/node-metrics",
		Model:            "claude-opus-4-8-medium",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103210)),
		// Main chain leaf is node 4; the chain is 1 -> 2 -> 4. Node 3 is
		// an abandoned retry branching off node 1 and must not be counted.
		MainChainID: new(int64(4)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"question"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"first answer","metadata":{"metrics":{"input_tokens":10,"output_tokens":5,"cache_read_tokens":100,"cache_creation_tokens":20}}}`, CreatedAt: 1704103205},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 3, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"abandoned retry","metadata":{"metrics":{"input_tokens":999,"output_tokens":999,"cache_read_tokens":999,"cache_creation_tokens":999}}}`, CreatedAt: 1704103206},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 4, ParentNodeID: new(int64(2)), ChatMessage: `{"role":"assistant","content":"second answer","metadata":{"metrics":{"input_tokens":3,"output_tokens":7,"cache_read_tokens":null,"cache_creation_tokens":50}}}`, CreatedAt: 1704103207},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Only the main chain (nodes 1, 2, 4) is reconstructed; the retry
	// branch (node 3) is dropped entirely.
	require.Len(t, msgs, 3)
	assert.Equal(t, "question", msgs[0].Content)
	assert.Equal(t, "first answer", msgs[1].Content)
	assert.Equal(t, "second answer", msgs[2].Content)

	first := msgs[1]
	assert.True(t, first.HasContextTokens)
	assert.True(t, first.HasOutputTokens)
	assert.Equal(t, 130, first.ContextTokens) // 10 + 100 + 20
	assert.Equal(t, 5, first.OutputTokens)
	require.NotEmpty(t, first.TokenUsage)
	assert.Equal(t, int64(10), gjson.GetBytes(first.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(5), gjson.GetBytes(first.TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(100), gjson.GetBytes(first.TokenUsage, "cache_read_input_tokens").Int())
	assert.Equal(t, int64(20), gjson.GetBytes(first.TokenUsage, "cache_creation_input_tokens").Int())

	// A null cache_read_tokens is treated as absent, not zero-present, so
	// the key is omitted from the priced payload.
	second := msgs[2]
	assert.Equal(t, 53, second.ContextTokens) // 3 + 0 + 50
	assert.Equal(t, 7, second.OutputTokens)
	require.NotEmpty(t, second.TokenUsage)
	assert.Equal(t, int64(3), gjson.GetBytes(second.TokenUsage, "input_tokens").Int())
	assert.False(t, gjson.GetBytes(second.TokenUsage, "cache_read_input_tokens").Exists())
	assert.Equal(t, int64(50), gjson.GetBytes(second.TokenUsage, "cache_creation_input_tokens").Int())

	// Session totals sum output along the main chain and peak the context;
	// node 3's inflated counters never contribute.
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 12, sess.TotalOutputTokens) // 5 + 7
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 130, sess.PeakContextTokens)
	hasTotal, hasPeak := sess.AggregateTokenPresence()
	assert.True(t, hasTotal)
	assert.True(t, hasPeak)
}

func TestParseDevinSessionMessageNodesPreferGenerationModel(t *testing.T) {
	const sessionID = "session-node-genmodel"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Generation model",
		WorkingDirectory: "/tmp/node-genmodel",
		// Coarse/empty session alias; the concrete model lives per message.
		Model:          "adaptive",
		CreatedAt:      new(int64(1704103200)),
		LastActivityAt: new(int64(1704103210)),
		MainChainID:    new(int64(2)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"hi"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"answer","metadata":{"generation_model":"claude-opus-4-6-thinking","metrics":{"input_tokens":4,"output_tokens":6}}}`, CreatedAt: 1704103205},
	)

	_, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	// User node has no generation_model, so it falls back to the session model.
	assert.Equal(t, "adaptive", msgs[0].Model)
	// Assistant node's per-message generation_model wins over the session alias.
	assert.Equal(t, "claude-opus-4-6-thinking", msgs[1].Model)
}

func TestParseDevinSessionMessageNodesDanglingMainChainFallsBackToAllNodes(t *testing.T) {
	const sessionID = "session-node-dangling"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Dangling chain",
		WorkingDirectory: "/tmp/node-dangling",
		Model:            "claude-opus-4-8-medium",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103210)),
		// Points at a node that does not exist; parsing must fall back to
		// every node rather than dropping the session.
		MainChainID: new(int64(9999)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"hi"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"answer","metadata":{"metrics":{"input_tokens":4,"output_tokens":6}}}`, CreatedAt: 1704103205},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 6, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 4, sess.PeakContextTokens)
}

func TestParseDevinSessionMessageNodesMissingParentFallsBackToAllNodes(t *testing.T) {
	const sessionID = "session-node-missing-parent"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Missing parent",
		WorkingDirectory: "/tmp/node-missing-parent",
		Model:            "claude-opus-4-8-medium",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103210)),
		MainChainID:      new(int64(3)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"hi"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ParentNodeID: new(int64(1)), ChatMessage: `{"role":"assistant","content":"earlier answer","metadata":{"metrics":{"input_tokens":4,"output_tokens":6}}}`, CreatedAt: 1704103205},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 3, ParentNodeID: new(int64(9999)), ChatMessage: `{"role":"assistant","content":"orphaned leaf","metadata":{"metrics":{"input_tokens":7,"output_tokens":8}}}`, CreatedAt: 1704103207},
	)

	sess, msgs, err := parseDevinSession(fixture.DBPath, sessionID, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "hi", msgs[0].Content)
	assert.Equal(t, "earlier answer", msgs[1].Content)
	assert.Equal(t, "orphaned leaf", msgs[2].Content)
	assert.Equal(t, 14, sess.TotalOutputTokens)
	assert.Equal(t, 7, sess.PeakContextTokens)
}

func TestParseDevinSessionTranscriptStillWinsOverMessageNodes(t *testing.T) {
	const sessionID = "session-transcript-wins"
	fixture := newDevinTestFixture(t, devinSessionRow{
		ID:               sessionID,
		Title:            "Transcript wins",
		WorkingDirectory: "/tmp/transcript-wins",
		Model:            "db-model",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103209)),
	})
	fixture.writeTranscript(t, sessionID, `{
		"agent":{"model_name":"transcript-model"},
		"steps":[
			{"step_id":"step-1","source":"user","timestamp":"2024-01-01T10:00:01Z","message":"Use transcript"},
			{"step_id":"step-2","source":"agent","timestamp":"2024-01-01T10:00:05Z","message":"Transcript answer"}
		]
	}`)
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"role":"user","content":"Use fallback instead"}`, CreatedAt: 1704103201},
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 2, ChatMessage: `{"role":"assistant","content":"Fallback answer"}`, CreatedAt: 1704103205},
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
		ID:               sessionID,
		Title:            "DB only session",
		WorkingDirectory: "/tmp/db-only-project",
		Model:            "db-only-model",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103209)),
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
		ID:               sessionID,
		Title:            "DB only session",
		WorkingDirectory: "/tmp/db-only-project",
		Model:            "db-only-model",
		CreatedAt:        new(int64(1704103200)),
		LastActivityAt:   new(int64(1704103209)),
	})
	fixture.insertMessageNodes(t,
		devinSyntheticMessageNodeRow{SessionID: sessionID, NodeID: 1, ChatMessage: `{"content":"` + secretSentinel, CreatedAt: 1704103201},
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
		ID:               sessionID,
		Title:            "Corrupt transcript",
		WorkingDirectory: "/tmp/app",
		Model:            "db-model",
		CreatedAt:        new(int64(1704103199)),
		LastActivityAt:   new(int64(1704103265)),
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
		devinSessionRow{ID: sessionID, Title: "Privacy", WorkingDirectory: "/tmp/app", Model: "db-model", CreatedAt: new(int64(1704103199)), LastActivityAt: new(int64(1704103265))},
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
