package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

const crushTestSchema = `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		parent_session_id TEXT,
		title TEXT NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL DEFAULT 0.0,
		updated_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		summary_message_id TEXT,
		todos TEXT
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		parts TEXT NOT NULL DEFAULT '[]',
		model TEXT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		finished_at INTEGER,
		provider TEXT,
		is_summary_message INTEGER DEFAULT 0 NOT NULL
	);
	CREATE INDEX idx_messages_session_id ON messages (session_id);
`

type crushTestFixture struct {
	projectDir string
	dataDir    string
	dbPath     string
	database   *sql.DB
}

func newCrushTestFixture(t *testing.T) *crushTestFixture {
	t.Helper()
	projectDir := t.TempDir()
	dataDir := filepath.Join(projectDir, ".crush")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, CrushDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(crushTestSchema)
	require.NoError(t, err)
	return &crushTestFixture{
		projectDir: projectDir,
		dataDir:    dataDir,
		dbPath:     dbPath,
		database:   database,
	}
}

func (f *crushTestFixture) insertSession(
	t *testing.T, id, title, parentID string,
	created, updated int64, prompt, completion int64, cost float64,
) {
	t.Helper()
	_, err := f.database.Exec(`
		INSERT INTO sessions (
			id, parent_session_id, title, created_at, updated_at,
			prompt_tokens, completion_tokens, cost
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, nullableCrushTestString(parentID), title, created, updated,
		prompt, completion, cost)
	require.NoError(t, err)
}

func (f *crushTestFixture) insertMessage(
	t *testing.T, id, sessionID, role, parts string, created int64,
	model, provider string,
) {
	t.Helper()
	_, err := f.database.Exec(`
		INSERT INTO messages (
			id, session_id, role, parts, model, created_at, updated_at,
			provider
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, sessionID, role, parts, nullableCrushTestString(model),
		created, created, nullableCrushTestString(provider))
	require.NoError(t, err)
}

func nullableCrushTestString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestCrushDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentCrush)
	require.True(t, ok)
	assert.Equal(t, []string{".local/share/crush", "AppData/Local/crush"}, def.DefaultDirs)
	assert.Equal(t, "CRUSH_DIR", def.EnvVar)
	assert.Equal(t, "crush_dirs", def.ConfigKey)
	assert.Equal(t, "crush:", def.IDPrefix)
	assert.False(t, def.FileBased)
}

func TestCrushProviderParsesTranscriptToolsAndUsage(t *testing.T) {
	fixture := newCrushTestFixture(t)
	// Unix seconds, 2026 era: 1789093626 is 2026-09-10 UTC.
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-1", "Review auth flow", "",
		created, created+120, 43_922, 185, 0.0126)
	fixture.insertMessage(t, "msg-user", "sess-1", "user", `[
		{"type":"text","data":{"text":"Review the auth flow please."}},
		{"type":"finish","data":{"reason":"stop","time":0}}
	]`, created, "", "")
	fixture.insertMessage(t, "msg-assistant-1", "sess-1", "assistant", `[
		{"type":"reasoning","data":{"thinking":"Inspect auth.go first.","started_at":1789093629,"finished_at":1789093629}},
		{"type":"reasoning","data":{"thinking":"Then check the middleware.","started_at":1789093630,"finished_at":1789093630}},
		{"type":"tool_call","data":{"id":"chatcmpl-tool-1","name":"view","input":"{\"file_path\":\"auth.go\"}","finished":true,"provider_executed":false}},
		{"type":"tool_call","data":{"id":"chatcmpl-tool-2","name":"bash","input":"{\"command\":\"go test\"}","finished":true,"provider_executed":false}},
		{"type":"finish","data":{"reason":"tool_use","time":1789093630}}
	]`, created+3, "glm-5.3-flash", "hyper")
	fixture.insertMessage(t, "msg-tool-1", "sess-1", "tool", `[
		{"type":"tool_result","data":{"tool_call_id":"chatcmpl-tool-1","name":"view","content":"package auth\n"}}
	]`, created+4, "", "")
	fixture.insertMessage(t, "msg-tool-2", "sess-1", "tool", `[
		{"type":"tool_result","data":{"tool_call_id":"chatcmpl-tool-2","name":"bash","content":""}}
	]`, created+5, "", "")
	fixture.insertMessage(t, "msg-assistant-2", "sess-1", "assistant", `[
		{"type":"text","data":{"text":"The auth flow looks correct."}},
		{"type":"finish","data":{"reason":"stop","time":1789093740}}
	]`, created+30, "glm-5.3-flash", "hyper")

	session, messages, err := parseCrushSession(fixture.dbPath, "sess-1", "workstation")
	require.NoError(t, err)

	require.NotNil(t, session)
	assert.Equal(t, "crush:sess-1", session.ID)
	assert.Equal(t, AgentCrush, session.Agent)
	assert.Equal(t, "Review auth flow", session.SessionName)
	assert.Equal(t, "crush-sqlite-v1", session.SourceVersion)
	assert.Equal(t, "workstation", session.Machine)
	assert.Equal(t, "Review the auth flow please.", session.FirstMessage)
	// Timestamps are Unix seconds; a ms interpretation would land in 1970.
	assert.Equal(t, time.Unix(created, 0).UTC(), session.StartedAt)
	assert.Equal(t, time.Unix(created+120, 0).UTC(), session.EndedAt)
	assert.Equal(t, 5, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	// Project resolves from the <project>/.crush store location.
	assert.Equal(t, filepath.Base(fixture.projectDir), session.Project)
	assert.Equal(t, fixture.projectDir, session.Cwd)

	require.Len(t, messages, 5)
	for i, message := range messages {
		assert.Equal(t, i, message.Ordinal)
	}

	user := messages[0]
	assert.Equal(t, RoleUser, user.Role)
	assert.False(t, user.IsSystem)
	assert.Equal(t, "Review the auth flow please.", user.Content)
	assert.Empty(t, user.StopReason, "finish parts are internal metadata")

	assistant := messages[1]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.Equal(t, "glm-5.3-flash", assistant.Model)
	assert.Equal(t, "hyper", assistant.ProviderID)
	assert.True(t, assistant.HasThinking)
	// Multiple reasoning parts accumulate instead of overwriting.
	assert.Contains(t, assistant.ThinkingText, "Inspect auth.go first.")
	assert.Contains(t, assistant.ThinkingText, "Then check the middleware.")
	assert.Equal(t, "tool_use", assistant.StopReason)
	require.Len(t, assistant.ToolCalls, 2)
	assert.True(t, assistant.HasToolUse)

	firstCall := assistant.ToolCalls[0]
	assert.Equal(t, "crush:chatcmpl-tool-1", firstCall.ToolUseID)
	assert.Equal(t, "view", firstCall.ToolName)
	assert.Equal(t, "Read", firstCall.Category)
	assert.JSONEq(t, `{"file_path":"auth.go"}`, firstCall.InputJSON)
	secondCall := assistant.ToolCalls[1]
	assert.Equal(t, "crush:chatcmpl-tool-2", secondCall.ToolUseID)
	assert.Equal(t, "bash", secondCall.ToolName)
	assert.Equal(t, "Bash", secondCall.Category)

	toolResult := messages[2]
	assert.Equal(t, RoleUser, toolResult.Role)
	assert.True(t, toolResult.IsSystem,
		"role='tool' rows are system messages, not human turns")
	require.Len(t, toolResult.ToolResults, 1)
	assert.Equal(t, "crush:chatcmpl-tool-1", toolResult.ToolResults[0].ToolUseID)

	// An empty-but-present result still completes its call.
	emptyResult := messages[3]
	require.Len(t, emptyResult.ToolResults, 1)
	assert.Equal(t, "crush:chatcmpl-tool-2", emptyResult.ToolResults[0].ToolUseID)

	final := messages[4]
	assert.Equal(t, RoleAssistant, final.Role)
	assert.Equal(t, "The auth flow looks correct.", final.Content)
	assert.Equal(t, "stop", final.StopReason)

	// Exactly one aggregate usage event; session totals are the sole
	// accounting source and per-message tokens are absent.
	require.Len(t, session.UsageEvents, 1)
	event := session.UsageEvents[0]
	assert.Equal(t, "crush:sess-1", event.SessionID)
	assert.Equal(t, "glm-5.3-flash", event.Model)
	assert.Equal(t, "hyper", event.ProviderID)
	assert.Equal(t, 43_922, event.InputTokens)
	assert.Equal(t, 185, event.OutputTokens)
	require.NotNil(t, event.Cost)
	assert.Equal(t, money.Money{Microdollars: 12_600}, *event.Cost)

	assert.True(t, session.HasTotalOutputTokens)
	assert.Equal(t, 185, session.TotalOutputTokens)
	assert.True(t, session.HasPeakContextTokens)
	assert.Equal(t, 43_922, session.PeakContextTokens)

	assert.False(t, hasOrphanedToolCall(messages),
		"every tool call has a paired result message")
}

func TestCrushTimestampSecondsNotMilliseconds(t *testing.T) {
	assert.True(t, crushUnixTimestamp(1_789_093_626).After(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		"second-scale timestamps must decode to the correct era")
	assert.True(t, crushUnixTimestamp(1_789_093_626_000).After(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		"millisecond-scale timestamps must decode forward-compatible")
	assert.True(t, crushUnixTimestamp(0).IsZero())
	assert.False(t, crushUnixTimestamp(10_000_000_000).After(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		"the exact boundary must decode as milliseconds (1970-era)")
	assert.True(t, crushUnixTimestamp(9_999_999_999).After(
		time.Date(2286, 1, 1, 0, 0, 0, 0, time.UTC)),
		"one below the boundary must decode as seconds")
}

func TestCrushZeroCostWithTokensStillEmitsEvent(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-free", "Free usage", "",
		created, created, 1_000, 10, 0.0)
	session, _, err := parseCrushSession(fixture.dbPath, "sess-free", "m")
	require.NoError(t, err)
	require.Len(t, session.UsageEvents, 1)
	assert.Equal(t, 1_000, session.UsageEvents[0].InputTokens)
	require.NotNil(t, session.UsageEvents[0].Cost)
	assert.Equal(t, money.Money{}, *session.UsageEvents[0].Cost)
}

func TestCrushNoUsageEventWithoutAccountingData(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-empty", "No usage", "",
		created, created, 0, 0, 0.0)
	session, _, err := parseCrushSession(fixture.dbPath, "sess-empty", "m")
	require.NoError(t, err)
	assert.Empty(t, session.UsageEvents)
}

func TestCrushParentSessionRelationship(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "parent", "Parent", "", created, created, 0, 0, 0)
	fixture.insertSession(t, "child", "Child", "parent", created, created, 0, 0, 0)
	session, _, err := parseCrushSession(fixture.dbPath, "child", "m")
	require.NoError(t, err)
	assert.Equal(t, "crush:parent", session.ParentSessionID)
	assert.Equal(t, RelSubagent, session.RelationshipType)
}

func TestCrushMalformedPartsFailSession(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-bad", "Broken parts", "",
		created, created, 0, 0, 0)
	fixture.insertMessage(t, "msg-bad", "sess-bad", "user", `not-json`,
		created, "", "")
	_, _, err := parseCrushSession(fixture.dbPath, "sess-bad", "m")
	require.Error(t, err)
}

func TestCrushUnknownRoleIsSkipped(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-roles", "Roles", "",
		created, created, 0, 0, 0)
	fixture.insertMessage(t, "msg-u", "sess-roles", "user", `[
		{"type":"text","data":{"text":"hello"}}
	]`, created, "", "")
	fixture.insertMessage(t, "msg-x", "sess-roles", "internal", `[
		{"type":"text","data":{"text":"hidden"}}
	]`, created, "", "")
	_, messages, err := parseCrushSession(fixture.dbPath, "sess-roles", "m")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, RoleUser, messages[0].Role)
}

func TestCrushSubagentToolCallLinking(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	// Crush names subagent sessions "<own-uuid>$$<spawning-tool-call-id>".
	fixture.insertSession(t, "parent", "Parent", "", created, created, 0, 0, 0)
	fixture.insertSession(t,
		"child-uuid$$chatcmpl-tool-fetch", "Child", "parent",
		created, created, 0, 0, 0)
	fixture.insertMessage(t, "msg-p", "parent", "assistant", `[
		{"type":"tool_call","data":{"id":"chatcmpl-tool-fetch","name":"agentic_fetch","input":"{\"url\":\"https://example.test\"}","finished":true,"provider_executed":false}}
	]`, created, "glm-5.3-flash", "")

	_, messages, err := parseCrushSession(fixture.dbPath, "parent", "m")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "crush:child-uuid$$chatcmpl-tool-fetch",
		messages[0].ToolCalls[0].SubagentSessionID,
		"the spawning tool call must link to the child session")
}

func TestCrushProviderDiscoveryAndRoots(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-1", "Discovery", "",
		created, created, 10, 5, 0.0)

	// A Crush data dir with projects.json expands to per-project roots.
	registryDir := t.TempDir()
	registry := `{"projects":[{"path":"` + fixture.projectDir +
		`","data_dir":"` + fixture.dataDir + `"}]}`
	require.NoError(t, os.WriteFile(
		filepath.Join(registryDir, CrushProjectsFileName),
		[]byte(registry), 0o600,
	))
	roots := normalizeCrushRoots([]string{registryDir})
	require.Equal(t, []string{fixture.dataDir}, roots)

	metas := make([]dbBackedSessionMeta, 0)
	require.NoError(t, forEachCrushSessionMeta(
		context.Background(), crushDBPath(roots[0]),
		func(meta dbBackedSessionMeta) error {
			metas = append(metas, meta)
			return nil
		},
	))
	require.Len(t, metas, 1)
	assert.Equal(t, "sess-1", metas[0].SessionID)
	assert.Equal(t, VirtualSourcePath(fixture.dbPath, "sess-1"), metas[0].VirtualPath)
	assert.Positive(t, metas[0].FileMtime)

	meta, found, err := crushSessionMeta(context.Background(), fixture.dbPath, "missing")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, meta.SessionID)

	// A root pointing directly at a .crush data dir is kept as-is.
	assert.Equal(t,
		[]string{fixture.dataDir},
		normalizeCrushRoots([]string{fixture.dataDir}),
	)
	// A root pointing at the db file resolves to its directory.
	assert.Equal(t,
		[]string{fixture.dataDir},
		normalizeCrushRoots([]string{fixture.dbPath}),
	)
	// An unreadable registry leaves the root untouched rather than
	// failing discovery.
	assert.Equal(t,
		[]string{registryDir + "-missing"},
		normalizeCrushRoots([]string{registryDir + "-missing"}),
	)
}

func TestCrushSchemaValidationRejectsGooseStores(t *testing.T) {
	fixture := newCrushTestFixture(t)
	// Drop the parts column marker: a messages table without it is not
	// a Crush store (e.g. goose's vendored migrations).
	_, err := fixture.database.Exec(`
		CREATE TABLE messages_like_goose (
			id TEXT PRIMARY KEY, session_id TEXT, role TEXT,
			content_json TEXT, created_at INTEGER
		)
	`)
	require.NoError(t, err)
	_, err = fixture.database.Exec(`DROP TABLE messages`)
	require.NoError(t, err)
	_, err = fixture.database.Exec(`ALTER TABLE messages_like_goose RENAME TO messages`)
	require.NoError(t, err)
	require.Error(t, validateCrushSchema(
		context.Background(), fixture.database,
	), "a messages table without parts must not be treated as Crush")
}

func TestCrushRegistryCompletenessIncludesCrush(t *testing.T) {
	def, ok := AgentByType(AgentCrush)
	require.True(t, ok)
	assert.Equal(t, "Charm Crush", def.DisplayName)
	assert.True(t, def.PeriodicReconcile)
	assert.True(t, def.Usage.NoPerMessageTokenData)
}

func TestCrushProviderParseCarriesUsageEvents(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-use", "Usage", "",
		created, created, 43_922, 185, 0.0126)

	factory := newCrushProviderFactory(AgentDef{Type: AgentCrush, IDPrefix: "crush:"})
	provider := factory.NewProvider(ProviderConfig{
		Roots: []string{fixture.dataDir},
	})
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:    AgentCrush,
			Key:         VirtualSourcePath(fixture.dbPath, "sess-use"),
			DisplayPath: VirtualSourcePath(fixture.dbPath, "sess-use"),
			Opaque: dbBackedSource{
				Root: fixture.dataDir, DBPath: fixture.dbPath, SessionID: "sess-use",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	require.Len(t, outcome.Results[0].Result.UsageEvents, 1,
		"the sync engine writes usage rows only from ParseResult.UsageEvents")
	event := outcome.Results[0].Result.UsageEvents[0]
	assert.Equal(t, "crush:sess-use", event.SessionID)
	assert.Equal(t, 43_922, event.InputTokens)
	assert.Equal(t, 185, event.OutputTokens)
	require.NotNil(t, event.Cost)
}

func TestCrushSummaryMessageIsCompactBoundary(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-summary", "Compacted", "",
		created, created, 0, 0, 0)
	fixture.insertMessage(t, "msg-sum", "sess-summary", "assistant", `[
		{"type":"text","data":{"text":"Summary of the conversation so far."}}
	]`, created, "glm-5.3-flash", "")
	// Mark the row as a condensed summary.
	_, err := fixture.database.Exec(
		`UPDATE messages SET is_summary_message = 1 WHERE id = 'msg-sum'`,
	)
	require.NoError(t, err)

	session, messages, err := parseCrushSession(fixture.dbPath, "sess-summary", "m")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	message := messages[0]
	assert.Equal(t, RoleSystem, message.Role)
	assert.True(t, message.IsSystem)
	assert.True(t, message.IsCompactBoundary)
	assert.Equal(t, "Summary of the conversation so far.", message.Content)
	assert.Empty(t, message.Model,
		"summary rows must not attribute the original row's model")
	assert.Equal(t, 0, session.UserMessageCount)
}

func TestCrushChangedSessionIDsAreBoundedToNewRows(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-a", "A", "", created, created, 0, 0, 0)
	fixture.insertSession(t, "sess-b", "B", "", created, created, 0, 0, 0)
	tracker := newCrushChangeTracker()

	// Cold start: no stored cursor, full enumeration is expected.
	ids, cold, snapshot, err := tracker.changedSessionIDs(context.Background(), fixture.dbPath)
	require.NoError(t, err)
	assert.True(t, cold)
	assert.Empty(t, ids)
	tracker.commit(fixture.dbPath, snapshot)

	// A new message in sess-a must report only sess-a, not sess-b.
	fixture.insertMessage(t, "msg-new", "sess-a", "user", `[
		{"type":"text","data":{"text":"more"}}
	]`, created+1, "", "")
	ids, cold, _, err = tracker.changedSessionIDs(context.Background(), fixture.dbPath)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Equal(t, []string{"sess-a"}, ids)

	// Committing again drains the cursor.
	_, cold, snapshot, err = tracker.changedSessionIDs(context.Background(), fixture.dbPath)
	require.NoError(t, err)
	assert.False(t, cold)
	tracker.commit(fixture.dbPath, snapshot)
	ids, cold, _, err = tracker.changedSessionIDs(context.Background(), fixture.dbPath)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Empty(t, ids)
}

func TestCrushFingerprintReflectsMessageContent(t *testing.T) {
	fixture := newCrushTestFixture(t)
	const created = int64(1_789_093_626)
	fixture.insertSession(t, "sess-fp", "Fingerprint", "",
		created, created, 0, 0, 0)
	fixture.insertMessage(t, "msg-fp", "sess-fp", "user", `[
		{"type":"text","data":{"text":"before"}}
	]`, created, "", "")

	first, found, err := crushSessionFingerprint(
		context.Background(), fixture.dbPath, "sess-fp",
	)
	require.NoError(t, err)
	require.True(t, found)

	// An edit within the same second leaves every timestamp unchanged;
	// the content hash must still move.
	_, err = fixture.database.Exec(`
		UPDATE messages SET parts = '[{"type":"text","data":{"text":"after"}}]'
		WHERE id = 'msg-fp'
	`)
	require.NoError(t, err)
	second, found, err := crushSessionFingerprint(
		context.Background(), fixture.dbPath, "sess-fp",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqual(t, first, second,
		"a same-second parts edit must change the fingerprint")

	// A vanished session reports not-found rather than a stale hash.
	_, found, err = crushSessionFingerprint(
		context.Background(), fixture.dbPath, "sess-missing",
	)
	require.NoError(t, err)
	assert.False(t, found)
}
