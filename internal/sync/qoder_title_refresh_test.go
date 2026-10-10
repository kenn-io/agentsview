package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

const qoderForkStem = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// qoderForkTranscript is a DAG with a gap large enough to split a fork.
// The session id is the file stem so the application database can name it.
const qoderForkTranscript = `{"type":"user","uuid":"root","timestamp":"2026-06-04T09:47:20.000Z","message":{"role":"user","content":"start"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"split","parentUuid":"root","timestamp":"2026-06-04T09:47:21.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"user","uuid":"main-u1","parentUuid":"split","timestamp":"2026-06-04T09:47:22.000Z","message":{"role":"user","content":"main 1"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"main-a1","parentUuid":"main-u1","timestamp":"2026-06-04T09:47:23.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 1"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"user","uuid":"main-u2","parentUuid":"main-a1","timestamp":"2026-06-04T09:47:24.000Z","message":{"role":"user","content":"main 2"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"main-a2","parentUuid":"main-u2","timestamp":"2026-06-04T09:47:25.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 2"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"user","uuid":"main-u3","parentUuid":"main-a2","timestamp":"2026-06-04T09:47:26.000Z","message":{"role":"user","content":"main 3"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"main-a3","parentUuid":"main-u3","timestamp":"2026-06-04T09:47:27.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 3"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"user","uuid":"main-u4","parentUuid":"main-a3","timestamp":"2026-06-04T09:47:28.000Z","message":{"role":"user","content":"main 4"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"main-a4","parentUuid":"main-u4","timestamp":"2026-06-04T09:47:29.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 4"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"user","uuid":"fork-u1","parentUuid":"split","timestamp":"2026-06-04T09:48:00.000Z","message":{"role":"user","content":"fork path"},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
{"type":"assistant","uuid":"fork-a1","parentUuid":"fork-u1","timestamp":"2026-06-04T09:48:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"fork answer"}]},"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
`

func writeQoderForkFile(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "proj", qoderForkStem+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(qoderForkTranscript), 0o644))
	return path
}

func parseQoderForkFile(t *testing.T, root, path string) (mainID, forkID string) {
	t.Helper()
	provider, ok := parser.NewProvider(parser.AgentQoder, parser.ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 2)
	for _, result := range outcome.Results {
		id := result.Result.Session.ID
		require.Equal(t, path, result.Result.Session.File.Path)
		if result.Result.Session.RelationshipType == parser.RelFork {
			forkID = id
			continue
		}
		mainID = id
	}
	require.Equal(t, "qoder:"+qoderForkStem, mainID)
	require.NotEmpty(t, forkID)
	require.NotEqual(t, mainID, forkID)
	return mainID, forkID
}

func TestSharedTitleRefreshUpdatesForksOfTheSameFile(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	root := filepath.Join(base, ".qoder", "projects")
	path := writeQoderForkFile(t, root)
	mainID, forkID := parseQoderForkFile(t, root, path)

	display := "用户起的名字"
	seedQoderBody := func(id, name, source, machine string) {
		t.Helper()
		require.NoError(t, database.UpsertSession(t.Context(), db.Session{
			ID: id, Agent: string(parser.AgentQoder), Machine: machine,
			SessionName: strPtr(name), DisplayName: &display,
			FilePath: &source, MessageCount: 1, TotalOutputTokens: 17,
			HasTotalOutputTokens: true,
		}))
	}
	seedQoderBody(mainID, "旧标题", path, "devbox")
	seedQoderBody(forkID, "旧标题", path, "devbox")
	otherPath := filepath.Join(root, "proj", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.jsonl")
	seedQoderBody("qoder:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "别的文件", otherPath, "devbox")
	subPath := filepath.Join(root, "proj", qoderForkStem, "subagents", "agent-123.jsonl")
	seedQoderBody("qoder:"+qoderForkStem+":subagent:agent-123", "子代理", subPath, "devbox")
	seedQoderBody("qoder:cccccccc-cccc-4ccc-8ccc-cccccccccccc", "另一台机器", path, "otherhost")
	require.NoError(t, database.RenameSession(t.Context(), mainID, &display))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: mainID, Ordinal: 1, Role: "user", Content: "正文保留",
		ToolCalls: []db.ToolCall{{
			ToolName: "read", ToolUseID: "toolu_1", InputJSON: `{"path":"a"}`,
		}},
	}}))

	renamed := "库里的新标题"
	titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
		map[string]*string{qoderForkStem: &renamed})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	requireStoredTitle(t, database, mainID, renamed)
	requireStoredTitle(t, database, forkID, renamed)
	requireStoredTitle(t, database, "qoder:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "别的文件")
	requireStoredTitle(t, database, "qoder:"+qoderForkStem+":subagent:agent-123", "子代理")
	requireStoredTitle(t, database, "qoder:cccccccc-cccc-4ccc-8ccc-cccccccccccc", "另一台机器")
	assert.Equal(t, []string{"sessions"}, emitter.take())

	stored, err := database.GetSessionFull(t.Context(), mainID)
	require.NoError(t, err)
	require.NotNil(t, stored.DisplayName)
	assert.Equal(t, display, *stored.DisplayName)
	assert.Equal(t, 1, stored.MessageCount)
	assert.Equal(t, 17, stored.TotalOutputTokens)
	messages, err := database.GetAllMessages(t.Context(), mainID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "正文保留", messages[0].Content)
	assert.Equal(t, "read", messages[0].ToolCalls[0].ToolName)

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	assert.Empty(t, emitter.take(), "a second pass with the same title writes nothing")

	blank := ""
	conn, err := sql.Open("sqlite3", titlePath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(),
		`UPDATE chat_sessions SET title = ? WHERE session_id = ?`, blank, qoderForkStem)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	requireStoredTitle(t, database, mainID, "")
	requireStoredTitle(t, database, forkID, "")
	stored, err = database.GetSessionFull(t.Context(), mainID)
	require.NoError(t, err)
	require.NotNil(t, stored.DisplayName)
	assert.Equal(t, display, *stored.DisplayName)
}

func TestSharedTitleRefreshLeavesForkWhenOwnerRowIsGone(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	engine, database := sharedTitleTestEngine(t, emitter)
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	root := filepath.Join(base, ".qoder", "projects")
	path := writeQoderForkFile(t, root)
	_, forkID := parseQoderForkFile(t, root, path)
	seedSharedTitleSessionAt(t, database, forkID, parser.AgentQoder, "分叉旧标题", path)

	renamed := "找不到主行"
	titlePath := writeQoderSessionsDB(t, base, "com.qoder.app.stable",
		map[string]*string{qoderForkStem: &renamed})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{titlePath}))
	requireStoredTitle(t, database, forkID, "分叉旧标题")
	assert.Empty(t, emitter.take())
}

func TestQoderTranscriptTitleReplacesStoredName(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", qoderForkStem+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := "{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-06-04T09:47:27.966Z\"," +
		"\"message\":{\"role\":\"user\",\"content\":\"hello\"},\"sessionId\":\"" + qoderForkStem + "\"}\n" +
		"{\"type\":\"custom-title\",\"customTitle\":\"Transcript title\"}\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	provider, ok := parser.NewProvider(parser.AgentQoder, parser.ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	parsed := outcome.Results[0].Result.Session
	require.True(t, parsed.SessionNamePresent)
	require.Equal(t, "Transcript title", parsed.SessionName)

	database := openTestDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: parsed.ID, Agent: string(parser.AgentQoder),
		SessionName: strPtr("旧标题"), FilePath: &path,
	}))
	converted, err := ingest.ConvertSessionContext(t.Context(), parsed, outcome.Results[0].Result.Messages)
	require.NoError(t, err)
	assert.False(t, converted.PreserveSessionName)
	require.NoError(t, database.UpsertSession(t.Context(), converted))
	requireStoredTitle(t, database, parsed.ID, "Transcript title")

	// A later parse of the same explicit title must not restore the old name.
	again, err := ingest.ConvertSessionContext(t.Context(), parsed, nil)
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), again))
	requireStoredTitle(t, database, parsed.ID, "Transcript title")
}

func TestQoderArchiveRebuildHonorsExplicitTitle(t *testing.T) {
	started := time.Date(2026, 6, 4, 9, 47, 0, 0, time.UTC)
	explicit := parser.ParsedSession{
		ID: "qoder:" + qoderForkStem, Agent: parser.AgentQoder, Machine: "devbox",
		Project: "proj", StartedAt: started, EndedAt: started,
		SessionName: "新标题", SessionNamePresent: true,
		File: parser.FileInfo{Path: "/sources/qoder/session.jsonl", Hash: "h1"},
	}
	silent := explicit
	silent.SessionName = ""
	silent.SessionNamePresent = false
	cleared := explicit
	cleared.SessionName = ""
	cleared.SessionNamePresent = true

	assertArchiveTitle(t, explicit, "新标题")
	assertArchiveTitle(t, silent, "归档旧标题")
	assertArchiveTitle(t, cleared, "")
}

func assertArchiveTitle(t *testing.T, parsed parser.ParsedSession, want string) {
	t.Helper()
	original := openTestDB(t)
	require.NoError(t, original.UpsertSession(t.Context(), db.Session{
		ID: parsed.ID, Agent: string(parser.AgentQoder), Machine: "devbox",
		SessionName: strPtr("归档旧标题"),
		FilePath:    &parsed.File.Path,
	}))
	replacement := openTestDB(t)
	engine := NewEngine(t.Context(), replacement, EngineConfig{Machine: "devbox"})
	engine.archiveStore = original
	t.Cleanup(engine.Close)
	outcome := engine.writeBatchWithOutcome([]pendingWrite{{sess: parsed}}, syncWriteDefault, true)
	require.Equal(t, 1, outcome.writtenSessions)
	require.Zero(t, outcome.failedSessions)
	requireStoredTitle(t, replacement, parsed.ID, want)
}
