package parser

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// cbcHome lays out a CodeBuddy home with one project directory and returns
// the home and the project directory.
func cbcHome(t *testing.T) (home, project string) {
	t.Helper()
	home = t.TempDir()
	project = filepath.Join(home, "projects", "-work-demo")
	require.NoError(t, os.MkdirAll(project, 0o755))
	return home, project
}

func writeCBC(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func parseCBCHome(t *testing.T, roots ...string) []ParseResult {
	t.Helper()
	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: roots, Machine: "m"})
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	var out []ParseResult
	for _, source := range sources {
		outcome, err := p.Parse(t.Context(), ParseRequest{Source: source, Machine: "m"})
		require.NoError(t, err)
		for _, r := range outcome.Results {
			out = append(out, r.Result)
		}
	}
	return out
}

func cbcSession(t *testing.T, results []ParseResult, id string) ParseResult {
	t.Helper()
	for _, r := range results {
		if r.Session.ID == id {
			return r
		}
	}
	require.Failf(t, "session not found", "%s", id)
	return ParseResult{}
}

func TestCodeBuddyCBCConversation(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-1","cwd":"/work/demo","content":[{"type":"input_text","text":"<system-reminder data-role=\"user-context\">memory</system-reminder>"},{"type":"input_text","text":"List the files"}],"providerData":{"agent":"cli","conversationRequestId":"r1"}}`,
		`{"id":"t1","type":"ai-title","aiTitle":"Listing files","timestamp":1001}`,
		`{"id":"a1","parentId":"u1","type":"reasoning","timestamp":1002,"rawContent":[{"type":"reasoning_text","text":"I should run ls"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","stepSeq":1,"model":"glm-x"}}`,
		`{"id":"a2","parentId":"a1","type":"function_call","timestamp":1003,"name":"Bash","callId":"c1","arguments":"{\"command\":\"ls\"}","providerData":{"conversationRequestId":"r1","messageId":"m1","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":60},"prompt_cache_miss_tokens":40,"cache_creation_input_tokens":0,"completion_thinking_tokens":12}}}`,
		`{"id":"a3","parentId":"a2","type":"function_call_result","timestamp":1004,"name":"Bash","callId":"c1","status":"completed","output":{"type":"text","text":"a.go\nb.go"},"providerData":{"conversationRequestId":"r1","messageId":"m1"}}`,
		`{"id":"a4","parentId":"a3","type":"message","role":"assistant","timestamp":1005,"status":"completed","content":[{"type":"output_text","text":"Two Go files."}],"providerData":{"conversationRequestId":"r1","messageId":"m2","stepSeq":2,"model":"glm-x","rawUsage":{"prompt_tokens":150,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":100}}}}`,
		`{"id":"mu","parentId":"a4","type":"model-usage","timestamp":1006,"providerData":{"conversationRequestId":"r1","messageId":"m3","stepSeq":3,"model":"glm-x","rawUsage":{"prompt_tokens":10,"completion_tokens":5}}}`,
		`{"id":"cu","parentId":"a4","type":"context-usage","timestamp":1007,"contextTokens":999}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 1)
	s := results[0].Session
	assert.Equal(t, "codebuddy:s1", s.ID)
	assert.Equal(t, "run-1", s.SourceSessionID)
	assert.Equal(t, "Listing files", s.SessionName)
	assert.Equal(t, "List the files", s.FirstMessage)
	assert.Equal(t, "cli", s.Entrypoint)
	assert.Equal(t, 1, s.UserMessageCount)

	msgs := results[0].Messages
	require.Len(t, msgs, 6)
	assert.Equal(t, "system_reminder", msgs[0].SourceSubtype)
	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, "List the files", msgs[1].Content)

	call := msgs[2]
	assert.Equal(t, RoleAssistant, call.Role)
	assert.Equal(t, "I should run ls", call.ThinkingText)
	require.Len(t, call.ToolCalls, 1)
	assert.Equal(t, "Bash", call.ToolCalls[0].ToolName)
	assert.Equal(t, "cb:r1:m1:1", call.SourceUUID)
	// prompt 100 = 60 cache reads + 40 uncached; reasoning is part of output.
	assert.JSONEq(t, `{"input_tokens":40,"output_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":0,"reasoning_tokens":12}`, string(call.TokenUsage))
	assert.Equal(t, 100, call.ContextTokens)

	require.Len(t, msgs[3].ToolResults, 1)
	assert.Equal(t, "c1", msgs[3].ToolResults[0].ToolUseID)
	assert.Equal(t, `"a.go\nb.go"`, msgs[3].ToolResults[0].ContentRaw)

	reply := msgs[4]
	assert.Equal(t, "Two Go files.", reply.Content)
	assert.Equal(t, 0, reply.OutputTokens)
	assert.True(t, reply.HasOutputTokens, "an explicit zero output is still reported")

	usageOnly := msgs[5]
	assert.True(t, usageOnly.IsSystem)
	assert.Empty(t, usageOnly.Content)
	assert.Equal(t, "cb:r1:m3:3", usageOnly.SourceUUID)
	assert.Equal(t, 5, usageOnly.OutputTokens)
}

func TestCodeBuddyCBCResendBranches(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s2.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"first try"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"old answer"}]}`,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1002}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1003,"content":[{"type":"input_text","text":"second try"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1004,"content":[{"type":"output_text","text":"new answer"}]}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 2)
	main := cbcSession(t, results, "codebuddy:s2")
	require.Len(t, main.Messages, 2)
	assert.Equal(t, "second try", main.Messages[0].Content)
	assert.Equal(t, "new answer", main.Messages[1].Content)

	fork := cbcSession(t, results, "codebuddy:s2-u1")
	assert.Equal(t, "codebuddy:s2", fork.Session.ParentSessionID)
	assert.Equal(t, RelFork, fork.Session.RelationshipType)
	assert.Empty(t, fork.Session.SourceSessionID, "a preserved branch cannot be resumed")
	require.Len(t, fork.Messages, 2)
	assert.Equal(t, "old answer", fork.Messages[1].Content)
}

func TestCodeBuddyCBCPendingResendEndsAtNoticeParent(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s3.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"keep me"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"kept"}]}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1002,"content":[{"type":"input_text","text":"rewound"}]}`,
		`{"id":"n1","parentId":"a1","type":"resend-fork-notice","timestamp":1003}`,
	)
	results := parseCBCHome(t, home)
	main := cbcSession(t, results, "codebuddy:s3")
	require.Len(t, main.Messages, 2)
	assert.Equal(t, "kept", main.Messages[1].Content)
	fork := cbcSession(t, results, "codebuddy:s3-u2")
	assert.Equal(t, "rewound", fork.Messages[0].Content)
}

func TestCodeBuddyCBCParallelToolsStayOnePath(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s4.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"read both"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"Read","callId":"x","arguments":"{}","providerData":{"conversationRequestId":"r","messageId":"m"}}`,
		`{"id":"c2","parentId":"c1","type":"function_call","timestamp":1002,"name":"Read","callId":"y","arguments":"{}","providerData":{"conversationRequestId":"r","messageId":"m"}}`,
		`{"id":"r1","parentId":"c2","type":"function_call_result","timestamp":1003,"name":"Read","callId":"x","output":"one"}`,
		`{"id":"r2","parentId":"r1","type":"function_call_result","timestamp":1004,"name":"Read","callId":"y","output":"two"}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 1, "parallel calls are not a user branch")
	msgs := results[0].Messages
	require.Len(t, msgs, 4)
	assert.Len(t, msgs[1].ToolCalls, 2, "calls of one response merge into one message")
}

func TestCodeBuddyCBCUserClassification(t *testing.T) {
	for _, tc := range []struct {
		name, content, providerData string
		wantPrompt, wantSubtype     string
	}{
		{"query wrapper", `[{"type":"input_text","text":"<user_query>fix it</user_query>"}]`, `{}`, "fix it", ""},
		{"quoted query stays", `[{"type":"input_text","text":"note <user_query>old</user_query> here"}]`, `{}`, "note <user_query>old</user_query> here", ""},
		{"slash command", `[{"type":"input_text","text":"<command-name>/goal</command-name><command-args>ship</command-args>"}]`, `{}`, "/goal ship", ""},
		{"bash input", `[{"type":"input_text","text":"<bash-input>ls</bash-input>"}]`, `{}`, "! ls", ""},
		{"teammate", `[{"type":"input_text","text":"<teammate-message teammate_id=\"lead\">done</teammate-message>"}]`, `{}`, "", "peer_message"},
		{"task notification", `[{"type":"input_text","text":"<task-notification>finished</task-notification>"}]`, `{}`, "", "task_notification"},
		{"compact summary", `[{"type":"input_text","text":"<cb_summary>earlier work</cb_summary>"}]`, `{}`, "", "compact_boundary"},
		{"aborted turn", `[{"type":"input_text","text":"stopped"}]`, `{"turnAborted":true}`, "", "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			writeCBC(t, filepath.Join(project, "s.jsonl"),
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":`+tc.content+`,"providerData":`+tc.providerData+`}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"ok"}]}`,
			)
			results := parseCBCHome(t, home)
			require.Len(t, results, 1)
			first := results[0].Messages[0]
			if tc.wantSubtype == "" {
				assert.False(t, first.IsSystem)
				assert.Equal(t, tc.wantPrompt, first.Content)
				assert.Equal(t, tc.wantPrompt, results[0].Session.FirstMessage)
				return
			}
			assert.True(t, first.IsSystem)
			assert.Equal(t, tc.wantSubtype, first.SourceSubtype)
			assert.Equal(t, 0, results[0].Session.UserMessageCount)
		})
	}
}

// cbcPNG is a 1x1 PNG.
const cbcPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func TestCodeBuddyCBCImagesReadOnlyFromBlobStore(t *testing.T) {
	home, project := cbcHome(t)
	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	blob := strings.Repeat("ab", 32)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "blobs", "ab"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "blobs", "ab", blob+".png"), png, 0o600))
	outside := filepath.Join(t.TempDir(), "secret.png")
	require.NoError(t, os.WriteFile(outside, png, 0o600))
	writeCBC(t, filepath.Join(project, "s5.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"what is this"},{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png","original_filename":"shot.png"},{"type":"image_blob_ref","blob_id":"../../x","blob_path":"`+filepath.ToSlash(outside)+`","original_filename":"escape.png"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"a pixel"}]}`,
	)
	assetsDir := t.TempDir()
	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{home}, Machine: "m", AssetsDir: assetsDir})
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := p.Parse(t.Context(), ParseRequest{Source: sources[0], Machine: "m"})
	require.NoError(t, err)
	prompt := outcome.Results[0].Result.Messages[0].Content
	assert.Contains(t, prompt, "what is this")
	assert.Contains(t, prompt, "![image](asset://")
	assert.Contains(t, prompt, "[image unavailable: escape.png]")
	stored, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, stored, 1, "only the blob-store image is copied")
}

func TestCodeBuddyCBCPersistedToolOutput(t *testing.T) {
	home, project := cbcHome(t)
	own := filepath.Join(project, "s6", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(own), 0o755))
	require.NoError(t, os.WriteFile(own, []byte("full output body"), 0o600))
	other := filepath.Join(project, "other", "tool-results", "c2.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o755))
	require.NoError(t, os.WriteFile(other, []byte("someone else's"), 0o600))
	placeholder := func(path string) string {
		return `"<persisted-output>\nOutput too large (1KB). Full output saved to: ` + filepath.ToSlash(path) + `\n</persisted-output>"`
	}
	writeCBC(t, filepath.Join(project, "s6.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1002,"name":"Bash","callId":"c1","output":`+placeholder(own)+`}`,
		`{"id":"c2","parentId":"r1","type":"function_call","timestamp":1003,"name":"Bash","callId":"c2","arguments":"{}"}`,
		`{"id":"r2","parentId":"c2","type":"function_call_result","timestamp":1004,"name":"Bash","callId":"c2","output":`+placeholder(other)+`}`,
		`{"id":"c3","parentId":"r2","type":"function_call","timestamp":1005,"name":"Read","callId":"c3","arguments":"{}"}`,
		`{"id":"r3","parentId":"c3","type":"function_call_result","timestamp":1006,"name":"Read","callId":"c3","output":"1: log quoting <persisted-output> Full output saved to: /nowhere.txt"}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 1)
	var got []ParsedToolResult
	for _, m := range results[0].Messages {
		got = append(got, m.ToolResults...)
	}
	require.Len(t, got, 3)
	assert.Equal(t, `"full output body"`, got[0].ContentRaw)
	assert.False(t, got[0].PersistedOutputUnresolved)
	assert.Contains(t, got[1].ContentRaw, "persisted-output", "another session's side file is never read")
	assert.True(t, got[1].PersistedOutputUnresolved)
	assert.False(t, got[2].PersistedOutputUnresolved, "quoted placeholder text is ordinary output")
}

func TestCodeBuddyCBCPersistedOutputKeepsInvalidUTF8(t *testing.T) {
	home, project := cbcHome(t)
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("head\xffbody"), 0o600))
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1002,"name":"Bash","callId":"c1","output":"<persisted-output>\nFull output saved to: `+filepath.ToSlash(side)+`\n</persisted-output>"}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 1)
	var got []ParsedToolResult
	for _, m := range results[0].Messages {
		got = append(got, m.ToolResults...)
	}
	require.Len(t, got, 1)
	assert.Equal(t, "head\uFFFDbody", DecodeContent(got[0].ContentRaw),
		"a side file with invalid UTF-8 keeps its readable text")
}

func TestCodeBuddyReferencedBlobsAcceptsSpacedJSON(t *testing.T) {
	home, project := cbcHome(t)
	blob := strings.Repeat("ab", 32)
	stored := filepath.Join(home, "blobs", "ab", blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
	require.NoError(t, os.WriteFile(stored, []byte("png"), 0o600))
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id": "u1", "type": "message", "role": "user", "content": [{"type": "image_blob_ref", "blob_id": "`+blob+`"}]}`)
	blobs, err := CodeBuddyReferencedBlobs(t.Context(), filepath.Join(home, "projects"))
	require.NoError(t, err)
	assert.Equal(t, []string{stored}, blobs)
}

func TestCodeBuddyCBCAskUserAnswerIsDialogue(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s7.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"set it up"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"AskUserQuestion","callId":"q","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1002,"name":"AskUserQuestion","callId":"q","output":{"type":"text","text":" · Which path? → /srv/app\n · Include samples? → yes"}}`,
	)
	results := parseCBCHome(t, home)
	msgs := results[0].Messages
	require.Len(t, msgs, 3)
	answer := msgs[2]
	assert.False(t, answer.IsSystem)
	assert.Equal(t, "ask_user_answer", answer.SourceSubtype)
	assert.Equal(t, "[Answer: Which path?] /srv/app\n[Answer: Include samples?] yes", answer.Content)
	assert.Equal(t, 2, results[0].Session.UserMessageCount)
}

func TestCodeBuddyCBCSubagentLinks(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s8.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"delegate"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"Agent","callId":"t1","arguments":"{\"prompt\":\"look\"}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1002,"name":"Agent","callId":"t1","output":"found it","providerData":{"toolResult":{"subAgent":{"sessionId":"agent-7"}}}}`,
	)
	writeCBC(t, filepath.Join(project, "s8", "subagents", "agent-7.jsonl"),
		`{"id":"su","type":"message","role":"user","timestamp":1001,"content":[{"type":"input_text","text":"look"}]}`,
		`{"id":"sa","parentId":"su","type":"message","role":"assistant","timestamp":1002,"content":[{"type":"output_text","text":"found it"}]}`,
	)
	results := parseCBCHome(t, home)
	parent := cbcSession(t, results, "codebuddy:s8")
	assert.Equal(t, "codebuddy:s8:subagent:agent-7", parent.Messages[1].ToolCalls[0].SubagentSessionID)
	child := cbcSession(t, results, "codebuddy:s8:subagent:agent-7")
	assert.Equal(t, "codebuddy:s8", child.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, child.Session.RelationshipType)
}

func TestCodeBuddyCBCTruncatedTailAndEmptyFiles(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "meta.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"meta","meta":{}}`)
	full := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hello"}]}` + "\n" +
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hi"}]}` + "\n" +
		`{"id":"bad"` + "\n" +
		`{"id":"a2","parentId":"a1","type":"mess`
	require.NoError(t, os.WriteFile(filepath.Join(project, "tail.jsonl"), []byte(full), 0o600))
	results := parseCBCHome(t, home)
	require.Len(t, results, 1, "a transcript with only metadata yields no session")
	s := results[0].Session
	assert.True(t, s.IsTruncated)
	assert.Equal(t, 1, s.MalformedLines, "the cut-off tail is an in-progress write")
	assert.Len(t, results[0].Messages, 2)
}

func TestCodeBuddyCBCTitles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"custom beats ai", []string{
			`{"id":"x1","type":"ai-title","aiTitle":"Generated","timestamp":2}`,
			`{"id":"x2","type":"custom-title","customTitle":"Mine","timestamp":1}`,
		}, "Mine"},
		{"placeholder ai falls back to topic", []string{
			`{"id":"x1","type":"ai-title","aiTitle":"(No content)","timestamp":2}`,
			`{"id":"x2","type":"topic","topic":"Topic title","timestamp":1}`,
		}, "Topic title"},
		{"newest ai wins", []string{
			`{"id":"x1","type":"ai-title","aiTitle":"Later","timestamp":5}`,
			`{"id":"x2","type":"ai-title","aiTitle":"Earlier","timestamp":3}`,
		}, "Later"},
		{"first input when untitled", nil, "a question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			lines := append([]string{
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"a question"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"an answer"}]}`,
			}, tc.lines...)
			writeCBC(t, filepath.Join(project, "s.jsonl"), lines...)
			results := parseCBCHome(t, home)
			assert.Equal(t, tc.want, results[0].Session.SessionName)
		})
	}
}

const cbcCatalogSchema = `
CREATE TABLE conversations (id TEXT PRIMARY KEY, workspace_authority TEXT,
  title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_provider TEXT,
  runtime_session_id TEXT, fork_source_id TEXT, deleted_at INTEGER,
  archived_at INTEGER);
CREATE TABLE conversation_runtime_sources (authority TEXT NOT NULL,
  runtime_session_id TEXT NOT NULL, conversation_id TEXT NOT NULL);
CREATE TABLE btw_entries (conversation_id TEXT PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE discussion_threads (conversation_id TEXT PRIMARY KEY, payload TEXT NOT NULL);
`

// writeCBCCatalog applies statements to the home's catalog, creating it with
// the columns the reader uses on first call.
func writeCBCCatalog(t *testing.T, home string, statements ...string) {
	t.Helper()
	path := filepath.Join(home, CodeBuddyCatalogDBName)
	_, statErr := os.Stat(path)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	if os.IsNotExist(statErr) {
		_, err = db.ExecContext(t.Context(), cbcCatalogSchema)
		require.NoError(t, err)
	}
	for _, stmt := range statements {
		_, err := db.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
}

func TestCodeBuddyCBCCatalogMetadata(t *testing.T) {
	home, project := cbcHome(t)
	for _, id := range []string{"run-a", "run-b", "run-c"} {
		writeCBC(t, filepath.Join(project, id+".jsonl"),
			`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"`+id+`","content":[{"type":"input_text","text":"hi `+id+`"}]}`,
			`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hello"}]}`,
			`{"id":"t1","type":"ai-title","aiTitle":"Transcript title","timestamp":1002}`,
		)
	}
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, custom_title, runtime_session_id) VALUES ('conv-a', 'Catalog title', 'Renamed', 'run-a')`,
		`INSERT INTO conversations (id, title, fork_source_id) VALUES ('conv-b', 'Forked', 'conv-a')`,
		`INSERT INTO conversation_runtime_sources VALUES ('', 'run-b', 'conv-b')`,
		`INSERT INTO conversations (id, workspace_authority, title, runtime_session_id) VALUES ('remote', 'ssh-host', 'Remote', 'run-c')`,
		`INSERT INTO btw_entries VALUES ('conv-a', '[{"id":"b1","question":"side q","answer":"side a","createdAt":2000},{"id":"b2","question":"with pics","status":"error","error":"quota","imageCount":2,"createdAt":3000}]')`,
		`INSERT INTO discussion_threads VALUES ('conv-a', '[{"id":"d1","anchor":{"quote":"hello","messageId":"a1"},"createdAt":2500,"entries":[{"id":"e1","question":"why?","answer":"because","createdAt":2500}]}]')`,
	)
	results := parseCBCHome(t, home)
	a := cbcSession(t, results, "codebuddy:run-a")
	assert.Equal(t, "Renamed", a.Session.SessionName, "a custom catalog title wins")
	b := cbcSession(t, results, "codebuddy:run-b")
	assert.Equal(t, "Forked", b.Session.SessionName, "the IDE title beats the generated transcript title")
	assert.Equal(t, "codebuddy:run-a", b.Session.ParentSessionID, "the fork parent resolves to its runtime session")
	assert.Equal(t, RelFork, b.Session.RelationshipType)

	btw := cbcSession(t, results, "codebuddy:run-a:btw")
	assert.Equal(t, "btw", btw.Session.SessionKind)
	assert.Equal(t, "codebuddy:run-a", btw.Session.ParentSessionID)
	require.Len(t, btw.Messages, 4)
	assert.Equal(t, "side a", btw.Messages[1].Content)
	assert.Contains(t, btw.Messages[2].Content, "2 image(s) attached; CodeBuddy did not store them")
	assert.Equal(t, "[Side question failed] quota", btw.Messages[3].Content)
	assert.True(t, btw.Messages[3].IsSystem)

	discussion := cbcSession(t, results, "codebuddy:run-a:discussion:d1")
	assert.Equal(t, "discussion_anchor", discussion.Messages[0].SourceSubtype)
	assert.Equal(t, "> hello", discussion.Messages[0].Content)
	c := cbcSession(t, results, "codebuddy:run-c")
	assert.Equal(t, "Transcript title", c.Session.SessionName,
		"another machine's catalog row does not rename a local conversation")
	for _, r := range results {
		assert.NotEqual(t, "Remote", r.Session.SessionName, "remote workspace rows are not local sessions")
	}
}

func clearCodeBuddyCatalogCache() {
	codebuddyCatalogCache.Lock()
	clear(codebuddyCatalogCache.byPath)
	codebuddyCatalogCache.Unlock()
}

func TestCodeBuddyCBCRemoteServerSideQuestions(t *testing.T) {
	home, project := cbcHome(t)
	transcript := filepath.Join(project, "run-a.jsonl")
	writeCBC(t, transcript,
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-a","content":[{"type":"input_text","text":"hi"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hello"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","rawUsage":{"prompt_tokens":500,"completion_tokens":40}}}`)
	// The IDE's remote server writes the side-question tables keyed by
	// runtime session id, without the conversations catalog.
	db, err := sql.Open("sqlite3", filepath.Join(home, CodeBuddyCatalogDBName))
	require.NoError(t, err)
	for _, stmt := range []string{
		`CREATE TABLE btw_entries (conversation_id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE discussion_threads (conversation_id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO btw_entries VALUES ('run-a', '[{"id":"b1","question":"side q","answer":"side a","createdAt":2000}]', 1)`,
		`INSERT INTO btw_entries VALUES ('run-gone', '[{"id":"b2","question":"orphan","createdAt":2000}]', 1)`,
		`INSERT INTO discussion_threads VALUES ('run-a', '[{"id":"d1","anchor":{"quote":"hello","messageId":"a1"},"createdAt":2500,"entries":[{"id":"e1","question":"why?","answer":"because","createdAt":2500}]}]', 1)`,
	} {
		_, err := db.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	results := parseCBCHome(t, home)
	require.Len(t, results, 3, "the main session, its /btw thread, and its discussion")
	main := cbcSession(t, results, "codebuddy:run-a")
	assert.Equal(t, 40, main.Session.TotalOutputTokens)
	btw := cbcSession(t, results, "codebuddy:run-a:btw")
	assert.Equal(t, "codebuddy:run-a", btw.Session.ParentSessionID)
	require.Len(t, btw.Messages, 2)
	assert.Equal(t, "side a", btw.Messages[1].Content)
	assert.False(t, btw.Session.HasTotalOutputTokens || btw.Session.HasPeakContextTokens,
		"a side thread does not repeat the conversation's token totals")
	assert.Empty(t, btw.Session.SourceSessionID, "a side thread has no CLI session to resume")
	assert.Equal(t, "codebuddy:run-a",
		cbcSession(t, results, "codebuddy:run-a:discussion:d1").Session.ParentSessionID)

	_, ok, err := CodeBuddyIDEConversationID(t.Context(), transcript, "run-a")
	require.NoError(t, err)
	assert.False(t, ok, "without the conversations table the IDE has no conversation to open")
}

func TestCodeBuddyCBCUnreadableCatalogKeepsLastGoodRead(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "run-a.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-a","content":[{"type":"input_text","text":"hi"}]}`)
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, custom_title, runtime_session_id) VALUES ('conv-a', 'T', 'Named in IDE', 'run-a')`)
	results := parseCBCHome(t, home)
	assert.Equal(t, "Named in IDE", cbcSession(t, results, "codebuddy:run-a").Session.SessionName)

	// The IDE rewrites the catalog and a read lands mid-write.
	require.NoError(t, os.WriteFile(filepath.Join(home, CodeBuddyCatalogDBName), []byte("not a database"), 0o600))
	results = parseCBCHome(t, home)
	assert.Equal(t, "Named in IDE", cbcSession(t, results, "codebuddy:run-a").Session.SessionName,
		"an unreadable catalog keeps the last good metadata")

	// Without an earlier good read, the parse fails so the archive keeps
	// its stored copy and the source is retried.
	clearCodeBuddyCatalogCache()
	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{home}, Machine: "m"})
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	_, err = p.Parse(t.Context(), ParseRequest{Source: sources[0], Machine: "m"})
	assert.ErrorContains(t, err, "CodeBuddy catalog")
}

func TestCodeBuddyCBCCatalogChangeSelectsChangedRows(t *testing.T) {
	for _, unrelated := range []int{1, 50} {
		home, project := cbcHome(t)
		ids := []string{"target"}
		for i := range unrelated {
			ids = append(ids, "other-"+strings.Repeat("x", i+1))
		}
		var stmts []string
		for _, id := range ids {
			writeCBC(t, filepath.Join(project, id+".jsonl"),
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"`+id+`","content":[{"type":"input_text","text":"hi"}]}`)
			stmts = append(stmts, "INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c-"+id+"', 'T', '"+id+"')")
		}
		writeCBCCatalog(t, home, stmts...)
		catalog := filepath.Join(home, CodeBuddyCatalogDBName)
		set := newCodeBuddyCBCSourceSet([]string{filepath.Join(home, "projects")}, "")
		req := ChangedPathRequest{Path: catalog, WatchRoot: home}
		first, err := set.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		assert.Len(t, first, len(ids), "without an earlier read every session is a candidate")

		writeCBCCatalog(t, home, `UPDATE conversations SET custom_title = 'Renamed' WHERE runtime_session_id = 'target'`)
		changed, err := set.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, changed, 1, "with %d unrelated sessions", unrelated)
		assert.Equal(t, filepath.Join(project, "target.jsonl"), changed[0].DisplayPath)

		again, err := set.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		assert.Empty(t, again, "an unchanged catalog selects nothing")
	}
}

func TestCodeBuddyCBCStatHashFollowsOwnCatalogRow(t *testing.T) {
	home, project := cbcHome(t)
	for _, id := range []string{"mine", "theirs"} {
		writeCBC(t, filepath.Join(project, id+".jsonl"),
			`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"`+id+`","content":[{"type":"input_text","text":"hi"}]}`)
	}
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c1', 'Mine', 'mine')`,
		`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c2', 'Theirs', 'theirs')`,
	)
	// The engine probes with a rootless provider.
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	path := filepath.Join(project, "mine.jsonl")
	before := probe.ComputeMultiFileStatHash(path)
	require.NotZero(t, before)

	writeCBCCatalog(t, home, `UPDATE conversations SET title = 'Other rename' WHERE id = 'c2'`)
	clearCodeBuddyCatalogCache()
	assert.Equal(t, before, probe.ComputeMultiFileStatHash(path), "another conversation's row does not invalidate this one")

	writeCBCCatalog(t, home, `UPDATE conversations SET custom_title = 'Renamed' WHERE id = 'c1'`)
	clearCodeBuddyCatalogCache()
	assert.NotEqual(t, before, probe.ComputeMultiFileStatHash(path))
}

// A transcript whose filename differs from the session id it records still
// reads its catalog row: the row is keyed by the recorded id, while the
// archive keeps the filename stem. A fork links to its parent's archive id
// even when the parent's filename also differs from its recorded id.
func TestCodeBuddyCBCCatalogFollowsRecordedSessionID(t *testing.T) {
	home, project := cbcHome(t)
	path := filepath.Join(project, "file-stem.jsonl")
	writeCBC(t, path,
		`{"type":"session-meta","id":"m","sessionId":"run-x","meta":{}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-x","content":[{"type":"input_text","text":"hi"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hello"}]}`,
	)
	writeCBC(t, filepath.Join(project, "parent-file.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":900,"sessionId":"parent","content":[{"type":"input_text","text":"first"}]}`)
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('c-parent', 'Parent', 'parent')`,
		`INSERT INTO conversations (id, title, custom_title, runtime_session_id, fork_source_id) VALUES ('c-x', 'T', 'Named in IDE', 'run-x', 'c-parent')`,
		`INSERT INTO btw_entries VALUES ('c-x', '[{"id":"b1","question":"side q","answer":"side a","createdAt":2000}]')`,
	)
	results := parseCBCHome(t, home)
	parent := cbcSession(t, results, "codebuddy:parent-file")
	assert.Equal(t, "parent", parent.Session.SourceSessionID, "the parent resumes by its recorded id")
	assert.Equal(t, "Parent", parent.Session.SessionName)
	main := cbcSession(t, results, "codebuddy:file-stem")
	assert.Equal(t, "run-x", main.Session.SourceSessionID)
	assert.Equal(t, "Named in IDE", main.Session.SessionName)
	assert.Equal(t, "codebuddy:parent-file", main.Session.ParentSessionID,
		"the fork links to the parent's archive id, its filename")
	assert.Equal(t, RelFork, main.Session.RelationshipType)
	btw := cbcSession(t, results, "codebuddy:file-stem:btw")
	assert.Equal(t, "codebuddy:file-stem", btw.Session.ParentSessionID)

	// The engine probes with a rootless provider.
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{home}, Machine: "m"})
	source, found, err := p.FindSource(t.Context(), FindSourceRequest{StoredFilePath: path})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint := func() string {
		fp, err := p.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		return fp.Hash
	}
	catalog := filepath.Join(home, CodeBuddyCatalogDBName)
	set := newCodeBuddyCBCSourceSet([]string{filepath.Join(home, "projects")}, "")
	req := ChangedPathRequest{Path: catalog, WatchRoot: home}
	_, err = set.SourcesForChangedPath(t.Context(), req)
	require.NoError(t, err)
	beforeStat, beforeHash := probe.ComputeMultiFileStatHash(path), fingerprint()
	require.NotZero(t, beforeStat)

	writeCBCCatalog(t, home, `UPDATE conversations SET custom_title = 'Renamed' WHERE id = 'c-x'`)
	clearCodeBuddyCatalogCache()
	assert.NotEqual(t, beforeStat, probe.ComputeMultiFileStatHash(path), "the recorded id's row feeds the stat hash")
	assert.NotEqual(t, beforeHash, fingerprint(), "the recorded id's row feeds the fingerprint")
	changed, err := set.SourcesForChangedPath(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, changed, 1, "a catalog write for the recorded id reaches the transcript")
	assert.Equal(t, path, changed[0].DisplayPath)
}

func TestCodeBuddyCBCDiscoveryContinuesPastUnreadableDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	for _, blocked := range []string{"project", "subagents"} {
		t.Run(blocked, func(t *testing.T) {
			projects := filepath.Join(t.TempDir(), "projects")
			readable := filepath.Join(projects, "b-readable", "s1.jsonl")
			writeCBC(t, readable, line)
			dir := filepath.Join(projects, "a-blocked")
			if blocked == "subagents" {
				dir = filepath.Join(projects, "b-readable", "s1", "subagents")
			}
			writeCBC(t, filepath.Join(dir, "agent-a.jsonl"), line)
			require.NoError(t, os.Chmod(dir, 0o000))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			// List projects in name order so the blocked one comes first.
			ctx := withStreamingDirectoryReader(t.Context(), func(_ context.Context, path string, yield func(os.DirEntry) error) error {
				entries, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if err := yield(entry); err != nil {
						return err
					}
				}
				return nil
			})

			set := newCodeBuddyCBCSourceSet([]string{projects}, "")
			var paths []string
			err := set.DiscoverEach(ctx, func(source SourceRef) error {
				paths = append(paths, source.DisplayPath)
				return nil
			})
			require.ErrorIs(t, err, os.ErrPermission, "the unreadable directory marks discovery incomplete")
			var incomplete DiscoveryIncompleteError
			require.ErrorAs(t, err, &incomplete)
			assert.Equal(t, []string{readable}, paths,
				"readable transcripts are still found beside the unreadable directory")
		})
	}
}

func TestCodeBuddyCBCStatHashFollowsSavedToolOutputs(t *testing.T) {
	_, project := cbcHome(t)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`)
	output := filepath.Join(project, "run-1", "tool-results", "call-1.txt")
	writeCBC(t, output, "first output")
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	before := probe.ComputeMultiFileStatHash(path)
	require.NotZero(t, before)

	// Rewriting a saved output in place leaves the directory's metadata as
	// it was.
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = f.WriteString("a longer second output")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.NotEqual(t, before, probe.ComputeMultiFileStatHash(path),
		"an edited saved output invalidates the stat hash")
}

// An agent transcript reads saved outputs from its parent's tool-results
// directory too, so a parent output that appears or changes refreshes it.
func TestCodeBuddyCBCSubagentFollowsParentToolOutputs(t *testing.T) {
	_, project := cbcHome(t)
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	parent := filepath.Join(project, "run-1.jsonl")
	child := filepath.Join(project, "run-1", "subagents", "agent-a.jsonl")
	writeCBC(t, parent, line)
	writeCBC(t, child, line)
	// An unrelated file under subagents is not a transcript.
	writeCBC(t, filepath.Join(project, "run-1", "subagents", "notes.jsonl"), line)
	set := newCodeBuddyCBCSourceSet([]string{filepath.Dir(project)}, "")
	source := set.sourceRef(filepath.Dir(project), child)
	fingerprint := func() string {
		t.Helper()
		got, err := set.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		return got.Hash
	}
	probe, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	beforeHash, beforeStat := fingerprint(), probe.ComputeMultiFileStatHash(child)
	require.NotZero(t, beforeStat)

	output := filepath.Join(project, "run-1", "tool-results", "call-1.txt")
	writeCBC(t, output, "parent output")
	afterHash, afterStat := fingerprint(), probe.ComputeMultiFileStatHash(child)
	assert.NotEqual(t, beforeHash, afterHash, "a new parent output changes the agent fingerprint")
	assert.NotEqual(t, beforeStat, afterStat, "a new parent output changes the agent stat hash")

	f, err := os.OpenFile(output, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = f.WriteString("a longer parent output")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.NotEqual(t, afterHash, fingerprint(), "an edited parent output changes the agent fingerprint")
	assert.NotEqual(t, afterStat, probe.ComputeMultiFileStatHash(child), "an edited parent output changes the agent stat hash")

	changed, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: output})
	require.NoError(t, err)
	var paths []string
	for _, source := range changed {
		paths = append(paths, source.DisplayPath)
	}
	assert.ElementsMatch(t, []string{parent, child}, paths, "a parent output refreshes the parent and its agents")

	own := filepath.Join(project, "run-1", "subagents", "agent-a", "tool-results", "call-2.txt")
	writeCBC(t, own, "agent output")
	changed, err = set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: own})
	require.NoError(t, err)
	require.Len(t, changed, 1, "an agent's own output refreshes only that agent")
	assert.Equal(t, child, changed[0].DisplayPath)
}

func TestCodeBuddyDiscoveryKeepsReadableSourcesPastUnreadableCLIDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	projects := filepath.Join(t.TempDir(), "projects")
	readable := filepath.Join(projects, "b-readable", "s1.jsonl")
	writeCBC(t, readable, line)
	blocked := filepath.Join(projects, "a-blocked")
	writeCBC(t, filepath.Join(blocked, "s2.jsonl"), line)
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	legacyRoot := t.TempDir()
	manifest := filepath.Join(legacyRoot, "history", "ws", "conv-1", "index.json")
	writeCBC(t, manifest, `{"messages":[]}`)

	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{projects, legacyRoot}, Machine: "m"})
	require.True(t, p.Capabilities().Sync.PartialDiscovery)
	sources, err := p.Discover(t.Context())
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete, "the unreadable directory marks discovery incomplete")
	var paths []string
	for _, source := range sources {
		paths = append(paths, source.DisplayPath)
	}
	assert.Contains(t, paths, readable, "a readable CLI transcript is still returned")
	assert.Contains(t, paths, manifest, "the 4.x history is still searched")
}

// An unreadable projects root leaves discovery incomplete without hiding the
// conversations in other roots or the 4.x history.
func TestCodeBuddyDiscoveryKeepsReadableRootsPastUnreadableProjectsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	blocked := filepath.Join(t.TempDir(), "projects")
	writeCBC(t, filepath.Join(blocked, "-work-demo", "s2.jsonl"), line)
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	readable := filepath.Join(t.TempDir(), "projects", "-work-demo", "s1.jsonl")
	writeCBC(t, readable, line)
	legacyRoot := t.TempDir()
	manifest := filepath.Join(legacyRoot, "history", "ws", "conv-1", "index.json")
	writeCBC(t, manifest, `{"messages":[]}`)

	for _, order := range []string{"blocked first", "blocked last"} {
		t.Run(order, func(t *testing.T) {
			roots := []string{blocked, filepath.Dir(filepath.Dir(readable)), legacyRoot}
			if order == "blocked last" {
				roots = []string{filepath.Dir(filepath.Dir(readable)), blocked, legacyRoot}
			}
			p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
				NewProvider(ProviderConfig{Roots: roots, Machine: "m"})
			sources, err := p.Discover(t.Context())
			require.ErrorIs(t, err, os.ErrPermission)
			var incomplete DiscoveryIncompleteError
			require.ErrorAs(t, err, &incomplete, "the unreadable root marks discovery incomplete")
			var paths []string
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.ElementsMatch(t, []string{readable, manifest}, paths,
				"the readable root and the 4.x history are still searched")
		})
	}
}

// Cancellation and a failing consumer still stop discovery at once instead
// of reading as an incomplete scan.
func TestCodeBuddyCBCDiscoveryStopsOnCancelOrYieldFailure(t *testing.T) {
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	first := filepath.Join(t.TempDir(), "projects")
	second := filepath.Join(t.TempDir(), "projects")
	writeCBC(t, filepath.Join(first, "-work-demo", "s1.jsonl"), line)
	writeCBC(t, filepath.Join(second, "-work-demo", "s2.jsonl"), line)
	set := newCodeBuddyCBCSourceSet([]string{first, second}, "")

	stop := errors.New("consumer failed")
	calls := 0
	err := set.DiscoverEach(t.Context(), func(SourceRef) error {
		calls++
		return stop
	})
	require.ErrorIs(t, err, stop)
	var incomplete DiscoveryIncompleteError
	assert.NotErrorAs(t, err, &incomplete)
	assert.Equal(t, 1, calls, "a failing consumer stops discovery")

	ctx, cancel := context.WithCancel(t.Context())
	calls = 0
	err = set.DiscoverEach(ctx, func(SourceRef) error {
		calls++
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorAs(t, err, &incomplete)
	assert.Equal(t, 1, calls, "cancellation stops discovery")
}
func TestCodeBuddyCBCSourcePathRejectsUnusableStems(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"-work-demo/run-1.jsonl", true},
		{"项目/run-1.jsonl", true},
		{"-work-demo/run 1.jsonl", false},
		{"-work-demo/agent-a.jsonl", false},
		{"-work-demo/run-1/subagents/agent-a.jsonl", true},
		{"-work-demo/run-1/subagents/helper.jsonl", false},
	} {
		assert.Equal(t, tc.want, codebuddyCBCSourcePath(root, filepath.Join(root, filepath.FromSlash(tc.rel))), tc.rel)
	}
}

// A remote sync archives paths in the sender's form, which can differ from the
// receiver's. An archived CLI/5.x transcript ranks above 4.x history in any of
// those forms, so an older copy cannot replace its later turns.
func TestCodeBuddyRankStoredSourcePathAcrossPathStyles(t *testing.T) {
	p, ok := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy}).
		NewProvider(ProviderConfig{}).(*codebuddyProvider)
	require.True(t, ok)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/home/u/.codebuddy/projects/-work-demo/conv1.jsonl", true},
		{"/home/u/.codebuddy/projects/-work-demo/conv1/subagents/agent-a.jsonl", true},
		{"host:/home/u/.codebuddy/projects/-work-demo/conv1.jsonl", true},
		{`C:\Users\u\.codebuddy\projects\-work-demo\conv1.jsonl`, true},
		{`C:\Users\u\.codebuddy\projects\-work-demo\conv1\subagents\agent-a.jsonl`, true},
		{`host:C:\Users\u\.codebuddy\projects\-work-demo\conv1.jsonl`, true},
		{"host:C:/Users/u/.codebuddy/projects/-work-demo/conv1.jsonl", true},
		{`\\server\share\.codebuddy\projects\-work-demo\conv1.jsonl`, true},
		{"/home/u/history/ws/conv1/index.json", false},
		{`host:C:\Users\u\history\ws\conv1\index.json`, false},
		{`C:\Users\u\.codebuddy\projects\-work-demo\conv1.json`, false},
		{`C:\Users\u\.codebuddy\projects\-work-demo\agent-a.jsonl`, false},
		{`C:\Users\u\.codebuddy\projects\-work-demo\conv1\notes\agent-a.jsonl`, false},
		{`C:\Users\u\.codebuddy\other\-work-demo\conv1.jsonl`, false},
	} {
		_, ranked := p.RankStoredSourcePath(tc.path)
		assert.Equal(t, tc.want, ranked, tc.path)
	}
}

// Discovery returns one copy of a conversation that several homes hold: the
// newest copy that holds a turn. Copies with no turn yet stay, so the 4.x
// history keeps showing until a transcript gains one.
func TestCodeBuddyDiscoverKeepsBestCopyOfConversation(t *testing.T) {
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	meta := `{"type":"session-meta","id":"m","sessionId":"conv1"}`
	for _, tc := range []struct {
		name         string
		newer, older string
		wantNewer    bool
		wantOlder    bool
	}{
		{name: "newer copy holds turns", newer: line, older: line, wantNewer: true},
		{name: "newer copy is empty", newer: meta, older: line, wantOlder: true},
		{name: "no copy holds a turn", newer: meta, older: meta, wantNewer: true, wantOlder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newerHome, newerProject := cbcHome(t)
			olderHome, olderProject := cbcHome(t)
			newer := filepath.Join(newerProject, "conv1.jsonl")
			older := filepath.Join(olderProject, "conv1.jsonl")
			agent := filepath.Join(olderProject, "conv1", "subagents", "agent-a.jsonl")
			writeCBC(t, newer, tc.newer)
			writeCBC(t, older, tc.older)
			writeCBC(t, agent, line)
			stale := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(older, stale, stale))
			manifest := filepath.Join(olderHome, "history", "ws", "conv1", "index.json")
			writeCBC(t, manifest, `{"messages":[]}`)

			p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
				NewProvider(ProviderConfig{Roots: []string{olderHome, newerHome}, Machine: "m"})
			sources, err := p.Discover(t.Context())
			require.NoError(t, err)
			var paths []string
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.Equal(t, tc.wantNewer, slices.Contains(paths, newer), "newer copy")
			assert.Equal(t, tc.wantOlder, slices.Contains(paths, older), "older copy")
			assert.Contains(t, paths, agent, "an agent transcript with one copy stays")
			assert.Contains(t, paths, manifest, "4.x manifests stay")
		})
	}
}

func TestCodeBuddyRootsSplitByLayout(t *testing.T) {
	both := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(both, "history"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(both, "projects"), 0o755))
	catalogOnly := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(catalogOnly, CodeBuddyCatalogDBName), nil, 0o600))
	legacyOnly := t.TempDir()
	projects := filepath.Join(t.TempDir(), "projects")

	legacy, cbc := splitCodeBuddyRoots([]string{both, catalogOnly, legacyOnly, projects})
	assert.Equal(t, []string{both, legacyOnly}, legacy)
	assert.Equal(t, []string{
		filepath.Join(both, "projects"), filepath.Join(catalogOnly, "projects"), projects,
	}, cbc)
}

func TestCodeBuddyConvertedSessionUsesCBCAndLegacyUsage(t *testing.T) {
	home, project := cbcHome(t)
	legacyRoot := t.TempDir()
	// history/<md5("/work/demo")>/<id>/
	convDir := filepath.Join(legacyRoot, "user", "CodeBuddyIDE", "history", "11dd9429831578384393182ae89b24ff", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "assets"), 0o755))
	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "assets", "pic.png"), png, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"u1","role":"user"},{"id":"a1","role":"assistant"},{"id":"a9","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt"}]}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"luna","lastStepInputTokens":90,"statsSnapshot":{"inputTokens":300,"outputTokens":40,"cachedInputTokens":200,"cacheWriteTokens":60,"thinkingTokens":10}},"message":{"content":[{"type":"text","text":"done"}]}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a9.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:09Z","extra":{"requestId":"req2","statsSnapshot":{"inputTokens":50,"outputTokens":5}},"message":{"content":[{"type":"text","text":"gone"}]}}`), 0o600))

	writeCBC(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"conv1-session-meta","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"},"cwd":"/work/demo"}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"cwd":"/work/demo","content":[{"type":"input_text","text":"old prompt"},{"type":"input_image","image":"codebuddy-asset://assets/pic.png"},{"type":"input_image","image":"codebuddy-asset://assets/../escape.png"}],"providerData":{"agent":"codebuddy-ide-4.x"}}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1"}}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"new prompt after upgrade"}],"providerData":{"agent":"craft"}}`,
	)
	results := parseCBCHome(t, home, legacyRoot)
	main := cbcSession(t, results, "codebuddy:conv1")
	assert.Equal(t, "ide-4.x", main.Session.Entrypoint)
	assert.Equal(t, 2, main.Session.UserMessageCount, "the CBC copy carries turns made after conversion")
	for _, r := range results {
		assert.NotEqual(t, filepath.Join(convDir, "index.json"), r.Session.File.Path, "the frozen 4.x manifest is not parsed again")
	}
	prompt := main.Messages[0].Content
	assert.Contains(t, prompt, "![image](asset://")
	assert.Contains(t, prompt, "[image unavailable: image]")

	reply := main.Messages[1]
	assert.Equal(t, "cb4:req1:a1", reply.SourceUUID)
	// inputTokens 300 = 200 cache reads + 60 cache writes + 40 uncached.
	assert.JSONEq(t, `{"input_tokens":40,"output_tokens":40,"cache_read_input_tokens":200,"cache_creation_input_tokens":60,"reasoning_tokens":10}`, string(reply.TokenUsage))
	assert.Equal(t, 90, reply.ContextTokens)

	var orphan *ParsedMessage
	for i := range main.Messages {
		if main.Messages[i].SourceUUID == "cb4:req2:a9" {
			orphan = &main.Messages[i]
		}
	}
	require.NotNil(t, orphan, "a request whose message the converter dropped still counts")
	assert.True(t, orphan.IsSystem)
	assert.Equal(t, 5, orphan.OutputTokens)
	assert.Equal(t, int64(50), gjson.GetBytes(orphan.TokenUsage, "input_tokens").Int())
}

func TestCodeBuddyConvertedTranscriptWithCopiedTotalsCountsOnce(t *testing.T) {
	home, project := cbcHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv2")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"a1","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"luna","lastStepInputTokens":90,"statsSnapshot":{"inputTokens":3000000,"outputTokens":40,"cachedInputTokens":2990000}}}`), 0o600))
	// Newer converters copy the request totals into the transcript.
	writeCBC(t, filepath.Join(project, "conv2.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv2","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1","model":"luna","lastStepInputTokens":90,"rawUsage":{"prompt_tokens":3000000,"completion_tokens":40,"prompt_cache_hit_tokens":2990000}}}`,
	)
	results := parseCBCHome(t, home, legacyRoot)
	main := cbcSession(t, results, "codebuddy:conv2")
	var usage []ParsedMessage
	for _, m := range main.Messages {
		if len(m.TokenUsage) > 0 {
			usage = append(usage, m)
		}
	}
	// The total's 2,990,000 cache reads pass the per-request bound, so the
	// reply carries the first part and one hidden row the rest.
	require.Len(t, usage, 2, "the copied total and the 4.x record are one response")
	assert.Equal(t, "cb4:req1:a1", usage[0].SourceUUID)
	assert.Equal(t, "cb4:req1:a1#2", usage[1].SourceUUID)
	assert.Equal(t, 40, usage[0].OutputTokens+usage[1].OutputTokens)
	assert.Equal(t, 90, usage[0].ContextTokens, "the request total sums every step; the context is the last step's prompt")
}

// FuzzCodeBuddyCBCTranscript feeds arbitrary bytes to the CBC transcript
// parser. Every session it returns must carry an ID and the agent, and the
// output must stay proportional to the input.
func FuzzCodeBuddyCBCTranscript(f *testing.F) {
	f.Add([]byte(`{"id":"u1","type":"message","role":"user","timestamp":1,"content":[{"type":"input_text","text":"hi"}]}
{"id":"a1","parentId":"u1","type":"function_call","name":"Bash","callId":"c","arguments":"{}","providerData":{"conversationRequestId":"r","messageId":"m","rawUsage":{"prompt_tokens":5,"completion_tokens":1}}}
{"id":"n1","type":"resend-fork-notice"}
{"id":"u2","type":"message","role":"user","content":"again"}
`))
	f.Add([]byte(`{"id":"x","parentId":"x","logicalParentId":"x","type":"message","role":"assistant","content":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		home, project := cbcHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(project, "fz.jsonl"), data, 0o600))
		results := parseCBCHome(t, home)
		total := 0
		for _, r := range results {
			assert.NotEmpty(t, r.Session.ID)
			assert.Equal(t, AgentCodeBuddy, r.Session.Agent)
			for _, m := range r.Messages {
				total += len(m.Content) + len(m.ThinkingText)
			}
		}
		assert.LessOrEqual(t, total, 16*len(data)+4096)
	})
}

// cbcTeamAssignment is a lead's first message to a team subagent: the
// team-context reminder followed by the task as a teammate message.
const cbcTeamAssignment = `[{"type":"input_text","text":"<system-reminder data-role=\"team-context\">\n# Agent Teammate Communication\n\nYou are **Explore-1**, a teammate in team **_auto_s9**.\n</system-reminder>"},{"type":"input_text","text":"<teammate-message teammate_id=\"team-lead\" summary=\"Initial task assignment for Explore-1\">\nMap the build scripts\n</teammate-message>"}]`

func TestCodeBuddyCBCTeamSubagentNames(t *testing.T) {
	for _, tc := range []struct {
		name, meta, agent, wantLabel string
	}{
		{"label from the producer's speaker", `{}`, "Explore-1", "Explore-1"},
		{"team member name wins", `{"teamContext":{"memberName":"reviewer","teamName":"t"}}`, "general-purpose", "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			pd := `{"conversationRequestId":"r1","rootRequestId":"root","agent":"` + tc.agent + `"}`
			writeCBC(t, filepath.Join(project, "s9", "subagents", "agent-x.jsonl"),
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":`+cbcTeamAssignment+`,"providerData":`+pd+`}`,
				`{"type":"session-meta","id":"m1","sessionId":"agent-x","meta":`+tc.meta+`}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"Two scripts."}],"providerData":`+pd+`}`,
				`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1002,"content":[{"type":"input_text","text":"<teammate-message teammate_id=\"team-lead\" summary=\"Follow-up\">\nAlso the tests\n</teammate-message>"}],"providerData":`+pd+`}`,
			)
			results := parseCBCHome(t, home)
			require.Len(t, results, 1)
			s := results[0].Session
			assert.Equal(t, RelSubagent, s.RelationshipType)
			assert.Equal(t, tc.wantLabel, s.AgentLabel)
			assert.Equal(t, "Map the build scripts", s.FirstMessage)
			assert.Equal(t, "Map the build scripts", s.SessionName)
			assert.Equal(t, 1, s.UserMessageCount)

			msgs := results[0].Messages
			require.Len(t, msgs, 4)
			assert.Equal(t, "system_reminder", msgs[0].SourceSubtype)
			assert.Contains(t, msgs[0].Content, "Agent Teammate Communication")
			assert.False(t, msgs[1].IsSystem, "the task assignment is the subagent's prompt")
			assert.Equal(t, "user", msgs[1].SourceType)
			assert.Contains(t, msgs[1].Content, "Map the build scripts")
			assert.True(t, msgs[3].IsSystem, "later lead messages stay peer messages")
			assert.Equal(t, "peer_message", msgs[3].SourceSubtype)
		})
	}
}

func TestCodeBuddyCBCTeammateMessagesOutsideAssignments(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s10.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"Split the work"}],"providerData":{"agent":"cli"}}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"Started a teammate."}],"providerData":{"agent":"cli"}}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1002,"content":[{"type":"input_text","text":"<teammate-message teammate_id=\"Explore-1\" summary=\"done\">\nFound two scripts\n</teammate-message>"}],"providerData":{"agent":"cli","teammateMessage":{"from":"Explore-1"}}}`,
	)
	writeCBC(t, filepath.Join(project, "s10", "subagents", "agent-y.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"Review the diff"}],"providerData":{"agent":"general-purpose"}}`,
		`{"id":"u2","parentId":"u1","type":"message","role":"user","timestamp":1001,"content":[{"type":"input_text","text":"<teammate-message teammate_id=\"team-lead\" summary=\"note\">\nskip docs\n</teammate-message>"}],"providerData":{"agent":"general-purpose"}}`,
	)
	results := parseCBCHome(t, home)
	main := cbcSession(t, results, "codebuddy:s10")
	assert.Empty(t, main.Session.AgentLabel, "a main session has no subagent name")
	assert.Equal(t, "Split the work", main.Session.FirstMessage)
	assert.Equal(t, 1, main.Session.UserMessageCount)
	require.Len(t, main.Messages, 3)
	assert.True(t, main.Messages[2].IsSystem)
	assert.Equal(t, "peer_message", main.Messages[2].SourceSubtype)

	child := cbcSession(t, results, "codebuddy:s10:subagent:agent-y")
	assert.Equal(t, "general-purpose", child.Session.AgentLabel)
	assert.Equal(t, "Review the diff", child.Session.FirstMessage)
	assert.Equal(t, 1, child.Session.UserMessageCount)
	require.Len(t, child.Messages, 2)
	assert.Equal(t, "peer_message", child.Messages[1].SourceSubtype,
		"a teammate message after a typed prompt is not an assignment")
}

// A conversation rewound before its first message has no current turns, but
// the history it replaced stays readable as a fork, and its side questions
// still attach to the conversation.
func TestCodeBuddyCBCRewoundBeforeFirstMessageKeepsHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name: "rewound history becomes a fork",
			lines: []string{
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-r","content":[{"type":"input_text","text":"first try"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"old answer"}]}`,
				`{"id":"n1","type":"resend-fork-notice","timestamp":1002}`,
			},
			want: []string{"codebuddy:run-r-u1", "codebuddy:run-r:btw"},
		},
		{
			name:  "metadata only yields nothing",
			lines: []string{`{"type":"session-meta","id":"m","sessionId":"run-r","meta":{}}`, `{"id":"n1","type":"resend-fork-notice","timestamp":1002}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			writeCBC(t, filepath.Join(project, "run-r.jsonl"), tc.lines...)
			writeCBCCatalog(t, home,
				`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('conv-r', 'Rewound', 'run-r')`,
				`INSERT INTO btw_entries VALUES ('conv-r', '[{"id":"b1","question":"side q","answer":"side a","createdAt":2000}]')`)
			t.Cleanup(clearCodeBuddyCatalogCache)
			results := parseCBCHome(t, home)
			var ids []string
			for _, r := range results {
				ids = append(ids, r.Session.ID)
				assert.Equal(t, "codebuddy:run-r", r.Session.ParentSessionID, r.Session.ID)
			}
			if len(tc.want) == 0 {
				assert.Empty(t, ids)
				return
			}
			assert.ElementsMatch(t, tc.want, ids, "the empty current session is left out so the fork stays visible")
			fork := cbcSession(t, results, "codebuddy:run-r-u1")
			assert.Equal(t, RelFork, fork.Session.RelationshipType)
			require.Len(t, fork.Messages, 2)
			assert.Equal(t, "old answer", fork.Messages[1].Content)
		})
	}
}

// Replacing a rewound second turn and then rewinding the first leaves two
// incompatible continuations of the same first turn. Each becomes its own
// fork that repeats the shared turn, and the shared response's usage keeps
// one identity so it counts once.
func TestCodeBuddyCBCDivergentBranchesSplit(t *testing.T) {
	home, project := cbcHome(t)
	usage := func(req string, out int) string {
		return `"providerData":{"conversationRequestId":"` + req + `","messageId":"m","stepSeq":1,"rawUsage":{"prompt_tokens":10,"completion_tokens":` + strconv.Itoa(out) + `}}`
	}
	writeCBC(t, filepath.Join(project, "s5.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"shared reply"}],`+usage("r1", 1)+`}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1002,"content":[{"type":"input_text","text":"second"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1003,"content":[{"type":"output_text","text":"second reply"}],`+usage("r2", 10)+`}`,
		`{"id":"n1","parentId":"a1","type":"resend-fork-notice","timestamp":1004}`,
		`{"id":"u3","parentId":"a1","type":"message","role":"user","timestamp":1005,"content":[{"type":"input_text","text":"second again"}]}`,
		`{"id":"a3","parentId":"u3","type":"message","role":"assistant","timestamp":1006,"content":[{"type":"output_text","text":"replacement reply"}],`+usage("r3", 100)+`}`,
		`{"id":"n2","type":"resend-fork-notice","timestamp":1007}`,
		`{"id":"u4","type":"message","role":"user","timestamp":1008,"content":[{"type":"input_text","text":"new first"}]}`,
		`{"id":"a4","parentId":"u4","type":"message","role":"assistant","timestamp":1009,"content":[{"type":"output_text","text":"new reply"}],`+usage("r4", 1000)+`}`,
	)
	results := parseCBCHome(t, home)
	require.Len(t, results, 3)
	texts := func(r ParseResult) []string {
		var out []string
		for _, m := range r.Messages {
			out = append(out, m.Content)
		}
		return out
	}
	assert.Equal(t, []string{"new first", "new reply"}, texts(cbcSession(t, results, "codebuddy:s5")))
	assert.Equal(t, []string{"first", "shared reply", "second", "second reply"},
		texts(cbcSession(t, results, "codebuddy:s5-u1")), "the first branch keeps its earlier id")
	assert.Equal(t, []string{"first", "shared reply", "second again", "replacement reply"},
		texts(cbcSession(t, results, "codebuddy:s5-u1-u3")))

	byIdentity := map[string]int{}
	for _, r := range results {
		for _, m := range r.Messages {
			if m.SourceUUID != "" && m.HasOutputTokens {
				byIdentity[m.SourceUUID] = m.OutputTokens
			}
		}
	}
	total := 0
	for _, out := range byIdentity {
		total += out
	}
	assert.Equal(t, 1111, total, "each response counts once across the forks")
}

// An agent call in a preserved branch opens the child transcript it started.
func TestCodeBuddyCBCForkLinksSubagentCalls(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s6.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"delegate"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1001,"name":"Agent","callId":"t1","arguments":"{\"prompt\":\"look\"}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1002,"name":"Agent","callId":"t1","output":"found it","providerData":{"toolResult":{"subAgent":{"sessionId":"agent-7"}}}}`,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1003}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1004,"content":[{"type":"input_text","text":"never mind"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1005,"content":[{"type":"output_text","text":"ok"}]}`,
	)
	writeCBC(t, filepath.Join(project, "s6", "subagents", "agent-7.jsonl"),
		`{"id":"su","type":"message","role":"user","timestamp":1001,"content":[{"type":"input_text","text":"look"}]}`,
		`{"id":"sa","parentId":"su","type":"message","role":"assistant","timestamp":1002,"content":[{"type":"output_text","text":"found it"}]}`,
	)
	results := parseCBCHome(t, home)
	fork := cbcSession(t, results, "codebuddy:s6-u1")
	require.Len(t, fork.Messages, 3)
	require.Len(t, fork.Messages[1].ToolCalls, 1)
	assert.Equal(t, "codebuddy:s6:subagent:agent-7", fork.Messages[1].ToolCalls[0].SubagentSessionID)
}
