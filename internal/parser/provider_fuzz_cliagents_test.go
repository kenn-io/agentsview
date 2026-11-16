package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func init() {
	// OpenCode, Kilo, and MiMoCode share one parser. Their released CLIs
	// write SQLite message and part rows; the JSON storage tree is the
	// legacy layout. The fuzz input is one assistant part's data column.
	for _, family := range []struct {
		agent  AgentType
		dbName string
	}{
		{AgentOpenCode, "opencode.db"},
		{AgentKilo, "kilo.db"},
		{AgentMiMoCode, "mimocode.db"},
	} {
		registerProviderFuzzTarget(providerFuzzTarget{
			agent: family.agent,
			place: placeOpenCodeFamilyFuzzPart(family.dbName),
			seeds: []providerFuzzSeed{
				{file: "opencode/part_tool.json", want: "config/settings.toml"},
				{file: "opencode/part_text.json", want: "The server listens on port 8080"},
			},
		})
	}
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOpenCodeReview,
		path:  "home-user-code-app/efb23cb0-2f65-4f32-9f9d-310b0614b737.jsonl",
		seeds: []providerFuzzSeed{{
			file: "opencodereview/Users-reviewer-src-wagtail-wagtail/efb23cb0-2f65-4f32-9f9d-310b0614b737.jsonl",
			want: "removes a stale FIXME comment",
		}},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOpenHands,
		path:  "4c1b2f0e7d1a4f6b9a332b8f6e1d0a00/events/event-00000.json",
		companions: map[string]string{
			"4c1b2f0e7d1a4f6b9a332b8f6e1d0a00/base_state.json": `{"id":"4c1b2f0e-7d1a-4f6b-9a33-2b8f6e1d0a00","agent":{"llm":{"model":"claude-sonnet-4-6"}}}`,
		},
		seeds: []providerFuzzSeed{
			{file: "openhands/event_user_message.json", want: "Find why the health check endpoint returns 500"},
			{file: "openhands/event_terminal_action.json", want: "tail -n 40 /home/user/code/app/logs/server.log"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAmp,
		path:  "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd.json",
		seeds: []providerFuzzSeed{
			{file: "amp/thread_basic.json", want: "Add a created_at column to the users table."},
			{file: "amp/thread_tools.json", want: "Read main.go and tell me what it does."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentJunie,
		path:  "session-260101-120000-abcd/events.jsonl",
		companions: map[string]string{
			"index.jsonl": `{"sessionId":"session-260101-120000-abcd","createdAt":1704067200000,"updatedAt":1704067207000,"projectDir":"/home/user/code/app","taskName":"Signup validation","status":"completed"}` + "\n",
		},
		seeds: []providerFuzzSeed{
			{file: "junie/events.jsonl", want: "Add input validation to the signup form"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentZencoder,
		path:  "abc-123.jsonl",
		seeds: []providerFuzzSeed{
			{file: "zencoder/session_basic.jsonl", want: "Fix the off-by-one error in pagination."},
			{file: "zencoder/session_tools.jsonl", want: "Read the pagination helper."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentGptme,
		path:  "2026-06-13-write-hello-world/conversation.jsonl",
		seeds: []providerFuzzSeed{
			{file: "gptme/2026-06-13-write-hello-world/conversation.jsonl", want: "Write a hello world program in Python."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentGrok,
		path:  "%2Fhome%2Fuser%2Fcode%2Fapp/019f6000-0000-7000-8000-000000000001/chat_history.jsonl",
		companions: map[string]string{
			"%2Fhome%2Fuser%2Fcode%2Fapp/019f6000-0000-7000-8000-000000000001/summary.json": `{"info":{"id":"019f6000-0000-7000-8000-000000000001","cwd":"/home/user/code/app"},"created_at":"2026-07-18T10:00:00Z","updated_at":"2026-07-18T10:30:00Z","current_model_id":"grok-4.5"}`,
		},
		seeds: []providerFuzzSeed{
			{
				file: "grok-build/current/%2Fworkspace%2Fgrok-worktrees%2Fparser-audit/019f6000-0000-7000-8000-000000000001/chat_history.jsonl",
				want: "Review parser compatibility",
			},
			{
				file: "grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/chat_history.jsonl",
				want: "Check the old format",
			},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentHermes,
		path:  "20260403_153620_5a3e2ff1.jsonl",
		seeds: []providerFuzzSeed{
			{file: "hermes/session_basic.jsonl", want: "Fix the flaky date parsing test"},
			{file: "hermes/session_tools.jsonl", want: "cmd/server/main.go"},
		},
	})
	// Augure Desktop keeps its sessions in a Hermes-schema state.db. The
	// fuzz input is the assistant row's tool_calls column, the JSON the
	// parser decodes from each message row.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAugureDesktop,
		place: placeAugureDesktopFuzzToolCalls,
		seeds: []providerFuzzSeed{
			{file: "augure-desktop/tool_calls_terminal.json", want: "ls internal/parser"},
			{file: "augure-desktop/tool_calls_read.json", want: "docs/usage.md"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentAider,
		path:  "app/.aider.chat.history.md",
		seeds: []providerFuzzSeed{
			{file: "aider/myrepo/.aider.chat.history.md", want: "add a retry to the webhook"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentVibe,
		path:  "session_20260613_100000_abc123de/messages.jsonl",
		companions: map[string]string{
			"session_20260613_100000_abc123de/meta.json": `{"session_id":"abc123de-0000-0000-0000-000000000000","start_time":"2026-06-13T10:00:00Z","end_time":"2026-06-13T10:05:00Z","model":"mistral-medium-3.5","environment":{"working_directory":"/home/user/code/app"}}`,
		},
		seeds: []providerFuzzSeed{
			{file: "vibe/session_basic/messages.jsonl", want: "Create a Python function to sort a list"},
			{file: "vibe/session_with_tools/messages.jsonl", want: "Read the README file"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCortex,
		path:  "11111111-2222-3333-4444-555555555555.json",
		seeds: []providerFuzzSeed{
			{file: "cortex/session_basic.json", want: "Explain what the revenue query does"},
			{file: "cortex/session_tools.json", want: "Read models/orders.sql"},
		},
	})
}

// placeOpenCodeFamilyFuzzPart builds an OpenCode-schema database named
// dbName with one session: a fixed user prompt, then an assistant message
// whose only part's data column holds the fuzz input.
func placeOpenCodeFamilyFuzzPart(dbName string) func(*testing.T, string, []byte) {
	return func(t *testing.T, root string, data []byte) {
		t.Helper()
		_, seeder, db := newTestDBAt(t, filepath.Join(root, dbName))
		seeder.AddProject("prj_fuzz", "/home/user/code/app")
		seeder.AddSession("ses_fuzz", "prj_fuzz", "", "Fuzz session",
			1700000000000, 1700000060000)
		seeder.AddMessage("msg_user", "ses_fuzz", 1700000001000, 1700000001000,
			`{"role":"user","time":{"created":1700000001000}}`)
		seeder.AddPart("prt_user", "msg_user", "ses_fuzz", 1700000001000, 1700000001000,
			`{"type":"text","text":"Which port does the server use?"}`)
		seeder.AddMessage("msg_asst", "ses_fuzz", 1700000002000, 1700000004000,
			`{"role":"assistant","modelID":"big-pickle","providerID":"opencode","time":{"created":1700000002000,"completed":1700000004000},"tokens":{"input":120,"output":30,"reasoning":0,"cache":{"read":0,"write":0}}}`)
		seeder.AddPart("prt_asst", "msg_asst", "ses_fuzz", 1700000002000, 1700000004000,
			string(data))
		require.NoError(t, db.Close())
	}
}

// placeAugureDesktopFuzzToolCalls builds the Augure Desktop state.db fixture
// and stores the fuzz input as the assistant row's tool_calls JSON.
func placeAugureDesktopFuzzToolCalls(t *testing.T, root string, data []byte) {
	t.Helper()
	stateDB := createAugureDesktopStateDB(t, root)
	db, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(),
		`UPDATE messages SET tool_calls = ? WHERE role = 'assistant'`, string(data))
	require.NoError(t, err)
}

func FuzzProviderOpenCode(f *testing.F) { fuzzProvider(f, AgentOpenCode) }

func FuzzProviderKilo(f *testing.F) { fuzzProvider(f, AgentKilo) }

func FuzzProviderMiMoCode(f *testing.F) { fuzzProvider(f, AgentMiMoCode) }

func FuzzProviderOpenCodeReview(f *testing.F) { fuzzProvider(f, AgentOpenCodeReview) }

func FuzzProviderOpenHands(f *testing.F) { fuzzProvider(f, AgentOpenHands) }

func FuzzProviderAmp(f *testing.F) { fuzzProvider(f, AgentAmp) }

func FuzzProviderJunie(f *testing.F) { fuzzProvider(f, AgentJunie) }

func FuzzProviderZencoder(f *testing.F) { fuzzProvider(f, AgentZencoder) }

func FuzzProviderGptme(f *testing.F) { fuzzProvider(f, AgentGptme) }

func FuzzProviderGrok(f *testing.F) { fuzzProvider(f, AgentGrok) }

func FuzzProviderHermes(f *testing.F) { fuzzProvider(f, AgentHermes) }

func FuzzProviderAugureDesktop(f *testing.F) { fuzzProvider(f, AgentAugureDesktop) }

func FuzzProviderAider(f *testing.F) { fuzzProvider(f, AgentAider) }

func FuzzProviderVibe(f *testing.F) { fuzzProvider(f, AgentVibe) }

func FuzzProviderCortex(f *testing.F) { fuzzProvider(f, AgentCortex) }
