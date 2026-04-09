package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePositronSession(t *testing.T) {
	// Create a minimal Positron session JSON
	sessionJSON := `{
		"version": 3,
		"requesterUsername": "testuser",
		"responderUsername": "Positron Assistant",
		"sessionId": "test-session-123",
		"creationDate": 1700000000000,
		"lastMessageDate": 1700001000000,
		"requests": [
			{
				"requestId": "req-1",
				"message": {
					"text": "Hello, help me with R code",
					"parts": []
				},
				"response": [
					{
						"value": "I can help you with R code."
					}
				],
				"timestamp": 1700000000000
			},
			{
				"requestId": "req-2",
				"message": {
					"text": "How do I load a CSV?",
					"parts": []
				},
				"response": [
					{
						"kind": "toolInvocationSerialized",
						"toolId": "copilot_readFile",
						"toolCallId": "call-1",
						"isComplete": true
					},
					{
						"value": "Use read.csv() function."
					}
				],
				"timestamp": 1700001000000
			}
		]
	}`

	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, "test-session.json")
	if err := os.WriteFile(
		sessionPath, []byte(sessionJSON), 0644,
	); err != nil {
		t.Fatal(err)
	}

	sess, msgs, err := ParsePositronSession(
		sessionPath, "test-project", "test-machine",
	)
	if err != nil {
		t.Fatalf("ParsePositronSession failed: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}

	// Verify session metadata
	if sess.Agent != AgentPositron {
		t.Errorf("agent = %v, want %v", sess.Agent, AgentPositron)
	}
	if sess.ID != "positron:test-session-123" {
		t.Errorf("ID = %v, want positron:test-session-123", sess.ID)
	}
	if sess.Project != "test-project" {
		t.Errorf("project = %v, want test-project", sess.Project)
	}
	if sess.FirstMessage != "Hello, help me with R code" {
		t.Errorf(
			"firstMessage = %v, want 'Hello, help me with R code'",
			sess.FirstMessage,
		)
	}

	// Verify messages
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4", len(msgs))
	}

	// First user message
	if msgs[0].Role != RoleUser {
		t.Errorf("msgs[0].Role = %v, want user", msgs[0].Role)
	}
	if msgs[0].Content != "Hello, help me with R code" {
		t.Errorf("msgs[0].Content = %v", msgs[0].Content)
	}

	// First assistant response
	if msgs[1].Role != RoleAssistant {
		t.Errorf("msgs[1].Role = %v, want assistant", msgs[1].Role)
	}

	// Second assistant should have tool use
	if !msgs[3].HasToolUse {
		t.Error("msgs[3] should have tool use")
	}
}

func TestDiscoverPositronSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directory structure:
	// <tmpDir>/workspaceStorage/<hash>/chatSessions/<uuid>.json
	// <tmpDir>/workspaceStorage/<hash>/workspace.json
	hashDir := filepath.Join(
		tmpDir, "workspaceStorage", "abc123hash",
	)
	chatDir := filepath.Join(hashDir, "chatSessions")
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create workspace.json
	wsJSON := `{"folder": "file:///Users/test/myproject"}`
	if err := os.WriteFile(
		filepath.Join(hashDir, "workspace.json"),
		[]byte(wsJSON),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	// Create session files
	sessionJSON := `{"version": 3, "requests": []}`
	for _, name := range []string{
		"session-1.json",
		"session-2.jsonl",
	} {
		if err := os.WriteFile(
			filepath.Join(chatDir, name),
			[]byte(sessionJSON),
			0644,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Create a non-session file that should be ignored
	if err := os.WriteFile(
		filepath.Join(chatDir, "readme.txt"),
		[]byte("ignore me"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	files := DiscoverPositronSessions(tmpDir)
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}

	for _, f := range files {
		if f.Agent != AgentPositron {
			t.Errorf("agent = %v, want positron", f.Agent)
		}
		if f.Project != "myproject" {
			t.Errorf("project = %v, want myproject", f.Project)
		}
	}
}

func TestFindPositronSourceFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directory structure
	hashDir := filepath.Join(
		tmpDir, "workspaceStorage", "abc123hash",
	)
	chatDir := filepath.Join(hashDir, "chatSessions")
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create session file
	sessionPath := filepath.Join(chatDir, "test-uuid.json")
	if err := os.WriteFile(
		sessionPath, []byte(`{}`), 0644,
	); err != nil {
		t.Fatal(err)
	}

	// Test finding existing session
	found := FindPositronSourceFile(tmpDir, "test-uuid")
	if found != sessionPath {
		t.Errorf("found = %v, want %v", found, sessionPath)
	}

	// Test finding non-existent session
	notFound := FindPositronSourceFile(tmpDir, "nonexistent")
	if notFound != "" {
		t.Errorf("expected empty string, got %v", notFound)
	}
}
