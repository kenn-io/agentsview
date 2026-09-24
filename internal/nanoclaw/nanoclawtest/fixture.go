// Package nanoclawtest builds synthetic NanoClaw cells for tests: a v2.db
// with the schema subset jilog's tests query (nanoclaw.rs:655-690) and
// Claude Code transcripts under <data>/v2-sessions. Every name is
// synthetic; jilog's fixture names are scrubbed.
package nanoclawtest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // "sqlite3" driver for the fixture DB
	"github.com/stretchr/testify/require"
)

const v2Schema = `
CREATE TABLE agent_groups (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, folder TEXT NOT NULL UNIQUE,
    agent_provider TEXT, created_at TEXT NOT NULL
);
CREATE TABLE messaging_groups (
    id TEXT PRIMARY KEY, channel_type TEXT NOT NULL, platform_id TEXT NOT NULL,
    instance TEXT NOT NULL, name TEXT, is_group INTEGER DEFAULT 0,
    unknown_sender_policy TEXT NOT NULL DEFAULT 'strict', created_at TEXT NOT NULL,
    denied_at TEXT
);
CREATE TABLE messaging_group_agents (
    id TEXT PRIMARY KEY, messaging_group_id TEXT NOT NULL,
    agent_group_id TEXT NOT NULL, session_mode TEXT DEFAULT 'shared',
    priority INTEGER DEFAULT 0, created_at TEXT NOT NULL
);`

const v2Rows = `
INSERT INTO agent_groups VALUES
    ('ag-1', 'helper', 'general', NULL, '2026-05-06'),
    ('ag-2', 'reviewer', 'reviewer', NULL, '2026-05-06'),
    ('ag-3', 'helper', 'steering', NULL, '2026-05-06');
INSERT INTO messaging_groups VALUES
    ('mg-1', 'chat', '1@g.example', 'chat', 'general', 1, 'public', '2026-05-06', NULL),
    ('mg-2', 'chat', '2@g.example', 'chat', 'events team', 1, 'public', '2026-05-06', NULL),
    ('mg-3', 'chat', '3@g.example', 'chat', 'steering committee', 1, 'public', '2026-05-06', NULL);
INSERT INTO messaging_group_agents VALUES
    ('mga-1', 'mg-1', 'ag-1', 'shared', 0, '2026-05-06'),
    ('mga-2', 'mg-2', 'ag-2', 'shared', 0, '2026-05-06'),
    ('mga-3', 'mg-3', 'ag-3', 'shared', 0, '2026-05-06');`

// WriteV2DB creates <dataDir>/v2.db with three agents: ag-1 (helper,
// folder general, channel general), ag-2 (reviewer/reviewer, events team)
// and ag-3 (helper, folder steering, steering committee). It returns the
// database path.
func WriteV2DB(t testing.TB, dataDir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	path := filepath.Join(dataDir, "v2.db")
	Exec(t, path, v2Schema+v2Rows)
	return path
}

// Exec runs stmts against the SQLite database at dbPath, creating it when
// absent. Multi-statement strings must not take args.
func Exec(t testing.TB, dbPath, stmts string, args ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(stmts, args...)
	require.NoError(t, err)
}

// ProjectsRoot is the Claude root a user configures for one cell agent.
func ProjectsRoot(dataDir, agent string) string {
	return filepath.Join(dataDir, "v2-sessions", agent, ".claude-shared", "projects")
}

// SessionPath is where a cell agent's session transcript lives.
func SessionPath(dataDir, agent, session string) string {
	return filepath.Join(ProjectsRoot(dataDir, agent), "-workspace-agent", session+".jsonl")
}

// WriteSession writes body as the agent's session transcript and returns
// its path.
func WriteSession(t testing.TB, dataDir, agent, session, body string) string {
	t.Helper()
	path := SessionPath(dataDir, agent, session)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// CellSessions is write_cell's layout (nanoclaw.rs:705-717): three mapped
// agents and one directory v2.db does not know.
var CellSessions = [][2]string{
	{"ag-1", "s-1"}, {"ag-2", "s-2"}, {"ag-3", "s-3"}, {"ag-unknown", "s-4"},
}

// WriteCell writes the v2.db and SessionBody for every CellSessions entry.
func WriteCell(t testing.TB, dataDir string) {
	t.Helper()
	WriteV2DB(t, dataDir)
	for _, cs := range CellSessions {
		WriteSession(t, dataDir, cs[0], cs[1], SessionBody)
	}
}

// SessionBody is nanoclaw.rs:695-703 (SESSION_BODY), scrubbed: a
// queue-operation line, an envelope user turn, an assistant turn with text
// and a Bash tool_use, the failing tool_result echo, a closing assistant
// turn, and three session-meta lines.
const SessionBody = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-08T10:11:51.540Z","sessionId":"s-1","content":"<message id=\"2\" from=\"chat-mg-1\">@helper ping</message>"}
{"type":"user","uuid":"u1","timestamp":"2026-07-08T10:11:51.576Z","message":{"role":"user","content":"<context timezone=\"UTC\" />\n<message id=\"2\" from=\"chat-mg-1\" sender=\"100@example.invalid\" time=\"Jul 8, 2026, 4:11 PM\">no helper, don&#39;t answer in that channel</message>"},"sessionId":"s-1"}
{"type":"assistant","uuid":"a1","timestamp":"2026-07-08T10:12:00.000Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"Understood, I will stay quiet there."},{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"true"}}],"usage":{"input_tokens":100,"cache_creation_input_tokens":200,"cache_read_input_tokens":300,"output_tokens":42}},"sessionId":"s-1"}
{"type":"user","uuid":"u2","timestamp":"2026-07-08T10:12:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","is_error":true,"content":"command not found"}]},"sessionId":"s-1"}
{"type":"assistant","uuid":"a2","timestamp":"2026-07-08T10:12:05.000Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"That failed."}],"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":600,"output_tokens":7}},"sessionId":"s-1"}
{"type":"ai-title","aiTitle":"quiet channel request","sessionId":"s-1"}
{"type":"last-prompt","leafUuid":"a2","sessionId":"s-1"}
{"type":"mode","mode":"normal","sessionId":"s-1"}
`
