package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOpenClawTestFile creates a test JSONL file inside an
// agent directory structure: <root>/<agentId>/sessions/<name>.jsonl.
// Returns the full path to the file and the root agents directory.
func writeOpenClawTestFile(
	t *testing.T, agentID string, lines ...string,
) (path, agentsDir string) {
	t.Helper()
	root := t.TempDir()
	sessDir := filepath.Join(root, agentID, "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(sessDir, "test-session.jsonl")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	content := b.String()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path, root
}

func TestParseOpenClawSession_Basic(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/home/user/project"}`,
		`{"type":"model_change","id":"mc1","timestamp":"2026-02-25T10:00:00Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello, how are you?"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"I'm doing well, thanks!"}],"timestamp":"2026-02-25T10:00:02Z"}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test-machine")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
		return
	}

	if sess.ID != "openclaw:main:abc-123" {
		t.Errorf("expected ID openclaw:main:abc-123, got %s", sess.ID)
	}
	if sess.Agent != AgentOpenClaw {
		t.Errorf("expected agent openclaw, got %s", sess.Agent)
	}
	if sess.Machine != "test-machine" {
		t.Errorf("expected machine test-machine, got %s", sess.Machine)
	}
	if sess.Project != "project" {
		t.Errorf("expected project 'project', got %s", sess.Project)
	}
	if sess.FirstMessage != "Hello, how are you?" {
		t.Errorf("expected first message 'Hello, how are you?', got %s", sess.FirstMessage)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != RoleUser {
		t.Errorf("expected first role user, got %s", msgs[0].Role)
	}
	if msgs[1].Role != RoleAssistant {
		t.Errorf("expected second role assistant, got %s", msgs[1].Role)
	}
	if sess.UserMessageCount != 1 {
		t.Errorf("expected 1 user message, got %d", sess.UserMessageCount)
	}
}

func TestParseOpenClawSession_Thinking(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"think-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Think about this"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."},{"type":"text","text":"Here is my response."}],"timestamp":"2026-02-25T10:00:02Z"}}`,
	)

	_, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if !msgs[1].HasThinking {
		t.Error("expected HasThinking=true for assistant message")
	}
}

func TestParseOpenClawSession_ToolResult(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"tool-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Read a file"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"read","input":{"path":"/etc/hosts"}}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		`{"type":"message","id":"m3","timestamp":"2026-02-25T10:00:03Z","message":{"role":"toolResult","toolCallId":"tu1","toolName":"read","content":[{"type":"text","text":"127.0.0.1 localhost"}],"isError":false,"timestamp":"2026-02-25T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-02-25T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"The hosts file contains localhost."}],"timestamp":"2026-02-25T10:00:04Z"}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	// Assistant with tool_use
	if !msgs[1].HasToolUse {
		t.Error("expected HasToolUse=true for tool-use message")
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msgs[1].ToolCalls))
	}
	if msgs[1].ToolCalls[0].ToolName != "read" {
		t.Errorf("expected tool name 'read', got %s", msgs[1].ToolCalls[0].ToolName)
	}
	if msgs[1].ToolCalls[0].Category != "Read" {
		t.Errorf("expected category 'Read', got %s", msgs[1].ToolCalls[0].Category)
	}

	// Tool result mapped to user role
	if msgs[2].Role != RoleUser {
		t.Errorf("expected tool result as user role, got %s", msgs[2].Role)
	}
	if len(msgs[2].ToolResults) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(msgs[2].ToolResults))
	}
	if msgs[2].ToolResults[0].ToolUseID != "tu1" {
		t.Errorf("expected tool use ID 'tu1', got %s", msgs[2].ToolResults[0].ToolUseID)
	}
	if sess.MessageCount != 4 {
		t.Errorf("expected 4 messages, got %d", sess.MessageCount)
	}

	// UserMessageCount should only count the real user message,
	// not the synthetic tool-result message.
	if sess.UserMessageCount != 1 {
		t.Errorf("expected UserMessageCount 1 (tool results excluded), got %d", sess.UserMessageCount)
	}
}

func TestParseOpenClawSession_OrphanToolResult(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"orphan-tr","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"read","input":{}}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		// toolResult with empty toolCallId — should be dropped
		`{"type":"message","id":"m3","timestamp":"2026-02-25T10:00:03Z","message":{"role":"toolResult","toolCallId":"","toolName":"read","content":[{"type":"text","text":"orphan result"}],"isError":false,"timestamp":"2026-02-25T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-02-25T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-02-25T10:00:04Z"}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	// 3 messages: user, assistant (tool_use), assistant (text).
	// The orphan toolResult is skipped entirely.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if sess.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", sess.MessageCount)
	}
	if sess.UserMessageCount != 1 {
		t.Errorf("UserMessageCount = %d, want 1", sess.UserMessageCount)
	}
	for _, m := range msgs {
		if m.Role == RoleUser && m.Content == "" {
			t.Error("blank user message leaked through")
		}
	}
}

func TestParseOpenClawSession_EmptyFile(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"empty","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
	)

	sess, _, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess != nil {
		t.Error("expected nil session for file with no messages")
	}
}

func TestParseOpenClawSession_AssistantUsage(t *testing.T) {
	// Synthetic fixture covering the OpenClaw assistant-turn usage
	// shape: per-message provider/model and a usage block with
	// short-name token counts plus a nested cost object.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"usage-1","timestamp":"2026-04-30T12:00:00Z","cwd":"/home/user/proj"}`,
		`{"type":"model_change","id":"mc1","timestamp":"2026-04-30T12:00:00Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"do a thing"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-sonnet-4-6","usage":{"input":3,"output":91,"cacheRead":0,"cacheWrite":9612,"totalTokens":9706,"cost":{"input":0.000009,"output":0.001365,"cacheRead":0,"cacheWrite":0.036045,"total":0.037419}}}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	a := msgs[1]
	if a.Role != RoleAssistant {
		t.Fatalf("expected assistant role, got %s", a.Role)
	}
	if a.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", a.Model)
	}
	if a.OutputTokens != 91 {
		t.Errorf("OutputTokens = %d, want 91", a.OutputTokens)
	}
	if !a.HasOutputTokens {
		t.Error("HasOutputTokens = false, want true")
	}
	// ContextTokens = input + cacheRead + cacheWrite.
	if a.ContextTokens != 9615 {
		t.Errorf("ContextTokens = %d, want 9615", a.ContextTokens)
	}
	if !a.HasContextTokens {
		t.Error("HasContextTokens = false, want true")
	}
	// TokenUsage must be normalized to Anthropic-style keys so
	// downstream usage aggregation (internal/db/usage.go) can
	// read input_tokens/output_tokens/cache_*_input_tokens.
	if len(a.TokenUsage) == 0 {
		t.Fatal("TokenUsage empty, want normalized JSON")
	}
	tu := string(a.TokenUsage)
	for _, want := range []string{
		`"input_tokens":3`,
		`"output_tokens":91`,
		`"cache_read_input_tokens":0`,
		`"cache_creation_input_tokens":9612`,
	} {
		if !strings.Contains(tu, want) {
			t.Errorf("TokenUsage %q missing %q", tu, want)
		}
	}

	// Session-level rollup must reflect the per-message totals.
	if !sess.HasTotalOutputTokens {
		t.Error("sess.HasTotalOutputTokens = false, want true")
	}
	if sess.TotalOutputTokens != 91 {
		t.Errorf("TotalOutputTokens = %d, want 91",
			sess.TotalOutputTokens)
	}
	if !sess.HasPeakContextTokens {
		t.Error("sess.HasPeakContextTokens = false, want true")
	}
	if sess.PeakContextTokens != 9615 {
		t.Errorf("PeakContextTokens = %d, want 9615",
			sess.PeakContextTokens)
	}
}

func TestParseOpenClawSession_AssistantUsageWithoutCost(t *testing.T) {
	// Older sessions may carry a usage block without the nested
	// cost object. Token extraction must still succeed and not
	// crash on the missing field.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"usage-2","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-haiku-4-5","usage":{"input":42,"output":17,"cacheRead":0,"cacheWrite":0,"totalTokens":59}}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	a := msgs[1]
	if a.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", a.Model)
	}
	if a.OutputTokens != 17 {
		t.Errorf("OutputTokens = %d, want 17", a.OutputTokens)
	}
	if a.ContextTokens != 42 {
		t.Errorf("ContextTokens = %d, want 42", a.ContextTokens)
	}
	if len(a.TokenUsage) == 0 {
		t.Error("TokenUsage empty, want normalized JSON")
	}
	if sess.TotalOutputTokens != 17 {
		t.Errorf("TotalOutputTokens = %d, want 17",
			sess.TotalOutputTokens)
	}
}

func TestParseOpenClawSession_PartialUsage(t *testing.T) {
	// Partial usage block: only output is present in the source.
	// applyOpenClawAssistantUsage normalizes to a 4-key JSON, but
	// HasContextTokens must still be false. TokenPresence() must
	// trust the parser's explicit flags rather than inferring from
	// the always-populated normalized keys.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"partial","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-haiku-4-5","usage":{"output":17}}}`,
	)

	_, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	a := msgs[1]
	if a.HasContextTokens {
		t.Error("HasContextTokens = true, want false")
	}
	if !a.HasOutputTokens {
		t.Error("HasOutputTokens = false, want true")
	}

	hasCtx, hasOut := a.TokenPresence()
	if hasCtx {
		t.Error("TokenPresence ctx = true, want false " +
			"(parser flags must take precedence over JSON keys)")
	}
	if !hasOut {
		t.Error("TokenPresence out = false, want true")
	}
}

func TestParseOpenClawSession_NoUsage(t *testing.T) {
	// Assistant turn without any usage block: the parser is still
	// authoritative — both presence flags must be false and stick.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"nousage","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"timestamp":"2026-04-30T12:00:02Z"}}`,
	)

	_, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	hasCtx, hasOut := msgs[1].TokenPresence()
	if hasCtx || hasOut {
		t.Errorf("TokenPresence = (%v, %v), want (false, false)",
			hasCtx, hasOut)
	}
}

func TestParseOpenClawSession_Compaction(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"compact","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"compaction","id":"c1","timestamp":"2026-02-25T10:00:01Z","summary":"Previous work summary"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:02Z","message":{"role":"user","content":[{"type":"text","text":"Continue from here"}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"Continuing..."}],"timestamp":"2026-02-25T10:00:03Z"}}`,
	)

	sess, msgs, err := ParseOpenClawSession(path, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	// Compaction should be skipped, only messages remain.
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages (compaction skipped), got %d", len(msgs))
	}
}

func TestParseOpenClawSession_AgentIDInSessionID(t *testing.T) {
	// Verify different agent subdirectories produce distinct
	// session IDs even when the raw session ID is the same.
	pathA, _ := writeOpenClawTestFile(t, "alpha",
		`{"type":"session","version":3,"id":"same-id","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
	)
	pathB, _ := writeOpenClawTestFile(t, "beta",
		`{"type":"session","version":3,"id":"same-id","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
	)

	sessA, _, err := ParseOpenClawSession(pathA, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	sessB, _, err := ParseOpenClawSession(pathB, "", "test")
	if err != nil {
		t.Fatal(err)
	}

	if sessA.ID == sessB.ID {
		t.Errorf("expected different session IDs for different agents, both got %s", sessA.ID)
	}
	if sessA.ID != "openclaw:alpha:same-id" {
		t.Errorf("expected openclaw:alpha:same-id, got %s", sessA.ID)
	}
	if sessB.ID != "openclaw:beta:same-id" {
		t.Errorf("expected openclaw:beta:same-id, got %s", sessB.ID)
	}
}

func TestIsOpenClawSessionFile(t *testing.T) {
	accepted := []string{
		"abc.jsonl",
		"abc.jsonl.deleted.2026-02-19T08-59-24.951Z",
		"abc.jsonl.reset.2026-02-17T09-39-39.691Z",
		"abc.jsonl.full.bak",
	}
	rejected := []string{
		"abc.jsonl.tmp",
		"abc.jsonl.lock",
		"abc.jsonl.partial",
		"abc.json",
		"sessions.json",
	}
	for _, name := range accepted {
		if !IsOpenClawSessionFile(name) {
			t.Errorf("expected %q to be accepted", name)
		}
	}
	for _, name := range rejected {
		if IsOpenClawSessionFile(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func TestBestOpenClawEntry_CrossSuffix(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// reset is newer (March) than deleted (January), even though
	// "deleted" > "reset" would be wrong lexicographically within
	// the suffix family.
	older := "abc.jsonl.deleted.2026-01-15T00-00-00.000Z"
	newer := "abc.jsonl.reset.2026-03-01T00-00-00.000Z"
	for _, name := range []string{older, newer} {
		if err := os.WriteFile(
			filepath.Join(sessDir, name), []byte("{}"), 0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	files := DiscoverOpenClawSessions(root)
	if len(files) != 1 {
		t.Fatalf("expected 1 (deduplicated), got %d", len(files))
	}
	if filepath.Base(files[0].Path) != newer {
		t.Errorf("expected %q, got %q", newer, filepath.Base(files[0].Path))
	}
}

func TestDiscoverOpenClawSessions(t *testing.T) {
	// Build a mock directory structure:
	// <root>/main/sessions/sess1.jsonl
	// <root>/main/sessions/sessions.json
	// <root>/claude/sessions/sess2.jsonl
	root := t.TempDir()

	mainSessions := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(mainSessions, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainSessions, "sess1.jsonl"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainSessions, "sessions.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	claudeSessions := filepath.Join(root, "claude", "sessions")
	if err := os.MkdirAll(claudeSessions, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSessions, "sess2.jsonl"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	files := DiscoverOpenClawSessions(root)
	if len(files) != 2 {
		t.Fatalf("expected 2 session files, got %d", len(files))
	}
	for _, f := range files {
		if f.Agent != AgentOpenClaw {
			t.Errorf("expected agent openclaw, got %s", f.Agent)
		}
	}
}

func TestDiscoverOpenClawSessions_DeduplicatesArchived(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Active file and two archived files for the same session.
	for _, name := range []string{
		"abc.jsonl",
		"abc.jsonl.deleted.2026-02-19T08-59-24.951Z",
		"abc.jsonl.reset.2026-02-17T09-39-39.691Z",
	} {
		if err := os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	files := DiscoverOpenClawSessions(root)
	if len(files) != 1 {
		t.Fatalf("expected 1 file (deduplicated), got %d", len(files))
	}
	// Active file should win.
	if !strings.HasSuffix(files[0].Path, "abc.jsonl") {
		t.Errorf(
			"expected active .jsonl to win, got %s",
			filepath.Base(files[0].Path),
		)
	}
}

func TestDiscoverOpenClawSessions_ArchiveOnlyPicksNewest(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Two archived files, no active — newest filename wins.
	for _, name := range []string{
		"xyz.jsonl.deleted.2026-01-01T00-00-00.000Z",
		"xyz.jsonl.deleted.2026-03-01T00-00-00.000Z",
	} {
		if err := os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	files := DiscoverOpenClawSessions(root)
	if len(files) != 1 {
		t.Fatalf("expected 1 file (deduplicated), got %d", len(files))
	}
	want := "xyz.jsonl.deleted.2026-03-01T00-00-00.000Z"
	if filepath.Base(files[0].Path) != want {
		t.Errorf("expected newest archive %q, got %q",
			want, filepath.Base(files[0].Path))
	}
}

func TestDiscoverOpenClawSessions_DifferentSessionsNotDeduped(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Two different session IDs — should not be deduplicated.
	for _, name := range []string{
		"aaa.jsonl",
		"bbb.jsonl.deleted.2026-01-01T00-00-00.000Z",
	} {
		if err := os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	files := DiscoverOpenClawSessions(root)
	if len(files) != 2 {
		t.Fatalf("expected 2 files (different sessions), got %d",
			len(files))
	}
}

func TestFindOpenClawSourceFile(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sessDir, "abc-123.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Raw ID is now "agentId:sessionId".
	found := FindOpenClawSourceFile(root, "main:abc-123")
	if found != target {
		t.Errorf("expected %s, got %s", target, found)
	}

	// Non-existent session.
	notFound := FindOpenClawSourceFile(root, "main:nonexistent")
	if notFound != "" {
		t.Errorf("expected empty string, got %s", notFound)
	}

	// Non-existent agent.
	notFound2 := FindOpenClawSourceFile(root, "other:abc-123")
	if notFound2 != "" {
		t.Errorf("expected empty string, got %s", notFound2)
	}

	// Invalid format (no colon separator).
	notFound3 := FindOpenClawSourceFile(root, "abc-123")
	if notFound3 != "" {
		t.Errorf("expected empty string for bare ID, got %s", notFound3)
	}
}

func TestFindOpenClawSourceFile_ArchiveOnly(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Only archived files exist — no active .jsonl.
	archived := "def-456.jsonl.deleted.2026-02-19T08-59-24.951Z"
	if err := os.WriteFile(
		filepath.Join(sessDir, archived),
		[]byte("{}"), 0644,
	); err != nil {
		t.Fatal(err)
	}

	found := FindOpenClawSourceFile(root, "main:def-456")
	want := filepath.Join(sessDir, archived)
	if found != want {
		t.Errorf("expected %s, got %s", want, found)
	}
}

func TestFindOpenClawSourceFile_PrefersActiveOverArchive(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Both active and archived files exist.
	active := filepath.Join(sessDir, "ghi-789.jsonl")
	if err := os.WriteFile(active, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	archived := "ghi-789.jsonl.deleted.2026-02-19T00-00-00.000Z"
	if err := os.WriteFile(
		filepath.Join(sessDir, archived),
		[]byte("{}"), 0644,
	); err != nil {
		t.Fatal(err)
	}

	found := FindOpenClawSourceFile(root, "main:ghi-789")
	if found != active {
		t.Errorf("expected active file %s, got %s", active, found)
	}
}

func TestFindOpenClawSourceFile_ArchiveOnlyNewest(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Two archived files — newest should be chosen.
	old := "jkl.jsonl.deleted.2026-01-01T00-00-00.000Z"
	newest := "jkl.jsonl.deleted.2026-03-01T00-00-00.000Z"
	for _, name := range []string{old, newest} {
		if err := os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	found := FindOpenClawSourceFile(root, "main:jkl")
	want := filepath.Join(sessDir, newest)
	if found != want {
		t.Errorf("expected newest archive %s, got %s", want, found)
	}
}
