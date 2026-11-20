package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/zstdframe"
)

// Fuzz targets for the providers that reuse or resemble the Claude and
// Codex transcript formats, plus the JSONL and JSON families that ship
// beside them.

const (
	claudeFamilyFuzzUUID     = "0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10"
	claudeFamilyFuzzRollout  = "2024/01/01/rollout-2024-01-01T10-00-00-" + claudeFamilyFuzzUUID + ".jsonl"
	coworkFuzzSessionDir     = "org/ws/local_50000000-0000-4000-8000-000000000001"
	coworkFuzzCLISessionID   = "c0000000-0000-4000-8000-000000000001"
	codeBuddyFuzzSessionDir  = "history/ws_hash/conv_1"
	kimiFuzzWorkspaceDir     = "wd_app_057f5c09ee3f"
	codexCloudFuzzMirrorPath = "account-key/thread-a.jsonl.zstd"
)

func init() {
	claudeSeeds := []providerFuzzSeed{
		{file: "claude/valid_session.jsonl", want: "Fix the login bug"},
		{file: "claude/tool_call_pending.jsonl", want: "Read auth.ts"},
	}

	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOpenClaude,
		path:  "-home-user-code-app/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: claudeSeeds,
	})
	// Cowork finds a transcript only through its local_<uuid>.json
	// metadata file, whose cliSessionId names the transcript.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCowork,
		path: coworkFuzzSessionDir + "/.claude/projects/-sessions-demo/" +
			coworkFuzzCLISessionID + ".jsonl",
		companions: map[string]string{
			coworkFuzzSessionDir + ".json": `{"sessionId":"local_50000000-0000-4000-8000-000000000001",` +
				`"cliSessionId":"` + coworkFuzzCLISessionID + `","title":"Demo session",` +
				`"userSelectedFolders":["/home/user/code/app"]}`,
		},
		seeds: claudeSeeds,
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentTraeX,
		path:  claudeFamilyFuzzRollout,
		seeds: []providerFuzzSeed{
			{file: "traex/history_mutation_session.jsonl", want: "Inspect the parser."},
			{file: "traex/rollout_subagent.jsonl", want: "Explore the parser package."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAugureCode,
		path:  claudeFamilyFuzzRollout,
		seeds: []providerFuzzSeed{
			{file: "augure/rollout_session.jsonl", want: "Summarize the provider facade."},
			{file: "codex/function_calls.jsonl", want: "Run the tests"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentInterpreter,
		path:  claudeFamilyFuzzRollout,
		seeds: []providerFuzzSeed{
			{file: "openinterpreter/rollout_session.jsonl", want: "List the notes directory."},
			{file: "codex/standard_session.jsonl", want: "Add rate limiting"},
		},
	})
	// Codex cloud mirrors are JSON lines inside zstd frames. Compressing
	// the input here keeps the fuzzer on the record parser instead of the
	// zstd decoder.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCodexCloud,
		place: func(t *testing.T, root string, data []byte) {
			t.Helper()
			frame, err := zstdframe.EncodeFrame(data)
			require.NoError(t, err)
			writeProviderFuzzFile(t,
				filepath.Join(root, filepath.FromSlash(codexCloudFuzzMirrorPath)), frame)
		},
		seeds: []providerFuzzSeed{
			{file: "codex-cloud/thread_session.jsonl", want: "Make the integration tests pass"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCommandCode,
		path:  "home-user-code-app/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "commandcode/v2_session.jsonl", want: "Inspect the server logs for startup errors"},
			{file: "commandcode/v3_session.jsonl", want: "Find why the build is slow"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentQwen,
		path:  "-home-user-code-app/chats/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "qwen/session.jsonl", want: "Find the failing test in the parser"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentIflow,
		path:  "app/session-" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "iflow/session-small.jsonl", want: "Show the version in the about page"},
			{file: "iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl", want: "DOCKER_API_VERSION"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentQoder,
		path:  "-home-user-code-app/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "qoder/session.jsonl", want: "Rename the config loader"},
			{file: "claude/valid_session.jsonl", want: "Fix the login bug"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentWorkBuddy,
		path:  "app/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: []providerFuzzSeed{
			{file: "workbuddy/session.jsonl", want: "Check which directory uses the most disk"},
		},
	})
	// A CodeBuddy session is an index.json manifest naming one JSON file
	// per message. The input is the first message file, the untrusted
	// transcript content; the manifest and a reply stay fixed.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCodeBuddy,
		path:  codeBuddyFuzzSessionDir + "/messages/m1.json",
		companions: map[string]string{
			codeBuddyFuzzSessionDir + "/index.json": `{"messages":[{"id":"m1"},{"id":"m2"}]}`,
			codeBuddyFuzzSessionDir + "/messages/m2.json": `{"role":"assistant",` +
				`"createdAt":"2026-08-03T09:00:09.000Z",` +
				`"message":{"content":[{"type":"text","text":"Done."}]}}`,
		},
		seeds: []providerFuzzSeed{
			{file: "codebuddy/user_message.json", want: "Add a health check endpoint"},
			{file: "codebuddy/assistant_tool_call.json", want: "server/routes.go"},
		},
	})
	// No run-state.json companion: its agentType is what turns a session
	// into Freebuff, which would fail the agent invariant by design.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCodebuff,
		path:  "app/chats/2026-07-15T20-01-32.065Z/chat-messages.json",
		seeds: []providerFuzzSeed{
			{file: "codebuff/chat-messages.json", want: "Add input validation to the signup form"},
		},
	})
	// IcodeMate has two storage families. This target drives the terminal
	// CLI's Claude-format projects root; the VS Code extension's OpenCode
	// storage shares the OpenCode format parser.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentIcodemate,
		path:  "app/" + claudeFamilyFuzzUUID + ".jsonl",
		seeds: claudeSeeds,
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentKimi,
		path:  kimiFuzzWorkspaceDir + "/session_" + claudeFamilyFuzzUUID + "/agents/main/wire.jsonl",
		seeds: []providerFuzzSeed{
			{file: "kimi/wire_session.jsonl", want: "List the Go files in this module"},
			{file: "kimi-work/wire_session.jsonl", want: "Summarize the meeting notes file"},
		},
	})
	// Kimi Work only reads conv-* session directories.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentKimiWork,
		path:  kimiFuzzWorkspaceDir + "/conv-" + claudeFamilyFuzzUUID + "/agents/main/wire.jsonl",
		seeds: []providerFuzzSeed{
			{file: "kimi-work/wire_session.jsonl", want: "Summarize the meeting notes file"},
			{file: "kimi/wire_session.jsonl", want: "List the Go files in this module"},
		},
	})
}

func FuzzProviderOpenClaude(f *testing.F) { fuzzProvider(f, AgentOpenClaude) }

func FuzzProviderCowork(f *testing.F) { fuzzProvider(f, AgentCowork) }

func FuzzProviderTraeX(f *testing.F) { fuzzProvider(f, AgentTraeX) }

func FuzzProviderAugureCode(f *testing.F) { fuzzProvider(f, AgentAugureCode) }

func FuzzProviderOpenInterpreter(f *testing.F) { fuzzProvider(f, AgentInterpreter) }

func FuzzProviderCodexCloud(f *testing.F) { fuzzProvider(f, AgentCodexCloud) }

func FuzzProviderCommandCode(f *testing.F) { fuzzProvider(f, AgentCommandCode) }

func FuzzProviderQwen(f *testing.F) { fuzzProvider(f, AgentQwen) }

func FuzzProviderIFlow(f *testing.F) { fuzzProvider(f, AgentIflow) }

func FuzzProviderQoder(f *testing.F) { fuzzProvider(f, AgentQoder) }

func FuzzProviderWorkBuddy(f *testing.F) { fuzzProvider(f, AgentWorkBuddy) }

func FuzzProviderCodeBuddy(f *testing.F) { fuzzProvider(f, AgentCodeBuddy) }

func FuzzProviderCodebuff(f *testing.F) { fuzzProvider(f, AgentCodebuff) }

func FuzzProviderIcodeMate(f *testing.F) { fuzzProvider(f, AgentIcodemate) }

func FuzzProviderKimi(f *testing.F) { fuzzProvider(f, AgentKimi) }

func FuzzProviderKimiWork(f *testing.F) { fuzzProvider(f, AgentKimiWork) }
