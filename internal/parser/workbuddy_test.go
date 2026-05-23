package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDiscoverWorkBuddySessions(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	files := DiscoverWorkBuddySessions(root)
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Path != mainPath || files[0].Project != "proj" || files[0].Agent != AgentWorkBuddy {
		t.Fatalf("files[0] = %+v", files[0])
	}
	if files[1].Path != subPath || files[1].Project != "proj" || files[1].Agent != AgentWorkBuddy {
		t.Fatalf("files[1] = %+v", files[1])
	}
}

func TestParseWorkBuddySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"u1","timestamp":1778749186168,"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":"/tmp/proj"}
{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20,"outputTokens":4,"cacheReadInputTokens":5}}}
{"id":"fc1","timestamp":1778749188168,"type":"function_call","name":"Bash","callId":"call_1","arguments":"{\"command\":\"pwd\"}","providerData":{"model":"gpt-5.5","usage":{"inputTokens":10,"outputTokens":3,"cacheReadInputTokens":2}}}
{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":"/tmp/proj"}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, msgs, err := ParseWorkBuddySession(path, "proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("session nil")
	}
	if sess.ID != "workbuddy:11111111-1111-4111-8111-111111111111" {
		t.Fatalf("ID = %q", sess.ID)
	}
	if sess.Project != "proj" {
		t.Fatalf("Project = %q", sess.Project)
	}
	if sess.Cwd != "/tmp/proj" || sess.FirstMessage != "hello" || sess.UserMessageCount != 1 {
		t.Fatalf("session = %+v", sess)
	}
	if !sess.HasTotalOutputTokens || sess.TotalOutputTokens != 7 {
		t.Fatalf("TotalOutputTokens = %d known=%v", sess.TotalOutputTokens, sess.HasTotalOutputTokens)
	}
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d", len(msgs))
	}
	if gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int() != 20 {
		t.Fatalf("assistant message TokenUsage = %s", string(msgs[1].TokenUsage))
	}
	if msgs[2].Role != RoleAssistant || !msgs[2].HasToolUse || msgs[2].ToolCalls[0].ToolName != "Bash" {
		t.Fatalf("tool call msg = %+v", msgs[2])
	}
	if msgs[2].ToolCalls[0].InputJSON != `{"command":"pwd"}` {
		t.Fatalf("InputJSON = %q", msgs[2].ToolCalls[0].InputJSON)
	}
	if gjson.GetBytes(msgs[2].TokenUsage, "input_tokens").Int() != 10 {
		t.Fatalf("TokenUsage = %s", string(msgs[2].TokenUsage))
	}
	if msgs[3].Role != RoleUser || msgs[3].ToolResults[0].ToolUseID != "call_1" {
		t.Fatalf("tool result msg = %+v", msgs[3])
	}
}

func TestParseWorkBuddySessionDoesNotDoubleCountOpenAICachedTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","rawUsage":{"prompt_tokens":20,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, msgs, err := ParseWorkBuddySession(path, "proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("session nil")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d", len(msgs))
	}
	if !msgs[0].HasContextTokens || msgs[0].ContextTokens != 20 {
		t.Fatalf("ContextTokens = %d known=%v", msgs[0].ContextTokens, msgs[0].HasContextTokens)
	}
	if got := gjson.GetBytes(msgs[0].TokenUsage, "input_tokens").Int(); got != 15 {
		t.Fatalf("input_tokens = %d, want 15; usage=%s", got, string(msgs[0].TokenUsage))
	}
	if got := gjson.GetBytes(msgs[0].TokenUsage, "cache_read_input_tokens").Int(); got != 5 {
		t.Fatalf("cache_read_input_tokens = %d, want 5; usage=%s", got, string(msgs[0].TokenUsage))
	}
}

func TestParseWorkBuddySessionUsesCwdProjectAndFileSessionID(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "stored-project", "22222222-2222-4222-8222-222222222222.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-git working directory under a controlled temp root so
	// ExtractProjectFromCwd falls through to the basename and applies
	// NormalizeName (hyphen -> underscore) deterministically.
	cwd := filepath.Join(tmp, "code", "cwd-project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":%q}
`, cwd)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := ParseWorkBuddySession(path, "stored-project", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "workbuddy:22222222-2222-4222-8222-222222222222" {
		t.Fatalf("ID = %q", sess.ID)
	}
	if sess.Project != "cwd_project" {
		t.Fatalf("Project = %q, want cwd_project", sess.Project)
	}
}

func TestParseWorkBuddySessionNormalizesWindowsCwdProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stored", "33333333-3333-4333-8333-333333333333.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"33333333-3333-4333-8333-333333333333","cwd":"C:\\Users\\alice\\projects\\report-builder"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := ParseWorkBuddySession(path, "stored", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != "report_builder" {
		t.Fatalf("Project = %q, want report_builder", sess.Project)
	}
}

func TestParseWorkBuddySessionFallsBackToDiscoveredProjectWhenCwdHasNoProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovered-proj", "44444444-4444-4444-8444-444444444444.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"44444444-4444-4444-8444-444444444444","cwd":"/"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := ParseWorkBuddySession(path, "discovered-proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != "discovered-proj" {
		t.Fatalf("Project = %q, want discovered-proj", sess.Project)
	}
}

func TestParseWorkBuddySessionOmitsAbsentTokenUsageKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, msgs, err := ParseWorkBuddySession(path, "proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d", len(msgs))
	}
	m := msgs[0]
	if m.HasOutputTokens {
		t.Fatalf("HasOutputTokens = true, want false; usage=%s", string(m.TokenUsage))
	}
	if gjson.GetBytes(m.TokenUsage, "output_tokens").Exists() {
		t.Fatalf("output_tokens key present, want absent; usage=%s", string(m.TokenUsage))
	}
	if got := gjson.GetBytes(m.TokenUsage, "input_tokens").Int(); got != 20 {
		t.Fatalf("input_tokens = %d, want 20; usage=%s", got, string(m.TokenUsage))
	}
	// The DB coverage backfill re-derives presence from JSON keys, so
	// an absent output field must not be inferred as output coverage.
	_, hasOutput := InferTokenPresence(m.TokenUsage, m.ContextTokens, m.OutputTokens, false, false)
	if hasOutput {
		t.Fatalf("InferTokenPresence inferred output coverage for input-only usage; usage=%s", string(m.TokenUsage))
	}
}

func TestParseWorkBuddySubagentSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"sub task"}],"cwd":"/tmp/cwd-project"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := ParseWorkBuddySession(path, "proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "workbuddy:11111111-1111-4111-8111-111111111111:subagent:agent-123" {
		t.Fatalf("ID = %q", sess.ID)
	}
	if sess.ParentSessionID != "workbuddy:11111111-1111-4111-8111-111111111111" || sess.RelationshipType != RelSubagent {
		t.Fatalf("relationship = %q parent=%q", sess.RelationshipType, sess.ParentSessionID)
	}
}

func TestFindWorkBuddySourceFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := FindWorkBuddySourceFile(root, "workbuddy:11111111-1111-4111-8111-111111111111")
	if got != path {
		t.Fatalf("got %q, want %q", got, path)
	}
}

func TestFindWorkBuddySourceFileRejectsInvalidSubagentID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := FindWorkBuddySourceFile(root, "workbuddy:11111111-1111-4111-8111-111111111111:subagent:../agent-123")
	if got != "" {
		t.Fatalf("got %q, want empty path", got)
	}
}

func TestParseWorkBuddyProjectNamedSubagentsIsNotSubagent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}]}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, _, err := ParseWorkBuddySession(path, "subagents", "local")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "workbuddy:11111111-1111-4111-8111-111111111111" {
		t.Fatalf("ID = %q", sess.ID)
	}
	if sess.ParentSessionID != "" || sess.RelationshipType != RelNone {
		t.Fatalf("relationship = %q parent=%q", sess.RelationshipType, sess.ParentSessionID)
	}
}

func TestParseWorkBuddySessionDecodesObjectToolResultText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":"/tmp/proj"}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, msgs, err := ParseWorkBuddySession(path, "proj", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolResults) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if got := DecodeContent(msgs[0].ToolResults[0].ContentRaw); got != "/tmp/proj" {
		t.Fatalf("DecodeContent = %q, want %q; ContentRaw=%s",
			got, "/tmp/proj", msgs[0].ToolResults[0].ContentRaw)
	}
}
