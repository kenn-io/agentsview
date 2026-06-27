package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVSCodeCopilotSession(t *testing.T) {
	tests := []struct {
		name         string
		json         string
		wantNil      bool
		wantMessages int
		wantTitle    string
		wantAgent    AgentType
		wantToolUse  bool
	}{
		{
			name:    "empty requests",
			json:    `{"version":3,"sessionId":"abc","requests":[]}`,
			wantNil: true,
		},
		{
			name: "single user+assistant turn",
			json: `{
				"version": 3,
				"sessionId": "test-123",
				"creationDate": 1755347684754,
				"lastMessageDate": 1755347728048,
				"customTitle": "Test session",
				"requests": [{
					"requestId": "req1",
					"message": {"text": "Hello world", "parts": []},
					"response": [
						{"value": "Hi there! ", "supportThemeIcons": false},
						{"value": "How can I help?", "supportThemeIcons": false}
					],
					"timestamp": 1755347728047,
					"modelId": "copilot/gpt-5"
				}]
			}`,
			wantMessages: 2,
			wantTitle:    "Hello world",
			wantAgent:    AgentVSCodeCopilot,
		},
		{
			name: "with tool invocations",
			json: `{
				"version": 3,
				"sessionId": "tools-456",
				"creationDate": 1755347684754,
				"lastMessageDate": 1755347728048,
				"customTitle": "Tool session",
				"requests": [{
					"requestId": "req1",
					"message": {"text": "Read the file", "parts": []},
					"response": [
						{"value": "Reading the file... "},
						{"kind": "prepareToolInvocation", "toolName": "copilot_readFile"},
						{"kind": "toolInvocationSerialized", "toolId": "copilot_readFile", "toolCallId": "tc1", "isConfirmed": true, "isComplete": true},
						{"value": "Done reading."}
					],
					"timestamp": 1755347728047,
					"modelId": "copilot/gpt-5"
				}]
			}`,
			wantMessages: 2,
			wantToolUse:  true,
		},
		{
			name: "multiple requests",
			json: `{
				"version": 3,
				"sessionId": "multi-789",
				"creationDate": 1755340000000,
				"lastMessageDate": 1755350000000,
				"customTitle": "Multi turn",
				"requests": [
					{
						"requestId": "req1",
						"message": {"text": "First question"},
						"response": [{"value": "First answer"}],
						"timestamp": 1755340000000
					},
					{
						"requestId": "req2",
						"message": {"text": "Second question"},
						"response": [{"value": "Second answer"}],
						"timestamp": 1755350000000
					}
				]
			}`,
			wantMessages: 4,
			wantTitle:    "First question",
		},
		{
			name: "no user text uses customTitle",
			json: `{
				"version": 3,
				"sessionId": "notitle-000",
				"creationDate": 1755340000000,
				"lastMessageDate": 1755340000000,
				"customTitle": "Fallback Title",
				"requests": [{
					"requestId": "req1",
					"message": {"text": ""},
					"response": [{"value": "Some response"}],
					"timestamp": 1755340000000
				}]
			}`,
			wantMessages: 1,
			wantTitle:    "Fallback Title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test-session.json")
			require.NoError(t, os.WriteFile(
				path, []byte(tt.json), 0644,
			))

			sess, msgs, err := parseVSCodeCopilotTestSession(t,
				path, "testproject", "local",
			)
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, sess, "expected nil session")
				return
			}

			require.NotNil(t, sess, "expected non-nil session")

			assert.Len(t, msgs, tt.wantMessages, "messages")

			if tt.wantTitle != "" {
				assert.Equal(t, tt.wantTitle, sess.FirstMessage, "first message")
			}

			if tt.wantAgent != "" {
				assert.Equal(t, tt.wantAgent, sess.Agent, "agent")
			}

			assert.Equal(t, "testproject", sess.Project, "project")

			if tt.wantToolUse {
				found := false
				for _, m := range msgs {
					if m.HasToolUse {
						found = true
						break
					}
				}
				assert.True(t, found, "expected tool use in messages")
			}
		})
	}
}

func TestParseVSCodeCopilotSession_NonExistent(t *testing.T) {
	sess, msgs, err := parseVSCodeCopilotTestSession(t,
		"/nonexistent/path.json", "proj", "local",
	)
	require.NoError(t, err, "expected nil error")
	assert.Nil(t, sess, "expected nil session for non-existent file")
	assert.Nil(t, msgs, "expected nil messages for non-existent file")
}

func TestParseVSCodeCopilotSession_MixedTextAndTools(t *testing.T) {
	data := `{
		"version": 3,
		"sessionId": "mixed-001",
		"creationDate": 1755340000000,
		"lastMessageDate": 1755340000000,
		"customTitle": "Mixed content",
		"requests": [{
			"requestId": "req1",
			"message": {"text": "Read the file"},
			"response": [
				{"kind": "toolInvocationSerialized", "toolId": "copilot_readFile", "toolCallId": "tc1", "isConfirmed": true, "isComplete": true, "pastTenseMessage": {"value": "Read main.go, lines 1 to 50"}},
				{"value": "Here is the file content."}
			],
			"timestamp": 1755340000000
		}]
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	require.NoError(t, os.WriteFile(path, []byte(data), 0644))

	_, msgs, err := parseVSCodeCopilotTestSession(t, path, "proj", "local")
	require.NoError(t, err)

	// Find assistant message
	var assistant *ParsedMessage
	for i := range msgs {
		if msgs[i].Role == RoleAssistant {
			assistant = &msgs[i]
			break
		}
	}
	require.NotNil(t, assistant, "no assistant message")

	assert.True(t, assistant.HasToolUse, "expected HasToolUse=true")

	// Content should include both tool markers and text
	assert.NotEmpty(t, assistant.Content, "expected non-empty content")

	// Tool calls should have InputJSON populated
	require.Len(t, assistant.ToolCalls, 1)
	tc := assistant.ToolCalls[0]
	assert.NotEmpty(t, tc.InputJSON, "expected non-empty InputJSON")
	assert.Equal(t, "Read", tc.Category, "category")
}

func TestParseVSCodeCopilotSession_TerminalToolData(t *testing.T) {
	data := `{
		"version": 3,
		"sessionId": "term-001",
		"creationDate": 1755340000000,
		"lastMessageDate": 1755340000000,
		"customTitle": "Terminal session",
		"requests": [{
			"requestId": "req1",
			"message": {"text": "Run tests"},
			"response": [
				{"kind": "toolInvocationSerialized", "toolId": "copilot_runInTerminal", "toolCallId": "tc1", "isConfirmed": true, "isComplete": true, "invocationMessage": "Using \"Run In Terminal\"", "toolSpecificData": {"kind": "terminal", "language": "sh", "command": "npm test"}}
			],
			"timestamp": 1755340000000
		}]
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	require.NoError(t, os.WriteFile(path, []byte(data), 0644))

	_, msgs, err := parseVSCodeCopilotTestSession(t, path, "proj", "local")
	require.NoError(t, err)

	var assistant *ParsedMessage
	for i := range msgs {
		if msgs[i].Role == RoleAssistant {
			assistant = &msgs[i]
			break
		}
	}
	require.NotNil(t, assistant, "no assistant message")

	require.Len(t, assistant.ToolCalls, 1)
	tc := assistant.ToolCalls[0]
	assert.Equal(t, "Bash", tc.Category, "category")
	assert.NotEmpty(t, tc.InputJSON, "expected non-empty InputJSON")

	// Content should include the command
	assert.Contains(t, assistant.Content, "npm test",
		"content should contain command, got: %s", assistant.Content)
}

func TestExtractProjectFromURI(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{"file:///Users/dev/projects/myapp", "myapp"},
		{"file:///home/user/code/repo", "repo"},
		{"file:///C:/Users/dev/projects/app", "app"},
		{"some-name", "some-name"},
	}
	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			assert.Equal(t, tt.want, extractProjectFromURI(tt.uri),
				"extractProjectFromURI(%q)", tt.uri)
		})
	}
}

func TestReadVSCodeWorkspaceManifest(t *testing.T) {
	dir := t.TempDir()

	// Valid workspace.json
	content := `{"folder":"file:///Users/dev/projects/agentsview"}`
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "workspace.json"),
		[]byte(content), 0644,
	))

	assert.Equal(t, "agentsview", ReadVSCodeWorkspaceManifest(dir))

	// Non-existent dir
	assert.Empty(t, ReadVSCodeWorkspaceManifest("/nonexistent"))
}

func TestDiscoverVSCodeCopilotSessions(t *testing.T) {
	root := t.TempDir()

	// Create workspace structure
	hash := "abc123def456"
	chatDir := filepath.Join(
		root, "workspaceStorage", hash, "chatSessions",
	)
	require.NoError(t, os.MkdirAll(chatDir, 0755))

	// workspace.json
	wsJSON := `{"folder":"file:///Users/dev/projects/myproject"}`
	wsPath := filepath.Join(
		root, "workspaceStorage", hash, "workspace.json",
	)
	require.NoError(t, os.WriteFile(wsPath, []byte(wsJSON), 0644))

	// Chat session file
	sessionJSON := `{"version":3,"sessionId":"sess1","requests":[{"requestId":"r1","message":{"text":"hi"},"response":[{"value":"hello"}],"timestamp":1755340000000}]}`
	sessPath := filepath.Join(chatDir, "sess1.json")
	require.NoError(t, os.WriteFile(sessPath, []byte(sessionJSON), 0644))

	// globalStorage/emptyWindowChatSessions
	globalDir := filepath.Join(
		root, "globalStorage", "emptyWindowChatSessions",
	)
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	globalPath := filepath.Join(globalDir, "global-sess.json")
	require.NoError(t, os.WriteFile(globalPath, []byte(sessionJSON), 0644))

	files := discoverVSCodeCopilotTestSessions(t, root)

	require.Len(t, files, 2)

	// Check workspace session
	var wsFile, globalFile DiscoveredFile
	for _, f := range files {
		switch f.Project {
		case "myproject":
			wsFile = f
		case "empty-window":
			globalFile = f
		}
	}

	assert.NotEmpty(t, wsFile.Path, "missing workspace session file")
	assert.Equal(t, AgentVSCodeCopilot, wsFile.Agent, "agent")

	assert.NotEmpty(t, globalFile.Path, "missing global session file")
}

func TestNormalizeVSCodeToolName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"copilot_readFile", "read_file"},
		{"copilot_replaceString", "edit_file"},
		{"copilot_runCommand", "shell"},
		{"copilot_searchFiles", "grep"},
		{"copilot_listDir", "glob"},
		{"copilot_createFile", "create_file"},
		{"copilot_runInTerminal", "shell"},
		{"copilot_getTerminalOutput", "shell"},
		{"copilot_findTextInFiles", "grep"},
		{"copilot_findFiles", "glob"},
		{"copilot_listDirectory", "glob"},
		{"copilot_applyPatch", "edit_file"},
		{"copilot_multiReplaceString", "edit_file"},
		{"copilot_fetchWebPage", "read_web_page"},
		{"copilot_think", "Tool"},
		{"runSubagent", "Task"},
		{"unknown_tool", "unknown_tool"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeVSCodeToolName(tt.input))
		})
	}
}

func TestExtractVSCopilotInputJSON(t *testing.T) {
	tests := []struct {
		name     string
		invMsg   string
		pastMsg  string
		toolData string
		wantKey  string
		wantVal  string
	}{
		{
			name:    "string invocation message",
			invMsg:  `"Using Run In Terminal"`,
			wantKey: "message",
			wantVal: "Using Run In Terminal",
		},
		{
			name:    "object invocation message",
			invMsg:  `{"value": "Reading file.txt, lines 1 to 50"}`,
			wantKey: "message",
			wantVal: "Reading file.txt, lines 1 to 50",
		},
		{
			name:    "prefers pastTenseMessage",
			invMsg:  `"Reading file..."`,
			pastMsg: `"Read file.txt, lines 1 to 50"`,
			wantKey: "message",
			wantVal: "Read file.txt, lines 1 to 50",
		},
		{
			name:     "terminal tool data",
			invMsg:   `"Using Run In Terminal"`,
			toolData: `{"kind":"terminal","language":"sh","command":"ls -la"}`,
			wantKey:  "command",
			wantVal:  "ls -la",
		},
		{
			name: "empty fields",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var inv, past, td json.RawMessage
			if tt.invMsg != "" {
				inv = json.RawMessage(tt.invMsg)
			}
			if tt.pastMsg != "" {
				past = json.RawMessage(tt.pastMsg)
			}
			if tt.toolData != "" {
				td = json.RawMessage(tt.toolData)
			}
			got := extractVSCopilotInputJSON(inv, past, td)

			if tt.wantKey == "" {
				assert.Empty(t, got, "expected empty")
				return
			}

			var m map[string]any
			err := json.Unmarshal([]byte(got), &m)
			require.NoError(t, err, "invalid JSON")
			val, ok := m[tt.wantKey].(string)
			assert.True(t, ok, "value not a string")
			assert.Equal(t, tt.wantVal, val, "value for key %q", tt.wantKey)
		})
	}
}

func TestParseVSCodeCopilotSession_JSONL(t *testing.T) {
	tests := []struct {
		name         string
		lines        []string
		wantNil      bool
		wantMessages int
		wantTitle    string
		wantToolUse  bool
	}{
		{
			name: "simple session with mutations",
			lines: []string{
				// kind=0: initial snapshot with empty requests
				`{"kind":0,"v":{"version":3,"sessionId":"jsonl-001","creationDate":1770650022790,"customTitle":"","requests":[],"responderUsername":"GitHub Copilot"}}`,
				// kind=1: set customTitle
				`{"kind":1,"k":["customTitle"],"v":"Test JSONL Session"}`,
				// kind=2: push a request
				`{"kind":2,"k":["requests"],"v":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"Hello JSONL","parts":[]},"response":[{"value":"Hi from JSONL!"}],"modelId":"copilot/gpt-4o"}]}`,
			},
			wantMessages: 2,
			wantTitle:    "Hello JSONL",
		},
		{
			name: "empty session no requests",
			lines: []string{
				`{"kind":0,"v":{"version":3,"sessionId":"jsonl-empty","creationDate":1770650022790,"requests":[]}}`,
			},
			wantNil: true,
		},
		{
			name: "session with tool calls",
			lines: []string{
				`{"kind":0,"v":{"version":3,"sessionId":"jsonl-tools","creationDate":1770650022790,"requests":[]}}`,
				`{"kind":2,"k":["requests"],"v":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"Read file","parts":[]},"response":[{"kind":"toolInvocationSerialized","toolId":"copilot_readFile","toolCallId":"tc1","isConfirmed":true,"isComplete":true},{"value":"Done."}],"modelId":"copilot/gpt-4o"}]}`,
			},
			wantMessages: 2,
			wantToolUse:  true,
			wantTitle:    "Read file",
		},
		{
			name: "multiple requests via push",
			lines: []string{
				`{"kind":0,"v":{"version":3,"sessionId":"jsonl-multi","creationDate":1770650022790,"requests":[]}}`,
				`{"kind":2,"k":["requests"],"v":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"First","parts":[]},"response":[{"value":"Answer 1"}],"modelId":"copilot/gpt-4o"}]}`,
				`{"kind":2,"k":["requests"],"v":[{"requestId":"req2","timestamp":1770650041889,"message":{"text":"Second","parts":[]},"response":[{"value":"Answer 2"}],"modelId":"copilot/gpt-4o"}]}`,
			},
			wantMessages: 4,
			wantTitle:    "First",
		},
		{
			name: "set mutation on response",
			lines: []string{
				`{"kind":0,"v":{"version":3,"sessionId":"jsonl-set","creationDate":1770650022790,"requests":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"Q","parts":[]},"response":[{"value":"partial"}],"modelId":"copilot/gpt-4o"}]}}`,
				// Update the first response item
				`{"kind":1,"k":["requests",0,"response",0],"v":{"value":"Complete answer"}}`,
			},
			wantMessages: 2,
			wantTitle:    "Q",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test-session.jsonl")

			content := strings.Join(tt.lines, "\n") + "\n"
			require.NoError(t, os.WriteFile(
				path, []byte(content), 0644,
			))

			sess, msgs, err := parseVSCodeCopilotTestSession(t,
				path, "testproject", "local",
			)
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, sess, "expected nil session")
				return
			}

			require.NotNil(t, sess, "expected non-nil session")

			assert.Len(t, msgs, tt.wantMessages, "messages")

			if tt.wantTitle != "" {
				assert.Equal(t, tt.wantTitle, sess.FirstMessage, "first message")
			}

			assert.Equal(t, AgentVSCodeCopilot, sess.Agent, "agent")

			if tt.wantToolUse {
				found := false
				for _, m := range msgs {
					if m.HasToolUse {
						found = true
						break
					}
				}
				assert.True(t, found, "expected tool use in messages")
			}
		})
	}
}

func TestReconstructJSONL(t *testing.T) {
	tests := []struct {
		name    string
		lines   []string
		wantErr bool
		check   func(t *testing.T, data []byte)
	}{
		{
			name: "initial only",
			lines: []string{
				`{"kind":0,"v":{"sessionId":"s1","version":3}}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				assert.Equal(t, "s1", m["sessionId"], "sessionId")
			},
		},
		{
			name: "set nested property",
			lines: []string{
				`{"kind":0,"v":{"a":{"b":"old"}}}`,
				`{"kind":1,"k":["a","b"],"v":"new"}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				a := m["a"].(map[string]any)
				assert.Equal(t, "new", a["b"])
			},
		},
		{
			name: "push to array",
			lines: []string{
				`{"kind":0,"v":{"items":["a"]}}`,
				`{"kind":2,"k":["items"],"v":["b","c"]}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				items := m["items"].([]any)
				require.Len(t, items, 3, "len")
				assert.Equal(t, "c", items[2], "items[2]")
			},
		},
		{
			name: "push with splice index",
			lines: []string{
				`{"kind":0,"v":{"items":["a","c"]}}`,
				`{"kind":2,"k":["items"],"v":["b"],"i":1}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				items := m["items"].([]any)
				require.Len(t, items, 3, "len")
				assert.Equal(t, []any{"a", "b", "c"}, items, "items")
			},
		},
		{
			name: "push with negative splice index",
			lines: []string{
				`{"kind":0,"v":{"items":["a","b"]}}`,
				`{"kind":2,"k":["items"],"v":["z"],"i":-1}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				items := m["items"].([]any)
				require.Len(t, items, 3, "len")
				// Negative index clamped to 0: inserted at front
				assert.Equal(t, "z", items[0], "items[0]")
			},
		},
		{
			name: "delete property",
			lines: []string{
				`{"kind":0,"v":{"a":"keep","b":"remove"}}`,
				`{"kind":3,"k":["b"]}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				_, ok := m["b"]
				assert.False(t, ok, "expected b to be deleted")
				assert.Equal(t, "keep", m["a"], "a")
			},
		},
		{
			name: "set array element by index",
			lines: []string{
				`{"kind":0,"v":{"arr":["x","y","z"]}}`,
				`{"kind":1,"k":["arr",1],"v":"Y"}`,
			},
			check: func(t *testing.T, data []byte) {
				var m map[string]any
				require.NoError(t, json.Unmarshal(data, &m))
				arr := m["arr"].([]any)
				assert.Equal(t, "Y", arr[1], "arr[1]")
			},
		},
		{
			name:  "empty file returns nil",
			lines: []string{},
			check: func(t *testing.T, data []byte) {
				assert.Nil(t, data, "expected nil")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.jsonl")

			content := strings.Join(tt.lines, "\n") + "\n"
			require.NoError(t, os.WriteFile(
				path, []byte(content), 0644,
			))

			data, err := reconstructJSONL(path)
			if tt.wantErr {
				require.Error(t, err, "expected error")
				return
			}
			require.NoError(t, err)
			tt.check(t, data)
		})
	}
}

func TestDiscoverVSCodeCopilot_JSONLDedup(t *testing.T) {
	root := t.TempDir()

	hash := "abc123def456"
	chatDir := filepath.Join(
		root, "workspaceStorage", hash, "chatSessions",
	)
	require.NoError(t, os.MkdirAll(chatDir, 0755))

	wsJSON := `{"folder":"file:///Users/dev/projects/myproject"}`
	wsPath := filepath.Join(
		root, "workspaceStorage", hash, "workspace.json",
	)
	require.NoError(t, os.WriteFile(wsPath, []byte(wsJSON), 0644))

	// Session with both .json and .jsonl - jsonl should win
	sessionJSON := `{"version":3,"sessionId":"dup1","requests":[{"requestId":"r1","message":{"text":"hi"},"response":[{"value":"hello"}],"timestamp":1755340000000}]}`
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "dup1.json"),
		[]byte(sessionJSON), 0644,
	))
	jsonlContent := `{"kind":0,"v":{"version":3,"sessionId":"dup1","creationDate":1755340000000,"requests":[{"requestId":"r1","timestamp":1755340000000,"message":{"text":"hi"},"response":[{"value":"hello"}]}]}}` + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "dup1.jsonl"),
		[]byte(jsonlContent), 0644,
	))

	// Session with only .jsonl
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "only-jsonl.jsonl"),
		[]byte(jsonlContent), 0644,
	))

	// Session with only .json
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "only-json.json"),
		[]byte(sessionJSON), 0644,
	))

	files := discoverVSCodeCopilotTestSessions(t, root)

	// Should get 3 files: dup1.jsonl, only-jsonl.jsonl, only-json.json
	if !assert.Len(t, files, 3, "expected 3 files") {
		for _, f := range files {
			t.Logf("  %s", f.Path)
		}
		t.FailNow()
	}

	// Verify dup1.json was excluded (dup1.jsonl present)
	for _, f := range files {
		assert.NotEqual(t, "dup1.json", filepath.Base(f.Path),
			"dup1.json should be excluded when dup1.jsonl exists")
	}
}

func TestFindVSCodeCopilotSourceFile(t *testing.T) {
	dir := t.TempDir()
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Set up a workspace session file
	chatDir := filepath.Join(
		dir, "workspaceStorage", "hash1", "chatSessions",
	)
	sessionPath := filepath.Join(chatDir, uuid+".json")
	require.NoError(t, os.MkdirAll(chatDir, 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}"), 0o644))

	tests := []struct {
		name string
		dir  string
		id   string
		want string
	}{
		{"valid UUID", dir, uuid, sessionPath},
		{"empty dir", "", uuid, ""},
		{"empty ID", dir, "", ""},
		{"traversal slash", dir, "../etc/passwd", ""},
		{"traversal dotdot", dir, "..", ""},
		{"path separator", dir, "foo/bar", ""},
		{"nonexistent UUID", dir, "00000000-0000-0000-0000-000000000000", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findVSCodeCopilotTestSourceFile(t,
				tt.dir, tt.id,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseVSCodeCopilotSession_TokenUsage(t *testing.T) {
	// Two turns carry result.metadata token accounting (the
	// post-credits VSCode Copilot format); a third turn has no
	// metadata and must not produce a usage event.
	sessionJSON := `{
		"version": 3,
		"sessionId": "usage-1",
		"creationDate": 1755340000000,
		"lastMessageDate": 1755350000000,
		"requests": [
			{
				"requestId": "r1",
				"message": {"text": "First"},
				"response": [{"value": "answer one"}],
				"timestamp": 1755340000000,
				"modelId": "copilot/claude-opus-4.8",
				"result": {"metadata": {
					"promptTokens": 35875,
					"outputTokens": 221,
					"resolvedModel": "claude-opus-4-8"
				}}
			},
			{
				"requestId": "r2",
				"message": {"text": "Second"},
				"response": [{"value": "answer two"}],
				"timestamp": 1755345000000,
				"modelId": "copilot/claude-opus-4.8",
				"result": {"metadata": {
					"promptTokens": 41055,
					"outputTokens": 69,
					"resolvedModel": "claude-opus-4-8"
				}}
			},
			{
				"requestId": "r3",
				"message": {"text": "Third"},
				"response": [{"value": "answer three"}],
				"timestamp": 1755350000000,
				"modelId": "copilot/claude-opus-4.8"
			}
		]
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	require.NoError(t, os.WriteFile(path, []byte(sessionJSON), 0644))

	sess, _, err := parseVSCodeCopilotTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Only the two turns with metadata yield usage events.
	require.Len(t, sess.UsageEvents, 2, "usage events")

	for i, ev := range sess.UsageEvents {
		assert.Equal(t, "vscode-copilot", ev.Source, "event[%d] source", i)
		assert.Equal(t, "claude-opus-4-8", ev.Model, "event[%d] model", i)
		assert.NotEmpty(t, ev.OccurredAt, "event[%d] occurredAt", i)
	}

	assert.Equal(t, 35875, sess.UsageEvents[0].InputTokens)
	assert.Equal(t, 221, sess.UsageEvents[0].OutputTokens)
	assert.Equal(t, 41055, sess.UsageEvents[1].InputTokens)
	assert.Equal(t, 69, sess.UsageEvents[1].OutputTokens)

	// Session output total sums the per-turn output tokens.
	assert.True(t, sess.HasTotalOutputTokens, "has total output")
	assert.Equal(t, 290, sess.TotalOutputTokens, "total output")

	// Peak context is the largest per-turn promptTokens.
	assert.True(t, sess.HasPeakContextTokens, "has peak context")
	assert.Equal(t, 41055, sess.PeakContextTokens, "peak context")
}

func TestParseVSCodeCopilotSession_TokenUsageModelFallback(t *testing.T) {
	// No resolvedModel: fall back to the prefixed modelId and
	// normalize the claude version dots to hyphens.
	sessionJSON := `{
		"version": 3,
		"sessionId": "usage-2",
		"creationDate": 1755340000000,
		"requests": [{
			"requestId": "r1",
			"message": {"text": "Hi"},
			"response": [{"value": "Hello"}],
			"timestamp": 1755340000000,
			"modelId": "copilot/claude-sonnet-4.6",
			"result": {"metadata": {"promptTokens": 1000, "outputTokens": 50}}
		}]
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "usage2.json")
	require.NoError(t, os.WriteFile(path, []byte(sessionJSON), 0644))

	sess, _, err := parseVSCodeCopilotTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	require.Len(t, sess.UsageEvents, 1)
	assert.Equal(t, "claude-sonnet-4-6", sess.UsageEvents[0].Model)
}

func TestParseVSCodeCopilotSession_NoTokenUsage(t *testing.T) {
	// A session whose requests carry no token metadata yields no
	// usage events and leaves output totals unset (cost -> n/a).
	sessionJSON := `{
		"version": 3,
		"sessionId": "no-usage",
		"creationDate": 1755340000000,
		"requests": [{
			"requestId": "r1",
			"message": {"text": "Hi"},
			"response": [{"value": "Hello"}],
			"timestamp": 1755340000000
		}]
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "nousage.json")
	require.NoError(t, os.WriteFile(path, []byte(sessionJSON), 0644))

	sess, _, err := parseVSCodeCopilotTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Empty(t, sess.UsageEvents, "no usage events expected")
	assert.False(t, sess.HasTotalOutputTokens, "no total output expected")
	assert.False(t, sess.HasPeakContextTokens, "no peak context expected")
}
