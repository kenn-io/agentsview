package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Provider fuzz targets for editor-hosted agents: Copilot CLI, Gemini,
// Antigravity, Cursor, VS Code and Visual Studio Copilot, Windsurf, Trae,
// Positron, Posit Assistant, Roo Code, Cline, and Kilo (legacy).

const providerFuzzEditorsSessionID = "0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10"

// providerFuzzPositAssistantTree is a conversation.json message tree whose
// active branch reads lm-messages 0 through 5 and whose abandoned branch
// reads 6, so the fuzzed lm-messages.jsonl is walked as Posit Assistant
// walks it.
const providerFuzzPositAssistantTree = `{"schemaVersion":"3",` +
	`"root":{"id":"11111111","title":"Sales data loading","timestamp":1735689600000,` +
	`"metadata":{"kind":"main","modeState":{},"gitBranch":"main"}},` +
	`"messages":[` +
	`{"id":"m1","parentId":"11111111","isActive":true,"lmMessageIds":[0],"timestamp":1735689600000},` +
	`{"id":"m2","parentId":"m1","isActive":true,"lmMessageIds":[1,2,3],"timestamp":1735689610000},` +
	`{"id":"m3","parentId":"m2","isActive":true,"lmMessageIds":[4],"timestamp":1735689630000},` +
	`{"id":"m4","parentId":"m3","isActive":true,"lmMessageIds":[5],"timestamp":1735689640000},` +
	`{"id":"m5","parentId":"m3","isActive":false,"lmMessageIds":[6],"timestamp":1735689650000}` +
	`],"files":[]}`

func init() {
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCopilot,
		path:  "session-state/" + providerFuzzEditorsSessionID + "/events.jsonl",
		seeds: []providerFuzzSeed{
			{file: "copilot/basic.jsonl", want: "Fix the login bug"},
			{file: "copilot/tool_calls.jsonl", want: "The config file contains a key-value pair."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentGemini,
		path:  "tmp/app/chats/session-2024-01-01T10-00-0e4ba1a2.json",
		seeds: []providerFuzzSeed{
			{file: "gemini/standard_session.json", want: "Fix the login bug"},
			{file: "gemini/tool_calls_with_results.json", want: "Read the planning doc"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAntigravity,
		place: placeAntigravityIDEFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "antigravity/assistant_reply.pb", want: "The retry loop now backs off exponentially."},
			{file: "antigravity/tool_call.pb", want: "I will read the file for you"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAntigravityCLI,
		place: placeAntigravityCLIFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "antigravity-cli/user_prompt.pb", want: "Summarize the open pull requests for this repo."},
			{file: "antigravity-cli/user_request_envelope.pb", want: "What does the sync engine do on conflict?"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCursor,
		path:  "fuzz-project/agent-transcripts/" + providerFuzzEditorsSessionID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "cursor/tool_use.jsonl", want: "Fix the failing build in main.go."},
			{file: "cursor/legacy-timestamps.txt", want: "Inspect the repository."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCursorIDE,
		place: placeCursorIDEFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "cursor-ide/user_bubble.json", want: "Rename the config loader and update its callers."},
			{file: "cursor-ide-object-tool-result.json", want: "TODO_STATUS_IN_PROGRESS"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentVSCodeCopilot,
		path:  "workspaceStorage/abc123/chatSessions/" + providerFuzzEditorsSessionID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "vscode-copilot/basic.jsonl", want: "Cap the backoff at thirty seconds"},
			{file: "vscode-copilot/tools.jsonl", want: "Read test.txt and run its command."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentVSCopilot,
		path:  "20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
		seeds: []providerFuzzSeed{
			{file: "visualstudio-copilot/chat_turn.jsonl", want: "Inspect the project and run tests."},
			{file: "visualstudio-copilot/tool_span.jsonl", want: "make lint"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentWindsurf,
		companions: map[string]string{
			"workspaceStorage/workspace-hash/workspace.json": `{"folder":"file:///home/user/code/app"}`,
		},
		place: placeWindsurfFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "windsurf/requests.json", want: "How do I add a new provider?"},
			{file: "windsurf/tabs.json", want: "Why does the watcher test flake?"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentTrae,
		companions: map[string]string{
			"workspaceStorage/hash/workspace.json": `{"folder":"file:///home/user/code/app"}`,
		},
		place: placeTraeFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "trae/session.json", want: "Add pagination to the users endpoint"},
			{file: "trae/two_sessions.json", want: "Why is the cache cold after restart?"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentPositron,
		path:  "workspaceStorage/abc123/chatSessions/" + providerFuzzEditorsSessionID + ".json",
		seeds: []providerFuzzSeed{
			{file: "positron/session.json", want: "Help me reshape this data frame to long format"},
			{file: "positron/terminal_tool.json", want: "Run the R unit tests"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentPositAssistant,
		path:  "a1b2c3d4/11111111/lm-messages.jsonl",
		companions: map[string]string{
			"a1b2c3d4/11111111/conversation.json": providerFuzzPositAssistantTree,
		},
		seeds: []providerFuzzSeed{
			{
				file: "posit-assistant/workspaces/a1b2c3d4/11111111/lm-messages.jsonl",
				want: "How do I load the sales data?",
			},
			{
				file: "posit-assistant/workspaces/default/33333333/lm-messages.jsonl",
				want: "Hi! How can I help?",
			},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentRooCode,
		path:  "tasks/" + providerFuzzEditorsSessionID + "/ui_messages.json",
		companions: map[string]string{
			"tasks/" + providerFuzzEditorsSessionID + "/history_item.json": `{"id":"` +
				providerFuzzEditorsSessionID + `","number":1,"ts":1688836851000,` +
				`"task":"Fuzz task","tokensIn":1200,"tokensOut":80,"totalCost":0.002,` +
				`"workspace":"/home/user/code/app","mode":"code","status":"completed"}`,
		},
		seeds: []providerFuzzSeed{
			{file: "roocode/ui_messages.json", want: "I'll start by reading the form component."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCline,
		path:  "data/sessions/1789000000000_abcde/1789000000000_abcde.messages.json",
		companions: map[string]string{
			"data/sessions/1789000000000_abcde/1789000000000_abcde.json": `{"version":1,` +
				`"session_id":"1789000000000_abcde","source":"cli",` +
				`"started_at":"2026-09-10T10:00:00.000Z","ended_at":"2026-09-10T10:05:00.000Z",` +
				`"status":"completed","model":"deepseek/deepseek-v4-flash",` +
				`"cwd":"/home/user/code/app","prompt":"Fuzz prompt",` +
				`"metadata":{"title":"Fuzz prompt"}}`,
		},
		seeds: []providerFuzzSeed{
			{file: "cline/sample_session/sample_session.messages.json", want: "Reading the spectrometer optics file..."},
			{file: "cline/command_error.messages.json", want: "error: missing DATABASE_URL"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentKiloLegacy,
		path:  "tasks/" + providerFuzzEditorsSessionID + "/ui_messages.json",
		companions: map[string]string{
			"tasks/" + providerFuzzEditorsSessionID + "/task_metadata.json":            `{"files_in_context":[]}`,
			"tasks/" + providerFuzzEditorsSessionID + "/api_conversation_history.json": `[]`,
		},
		seeds: []providerFuzzSeed{
			{file: "kilo-legacy/ui_messages.json", want: "Review the new parser implementation"},
		},
	})
}

// placeAntigravityIDEFuzzInput writes an Antigravity IDE conversation
// database with a fixed user step and the fuzz input as the step_payload of
// the assistant step that follows it. step_payload is the protobuf the
// parser walks; the rest of the row is schema bookkeeping.
func placeAntigravityIDEFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	dir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	db := openProviderFuzzSQLite(t, filepath.Join(dir, providerFuzzEditorsSessionID+".db"))
	defer db.Close()
	createAntigravityStepTables(t, db)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 14, antigravityTestStepPayload(1779000000, "Add retries to the push loop"))
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		1, 17, data)
}

// placeAntigravityCLIFuzzInput writes an Antigravity CLI conversation
// database whose only step is a user step carrying the fuzz input as its
// step_payload. With no history or brain transcript the CLI parser selects
// the SQLite steps, so the payload is what it decodes.
func placeAntigravityCLIFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	dir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	db := openProviderFuzzSQLite(t, filepath.Join(dir, providerFuzzEditorsSessionID+".db"))
	defer db.Close()
	createAntigravityStepTables(t, db)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 14, data)
}

// placeCursorIDEFuzzInput writes a Cursor IDE state.vscdb holding one
// composer whose single bubble row is the fuzz input. Bubbles carry the
// transcript text and tool calls; the composer document only lists them.
func placeCursorIDEFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	if data == nil {
		data = []byte{}
	}
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        providerFuzzEditorsSessionID,
		name:      "Fuzz composer",
		createdAt: 1779000000000,
		updatedAt: 1779000100000,
		cwd:       "/home/user/code/app",
		bubbles:   []cursorIDETestBubble{{id: "fuzz-bubble", bubbleType: 1, raw: data}},
	}})
	require.NoError(t, os.Rename(dbPath, filepath.Join(root, CursorIDEDBRelPath)))
}

// placeWindsurfFuzzInput writes a Windsurf workspace state.vscdb whose
// chat-data ItemTable value is the fuzz input. That value is the JSON
// document holding every chat in the workspace.
func placeWindsurfFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	writeWindsurfStateDB(t,
		filepath.Join(root, "workspaceStorage", "workspace-hash", windsurfStateDBName),
		string(data))
}

// placeTraeFuzzInput writes a Trae workspace state.vscdb whose agent
// storage ItemTable value is the fuzz input, beside an unrelated key the
// parser must ignore.
func placeTraeFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	writeTraeDB(t,
		filepath.Join(root, "workspaceStorage", "hash", traeStateDBName),
		string(data), "memento/unrelated-chat-storage")
}

func FuzzProviderCopilot(f *testing.F) { fuzzProvider(f, AgentCopilot) }

func FuzzProviderGemini(f *testing.F) { fuzzProvider(f, AgentGemini) }

func FuzzProviderAntigravity(f *testing.F) { fuzzProvider(f, AgentAntigravity) }

func FuzzProviderAntigravityCLI(f *testing.F) { fuzzProvider(f, AgentAntigravityCLI) }

func FuzzProviderCursor(f *testing.F) { fuzzProvider(f, AgentCursor) }

func FuzzProviderCursorIDE(f *testing.F) { fuzzProvider(f, AgentCursorIDE) }

func FuzzProviderVSCodeCopilot(f *testing.F) { fuzzProvider(f, AgentVSCodeCopilot) }

func FuzzProviderVisualStudioCopilot(f *testing.F) { fuzzProvider(f, AgentVSCopilot) }

func FuzzProviderWindsurf(f *testing.F) { fuzzProvider(f, AgentWindsurf) }

func FuzzProviderTrae(f *testing.F) { fuzzProvider(f, AgentTrae) }

func FuzzProviderPositron(f *testing.F) { fuzzProvider(f, AgentPositron) }

func FuzzProviderPositAssistant(f *testing.F) { fuzzProvider(f, AgentPositAssistant) }

func FuzzProviderRooCode(f *testing.F) { fuzzProvider(f, AgentRooCode) }

func FuzzProviderCline(f *testing.F) { fuzzProvider(f, AgentCline) }

func FuzzProviderKiloLegacy(f *testing.F) { fuzzProvider(f, AgentKiloLegacy) }
