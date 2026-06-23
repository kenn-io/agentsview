package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPositronTestSourceSet(roots ...string) positronSourceSet {
	return newPositronSourceSet(roots)
}

func TestPositronProviderParseSession(t *testing.T) {
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
	require.NoError(t, os.WriteFile(
		sessionPath, []byte(sessionJSON), 0644,
	))

	p := &positronProvider{}
	sess, msgs, err := p.parseSession(
		sessionPath, "test-project", "test-machine",
	)
	require.NoError(t, err, "parseSession failed")
	require.NotNil(t, sess, "expected session, got nil")

	// Verify session metadata
	assert.Equal(t, AgentPositron, sess.Agent)
	assert.Equal(t, "positron:test-session-123", sess.ID)
	assert.Equal(t, "test-project", sess.Project)
	assert.Equal(t, "Hello, help me with R code", sess.FirstMessage)

	// Verify messages
	require.Len(t, msgs, 4)

	// First user message
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Hello, help me with R code", msgs[0].Content)

	// First assistant response
	assert.Equal(t, RoleAssistant, msgs[1].Role)

	// Second assistant should have tool use
	assert.True(t, msgs[3].HasToolUse, "msgs[3] should have tool use")
}

func TestPositronSourceSetDiscoverSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directory structure:
	// <tmpDir>/workspaceStorage/<hash>/chatSessions/<uuid>.json
	// <tmpDir>/workspaceStorage/<hash>/workspace.json
	hashDir := filepath.Join(
		tmpDir, "workspaceStorage", "abc123hash",
	)
	chatDir := filepath.Join(hashDir, "chatSessions")
	require.NoError(t, os.MkdirAll(chatDir, 0755))

	// Create workspace.json
	wsJSON := `{"folder": "file:///Users/test/myproject"}`
	require.NoError(t, os.WriteFile(
		filepath.Join(hashDir, "workspace.json"),
		[]byte(wsJSON),
		0644,
	))

	// Create session files. The .json file with a .jsonl sibling must be
	// deduped so full discovery matches changed-path sync precedence.
	sessionJSON := `{"version": 3, "requests": []}`
	for _, name := range []string{
		"session-1.json",
		"session-2.jsonl",
		"session-2.json",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(chatDir, name),
			[]byte(sessionJSON),
			0644,
		))
	}

	// Create a non-session file that should be ignored
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "readme.txt"),
		[]byte("ignore me"),
		0644,
	))

	set := newPositronTestSourceSet(tmpDir)
	files := set.discoverSessions(tmpDir)
	require.Len(t, files, 2)

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, filepath.Base(f.Path))
		assert.Equal(t, AgentPositron, f.Agent)
		assert.Equal(t, "myproject", f.Project)
	}
	assert.ElementsMatch(t, []string{"session-1.json", "session-2.jsonl"}, paths)
}

func TestPositronSourceSetFindSourceFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directory structure
	hashDir := filepath.Join(
		tmpDir, "workspaceStorage", "abc123hash",
	)
	chatDir := filepath.Join(hashDir, "chatSessions")
	require.NoError(t, os.MkdirAll(chatDir, 0755))

	// Create session file
	sessionPath := filepath.Join(chatDir, "test-uuid.json")
	require.NoError(t, os.WriteFile(
		sessionPath, []byte(`{}`), 0644,
	))

	set := newPositronTestSourceSet(tmpDir)

	// Test finding existing session
	found := set.findSourceFile(tmpDir, "test-uuid")
	assert.Equal(t, sessionPath, found)

	// Test finding non-existent session
	notFound := set.findSourceFile(tmpDir, "nonexistent")
	assert.Empty(t, notFound)
}
