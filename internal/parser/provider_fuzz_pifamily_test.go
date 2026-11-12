package parser

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Pi-derived and single-directory providers: the five agents that share the
// Pi parser, plus fx, Tau, Muse, OpenClaw, QClaw, QwenPaw, Omnigent, DeepSeek
// TUI, DeepSeek Harness, Evener, and Reasonix.

const providerFuzzPiSessionFile = "2025-01-01T10-00-00-000Z_0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.jsonl"

var providerFuzzPiSeeds = []providerFuzzSeed{
	{file: "pi/session.jsonl", want: "Fix the login bug"},
}

func init() {
	// Pi, OMO, and StepCode group sessions by project directory and accept
	// transcripts at any depth.
	for _, agent := range []AgentType{AgentPi, AgentOMO, AgentStepCode} {
		registerProviderFuzzTarget(providerFuzzTarget{
			agent: agent,
			path:  "--home-user-code-app--/" + providerFuzzPiSessionFile,
			seeds: providerFuzzPiSeeds,
		})
	}
	// OMP requires at least one project directory level.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOMP,
		path:  "--home-user-code-app--/0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.jsonl",
		seeds: providerFuzzPiSeeds,
	})
	// Prime Agent keeps sessions flat under its root.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentPrimeAgent,
		path:  "0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.jsonl",
		seeds: providerFuzzPiSeeds,
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentFx,
		path:  "parent-session/events.jsonl",
		companions: map[string]string{
			"parent-session/session.json": `{"schema_version":4,"id":"parent-session",` +
				`"workspace_root":"/tmp/fx-demo/widget","created_at_ms":1791839193807,` +
				`"updated_at_ms":1791839193807,"provider":"codex","model":"gpt-6.1-sol",` +
				`"title":"Rename widget helper","subagent_child":false}`,
		},
		seeds: []providerFuzzSeed{
			{file: "fx/parent-session/events.jsonl", want: "rename helper to buildWidget"},
			{file: "fx/child-session/events.jsonl", want: "Review widget.go in /tmp/fx-demo/widget read-only"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentTau,
		path:  "project-a/7f43089ed2b74b6797cc8fb13d5cac03.jsonl",
		seeds: []providerFuzzSeed{
			{file: "tau/issue-session.jsonl", want: "add ruff ty to pyproject.toml"},
		},
	})
	// Muse keeps only records whose stream ID names the session directory,
	// so the subagent log is not a usable seed at this path.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentMuse,
		path:  "2026/09/10/0199a1b2-6c3d-7e4f-8a5b-000000000001/session.jsonl",
		seeds: []providerFuzzSeed{{
			file: "muse/sessions/2026/09/10/0199a1b2-6c3d-7e4f-8a5b-000000000001/session.jsonl",
			want: "Fix the failing login test",
		}},
	})
	// OpenClaw reads both its legacy JSONL files and its SQLite store, so the
	// input feeds both: as one agent's JSONL transcript and as another
	// agent's transcript_events rows, one row per line.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOpenClaw,
		place: placeOpenClawFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "openclaw/session.jsonl", want: "List the files in the app directory"},
			{file: "pi/session.jsonl", want: "Fix the login bug"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentQClaw,
		path:  "main/sessions/2a3b4c5d-6e7f-4801-9a2b-3c4d5e6f7a8b.jsonl",
		seeds: []providerFuzzSeed{
			{file: "qclaw/session.jsonl", want: "Show me the hosts file"},
			{file: "pi/session.jsonl", want: "Fix the login bug"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentQwenPaw,
		path:  "default/sessions/default_1700000000000.json",
		seeds: []providerFuzzSeed{
			{file: "qwenpaw/default/sessions/default_1700000000000.json", want: "QwenPaw 的默认会话布局长什么样？"},
			{file: "qwenpaw/default/sessions/main_main.json", want: "读一下 NOTES.md 的内容"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentOmnigent,
		place: placeOmnigentFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "omnigent/message_user.json", want: "do the thing"},
			{file: "omnigent/function_call.json", want: `{"command": "pwd"}`},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentDeepSeekTUI,
		path:  "session_fuzz.json",
		seeds: []providerFuzzSeed{
			{file: "deepseek-tui/session_tools.json", want: "Read main.go and tell me what it does"},
			{file: "deepseek-tui/session_basic.json", want: "Inspect the server logs"},
		},
	})
	// The header's id and cwd must match the session and project
	// directories, so the path mirrors the fixture header.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentDeepSeekHarness,
		path:  "--home-user-code-app--/session-1/session.jsonl",
		seeds: []providerFuzzSeed{
			{file: "deepseek-harness/session.jsonl", want: "Open notes.txt and summarize it"},
		},
	})
	// The header's session_id must match the file name.
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentEvener,
		path:  "projects/app/sessions/session.transcript.jsonl",
		companions: map[string]string{
			"projects/app/sessions/session.meta.json": `{"id":"session","name":"Working directory"}`,
		},
		seeds: []providerFuzzSeed{
			{file: "evener/session.transcript.jsonl", want: "Print the working directory"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentReasonix,
		path:  "projects/app/sessions/session-1.jsonl",
		companions: map[string]string{
			"projects/app/sessions/session-1.jsonl.meta": `{"id":"session-1",` +
				`"created_at":"2026-06-12T10:42:35Z","updated_at":"2026-06-12T10:58:03Z",` +
				`"workspace_root":"/home/user/code/app","topic_title":"Config summary"}`,
		},
		seeds: []providerFuzzSeed{
			{file: "reasonix/session.jsonl", want: "Read config.json and summarize it"},
		},
	})
}

// placeOpenClawFuzzInput writes data as the legacy JSONL transcript of agent
// "main" and, split on newlines, as the transcript_events rows of agent
// "worker"'s SQLite store.
func placeOpenClawFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	writeProviderFuzzFile(t,
		filepath.Join(root, "main", "sessions", "7b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d.jsonl"),
		data)
	dbPath := openClawSQLiteDBPath(root, "worker")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db := openProviderFuzzSQLite(t, dbPath)
	defer db.Close()
	// Same table the OpenClaw SQLite store and createOpenClawSQLiteFixture use.
	_, err := db.ExecContext(t.Context(), `
		CREATE TABLE transcript_events (
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			event_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, seq)
		) STRICT
	`)
	require.NoError(t, err)
	seq := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO transcript_events(session_id, seq, event_json, created_at) VALUES (?, ?, ?, ?)`,
			"7b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d", seq, string(line), 1_700_000_000_000+seq,
		)
		require.NoError(t, err)
		seq++
	}
}

// placeOmnigentFuzzInput builds a chat.db with the older supported schema
// and one conversation whose items all carry data as their JSON payload, one
// item per decoded type, so every item decoder sees the input.
func placeOmnigentFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzSQLite(t, filepath.Join(root, omnigentDBName))
	defer db.Close()
	execOmnigentDDL(t, db, omnigentSplitGenDDL)
	_, err := db.ExecContext(t.Context(), `INSERT INTO alembic_version VALUES ('fuzz')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO conversations
		(id, created_at, updated_at, title, parent_conversation_id,
		 root_conversation_id)
		VALUES ('conv_root', 1783716327, 1783718231, 'fuzz', '', 'conv_root')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO omnigent_conversation_metadata
		(id, kind, sub_agent_name, workspace, git_branch, session_usage)
		VALUES ('conv_root', 1, '', '/work/proj', 'main', '')`)
	require.NoError(t, err)
	for pos, typeName := range []string{
		omnigentTypeMessage, omnigentTypeFuncCall, omnigentTypeFuncOutput,
		omnigentTypeReasoning, omnigentTypeError,
	} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO conversation_items
			(conversation_id, id, position, type, data, search_text)
			VALUES ('conv_root', ?, ?, ?, ?, 'fuzz')`,
			"item_"+typeName, pos, omnigentItemTypeCode[typeName], string(data))
		require.NoError(t, err)
	}
}

// openProviderFuzzSQLite opens a scratch database without a journal or
// fsync. Placement runs once per fuzz input, and synced writes made each
// placement take seconds.
func openProviderFuzzSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3",
		"file:"+sqliteURIPath(path)+"?_journal_mode=OFF&_synchronous=OFF")
	require.NoError(t, err)
	return db
}

func FuzzProviderPi(f *testing.F) { fuzzProvider(f, AgentPi) }

func FuzzProviderPrimeAgent(f *testing.F) { fuzzProvider(f, AgentPrimeAgent) }

func FuzzProviderOMP(f *testing.F) { fuzzProvider(f, AgentOMP) }

func FuzzProviderOMO(f *testing.F) { fuzzProvider(f, AgentOMO) }

func FuzzProviderStepCode(f *testing.F) { fuzzProvider(f, AgentStepCode) }

func FuzzProviderFx(f *testing.F) { fuzzProvider(f, AgentFx) }

func FuzzProviderTau(f *testing.F) { fuzzProvider(f, AgentTau) }

func FuzzProviderMuse(f *testing.F) { fuzzProvider(f, AgentMuse) }

func FuzzProviderOpenClaw(f *testing.F) { fuzzProvider(f, AgentOpenClaw) }

func FuzzProviderQClaw(f *testing.F) { fuzzProvider(f, AgentQClaw) }

func FuzzProviderQwenPaw(f *testing.F) { fuzzProvider(f, AgentQwenPaw) }

func FuzzProviderOmnigent(f *testing.F) { fuzzProvider(f, AgentOmnigent) }

func FuzzProviderDeepSeekTUI(f *testing.F) { fuzzProvider(f, AgentDeepSeekTUI) }

func FuzzProviderDeepSeekHarness(f *testing.F) { fuzzProvider(f, AgentDeepSeekHarness) }

func FuzzProviderEvener(f *testing.F) { fuzzProvider(f, AgentEvener) }

func FuzzProviderReasonix(f *testing.F) { fuzzProvider(f, AgentReasonix) }
