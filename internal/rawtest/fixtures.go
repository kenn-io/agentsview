package rawtest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

const ClaudeID = "captured-session"
const ZCodeID = "zcode:captured-session"
const ZCodeBillableID = "zcode:billable-only"

// Claude includes a persisted companion, a failed tool, thinking, explicit zero
// usage and absent usage. The synthetic credential-shaped text exercises findings.
func Claude(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "project", ClaudeID+".jsonl")
	companion := filepath.Join(root, "project", ClaudeID, "tool-results", "result.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(companion), 0700))
	require.NoError(t, os.WriteFile(companion, []byte("Error: synthetic build failure\n"), 0600))
	notice, _ := json.Marshal("<persisted-output>\nFull output saved to: " + companion + "\n</persisted-output>")
	quotedPath, _ := json.Marshal(companion)
	// Derive a credential-shaped scanner input from public fixture text; no
	// credential is used or retained by the test.
	digest := sha256.Sum256([]byte("hosted capture scanner fixture"))
	secret, _ := json.Marshal("Inspect the build. Synthetic scanner fixture: ghp_" + hex.EncodeToString(digest[:])[:36])
	body := `{"type":"user","uuid":"u1","sessionId":"` + ClaudeID + `","timestamp":"2026-07-06T12:00:00Z","cwd":"/workspace/project","message":{"content":` + string(secret) + `}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-07-06T12:00:01Z","message":{"id":"response-1","model":"claude-sonnet-4-6","content":[{"type":"thinking","thinking":"Check the build first."},{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"make check"}}],"usage":{"input_tokens":100,"output_tokens":7,"cache_creation_input_tokens":5,"cache_read_input_tokens":11}}}
{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-07-06T12:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true,"content":` + string(notice) + `}]},"toolUseResult":{"persistedOutputPath":` + string(quotedPath) + `}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"2026-07-06T12:00:03Z","message":{"model":"claude-sonnet-4-6","content":"The build failed.","usage":{"input_tokens":0,"output_tokens":0}}}
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}
func AppendClaude(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"user","uuid":"u3","parentUuid":"a2","timestamp":"2026-07-06T12:01:00Z","message":{"content":"Keep the failure visible."}}
{"type":"assistant","uuid":"a3","parentUuid":"u3","timestamp":"2026-07-06T12:01:01Z","message":{"model":"claude-sonnet-4-6","content":"Recorded the failure.","usage":{"input_tokens":120,"output_tokens":9}}}
`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// ZCode is a live WAL-backed provider database, including billable usage with no
// transcript. Capture must create a safe source snapshot before upload.
func ZCode(t *testing.T, root string) *sql.DB {
	t.Helper()
	dir := filepath.Join(root, "db")
	require.NoError(t, os.MkdirAll(dir, 0700))
	source, err := sql.Open("sqlite3", filepath.Join(dir, parser.ZCodeDBName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE session(id TEXT PRIMARY KEY,project_id TEXT,workspace_id TEXT,directory TEXT,title TEXT,time_created INTEGER,time_updated INTEGER);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created TEXT,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,session_id TEXT,time_created TEXT,data TEXT);
CREATE TABLE model_usage(session_id TEXT,turn_id TEXT,provider_id TEXT,model_id TEXT,status TEXT,input_tokens INTEGER,output_tokens INTEGER,reasoning_tokens INTEGER,cache_creation_input_tokens INTEGER,cache_read_input_tokens INTEGER,computed_total_tokens INTEGER,started_at INTEGER,completed_at INTEGER,duration_ms INTEGER,tool_call_count INTEGER);
INSERT INTO session VALUES('captured-session',NULL,NULL,'/workspace/project','Captured database session',1783339200000,1783339260000),('billable-only',NULL,NULL,'/workspace/project','Billable without transcript',1783339200000,1783339260000);
INSERT INTO message VALUES('u1','captured-session','2026-07-06T12:00:00Z','{"role":"user"}'),('a1','captured-session','2026-07-06T12:00:01Z','{"role":"assistant","model":{"modelID":"claude-sonnet-4-6"}}');
INSERT INTO part VALUES('p1','u1','captured-session','2026-07-06T12:00:00Z','{"type":"text","text":"Inspect the build."}'),('p2','a1','captured-session','2026-07-06T12:00:01Z','{"type":"text","text":"Build inspected."}');
INSERT INTO model_usage VALUES('captured-session','turn-1','synthetic','claude-sonnet-4-6','done',100,7,3,5,11,126,1783339201000,1783339202000,1000,0),('billable-only','turn-2','synthetic','claude-sonnet-4-6','done',200,17,4,6,12,239,1783339201000,1783339202000,1000,0);`)
	require.NoError(t, err)
	return source
}

const CodexChildID = "22222222-2222-4222-8222-222222222222"
const CodexParentID = "11111111-1111-4111-8111-111111111111"

func CodexFork(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", "06")
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, "rollout-2026-07-06T12-00-00-"+CodexChildID+".jsonl")
	body := `{"type":"session_meta","timestamp":"2026-07-06T12:00:00Z","payload":{"id":"` + CodexChildID + `","forked_from_id":"` + CodexParentID + `","cwd":"/workspace/project","originator":"codex_cli_rs"}}
{"type":"turn_context","timestamp":"2026-07-06T12:00:01Z","payload":{"model":"gpt-5.4","turn_id":"child-turn"}}
{"type":"response_item","timestamp":"2026-07-06T12:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the fork."}]}}
{"type":"response_item","timestamp":"2026-07-06T12:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Fork inspected."}]}}
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}
func CodexParent(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "2026", "07", "06", "rollout-2026-07-06T11-00-00-"+CodexParentID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta","timestamp":"2026-07-06T11:00:00Z","payload":{"id":"`+CodexParentID+`","cwd":"/workspace/project","originator":"codex_cli_rs"}}
`), 0600))
}

const CodexToolsID = "33333333-3333-4333-8333-333333333333"

func CodexTools(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", "06")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := `{"type":"session_meta","timestamp":"2026-07-06T12:00:00Z","payload":{"id":"` + CodexToolsID + `","cwd":"/workspace/project","originator":"codex_cli_rs"}}
{"type":"turn_context","timestamp":"2026-07-06T12:00:01Z","payload":{"model":"gpt-5.4","turn_id":"tool-turn"}}
{"type":"response_item","timestamp":"2026-07-06T12:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Run the build."}]}}
{"type":"response_item","timestamp":"2026-07-06T12:00:02Z","payload":{"type":"function_call","name":"exec_command","call_id":"build-1","arguments":"{\"cmd\":\"make check\"}"}}
{"type":"response_item","timestamp":"2026-07-06T12:00:03Z","payload":{"type":"function_call_output","call_id":"build-1","output":"Process exited with code 1\nError: synthetic failure"}}
{"type":"response_item","timestamp":"2026-07-06T12:00:04Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Build failed."}]}}
{"type":"event_msg","timestamp":"2026-07-06T12:00:05Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":9,"total_tokens":129},"last_token_usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":9,"total_tokens":129}}}}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-2026-07-06T12-00-00-"+CodexToolsID+".jsonl"), []byte(body), 0600))
}

const ClaudeChildID = "agent-captured-child"

func ClaudeChild(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "project", ClaudeID, "subagents", ClaudeChildID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"child-u1","isSidechain":true,"timestamp":"2026-07-06T12:00:01Z","cwd":"/workspace/project","message":{"content":"Inspect the child task."}}
{"type":"assistant","uuid":"child-a1","parentUuid":"child-u1","isSidechain":true,"timestamp":"2026-07-06T12:00:02Z","message":{"content":"Child task inspected."}}
`), 0600))
}
