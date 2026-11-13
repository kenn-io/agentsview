package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fuzz targets for providers whose sessions live in a database, or whose
// transcript sits beside one. Each place function builds the store with the
// schema the provider's own tests use and puts the fuzz input in the column
// that holds transcript content, so the fuzzer explores the parser instead
// of SQLite's file format.

func init() {
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentGoose,
		place: placeGooseFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "goose/text_message.json", want: "Inspect the authentication flow."},
			{file: "goose/tool_turn.json", want: "Checking the auth package."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentForge,
		place: placeForgeFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "forge/conversation.json", want: "Add a retry to the HTTP client."},
			{file: "forge/user_only.json", want: "Explain the build script."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentDevin,
		place: placeDevinFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "devin/transcript.json", want: "Fix the login bug"},
			{file: "devin/user_only.json", want: "Summarize the README"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentPiebald,
		place: placePiebaldFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "piebald/tool_input.json", want: "go test ./internal/auth/..."},
			{file: "piebald/plain_text.txt", want: "Please fix the flaky login test"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentWarp,
		place: placeWarpFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "warp/conversation_data.json", want: "[Bash]"},
			{file: "warp/read_only.json", want: "[Read]"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentZCode,
		place: placeZCodeFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "zcode/text_part.json", want: "Fix the login bug."},
			{file: "zcode/tool_use_part.json", want: "internal/auth/session.go"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCrush,
		place: placeCrushFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "crush/user_parts.json", want: "Review the auth flow please."},
			{file: "crush/tool_parts.json", want: "Reading the handler now."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentZed,
		place: placeZedFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "zed/thread.json", want: "Generate the record scaffold"},
			{file: "zed/user_only.json", want: "Explain the failing test"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentShelley,
		place: placeShelleyFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "shelley/user_llm_data.json", want: "Add a Shelley parser"},
			{file: "shelley/agent_llm_data.json", want: "Listing the directory first."},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentKiro,
		place: placeKiroFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "kiro_sqlite/standard_payload.json", want: "Build the Kiro parser"},
			{file: "kiro_sqlite/overlap_payload.json", want: "Current store should win"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentKiroIDE,
		place: placeKiroIDEFuzzInput,
		seeds: []providerFuzzSeed{
			{file: "kiro-ide/old_format.chat", want: "Rename the config loader"},
			{file: "kiro-ide/new_format.json", want: "Add pagination to the list endpoint"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentPoolside,
		path:  "trajectories/trajectory-standalone_0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.ndjson",
		seeds: []providerFuzzSeed{
			{file: "poolside/trajectory.ndjson", want: "Read the config file"},
			{file: "poolside/simple.ndjson", want: "Hello, can you help me?"},
		},
	})
}

func FuzzProviderGoose(f *testing.F) { fuzzProvider(f, AgentGoose) }

func FuzzProviderForge(f *testing.F) { fuzzProvider(f, AgentForge) }

func FuzzProviderDevin(f *testing.F) { fuzzProvider(f, AgentDevin) }

func FuzzProviderPiebald(f *testing.F) { fuzzProvider(f, AgentPiebald) }

func FuzzProviderWarp(f *testing.F) { fuzzProvider(f, AgentWarp) }

func FuzzProviderZCode(f *testing.F) { fuzzProvider(f, AgentZCode) }

func FuzzProviderCrush(f *testing.F) { fuzzProvider(f, AgentCrush) }

func FuzzProviderZed(f *testing.F) { fuzzProvider(f, AgentZed) }

func FuzzProviderShelley(f *testing.F) { fuzzProvider(f, AgentShelley) }

func FuzzProviderKiro(f *testing.F) { fuzzProvider(f, AgentKiro) }

func FuzzProviderKiroIDE(f *testing.F) { fuzzProvider(f, AgentKiroIDE) }

func FuzzProviderPoolside(f *testing.F) { fuzzProvider(f, AgentPoolside) }

// openProviderFuzzDB creates a SQLite database at path with schema applied.
// The caller closes it before the provider reads the file.
func openProviderFuzzDB(t *testing.T, path, schema string) *sql.DB {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db := openProviderFuzzSQLite(t, path)
	_, err := db.ExecContext(t.Context(), schema)
	require.NoError(t, err)
	return db
}

// placeGooseFuzzInput stores the input as messages.content_json for a user
// row and an assistant row, since Goose reads tool requests and responses
// differently by role.
func placeGooseFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, GooseDBName), gooseTestSchema)
	fixture := &gooseTestFixture{database: db}
	fixture.insertSession(t, "fuzz-session", "Fuzz session", "user", "")
	fixture.insertMessage(t, "fuzz-session", "user", string(data), 1_700_000_000)
	fixture.insertMessage(t, "fuzz-session", "assistant", string(data), 1_700_000_001)
	require.NoError(t, db.Close())
}

// placeForgeFuzzInput stores the input as conversations.context, the JSON
// document holding the whole Forge transcript.
func placeForgeFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, ForgeDBFilename), forgeSchema)
	seeder := &ForgeSeeder{db: db, t: t}
	seeder.AddConversation(t.Context(), "conv-fuzz", "Fuzz conversation", 1,
		string(data), "2026-05-02 09:58:15", "2026-05-02 10:00:16", "")
	require.NoError(t, db.Close())
}

// placeDevinFuzzInput registers one session in cli/sessions.db and writes
// the input as its transcript file, which Devin prefers over the database's
// message_nodes rows.
func placeDevinFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	const sessionID = "session-fuzz"
	cliDir := filepath.Join(root, "cli")
	dbPath := filepath.Join(cliDir, devinDBFilename)
	require.NoError(t, os.MkdirAll(cliDir, 0o755))
	initDevinTestDB(t, dbPath)
	execDevinTestSQL(t, dbPath, devinSyntheticSessionsSchema)
	execDevinTestSQL(t, dbPath, devinSyntheticMessageNodesSchema)
	insertDevinSessionRow(t, dbPath, devinSessionRow{
		ID:               sessionID,
		Title:            "Fuzz session",
		WorkingDirectory: "/home/user/code/app",
		CreatedAt:        new(int64(1_704_103_199)),
		LastActivityAt:   new(int64(1_704_103_265)),
	})
	writeProviderFuzzFile(t, filepath.Join(cliDir, "transcripts", sessionID+".json"), data)
}

// placePiebaldFuzzInput uses the input as the user's text node and as the
// assistant tool call's input and result. Piebald stores parts in
// normalized tables, so these are the columns that carry agent output.
func placePiebaldFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	dbPath := filepath.Join(root, PiebaldDBFilename)
	require.NoError(t, os.Rename(newPiebaldTestDB(t), dbPath))
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats
		 (id, title, created_at, updated_at, is_deleted, message_count, current_directory)
		 VALUES (42, 'Fuzz chat', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 2, '/home/user/code/app')`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, status)
		 VALUES (100, 42, 'user', '', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
	seedPiebaldTextPart(t, dbPath, 200, 100, 0, string(data), false)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, status, finish_reason)
		 VALUES (101, 42, 'assistant', 'claude-test', '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z', 'completed', 'end_turn')`)
	withPiebaldTestTx(t, dbPath, func(tx *sql.Tx) {
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
			 VALUES (201, 101, 0, 'tool_call')`)
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_part_tool_call
			 (message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state)
			 VALUES (201, 'toolu_1', 'Bash', ?, ?, 'completed')`, string(data), string(data))
	})
}

// placeWarpFuzzInput stores the input as agent_conversations.
// conversation_data. Warp keeps only the user's query text per exchange;
// conversation_data is the JSON from which the parser synthesizes the
// assistant's tool calls and token totals. One fixed exchange carries the
// query so the conversation yields a session.
func placeWarpFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, WarpDBFilename), warpSchema)
	seeder := &WarpSeeder{db: db, t: t}
	seeder.AddConversation(t.Context(), "conv-fuzz", string(data), "2026-04-07 10:00:00")
	seeder.AddExchange(t.Context(), "ex-001", "conv-fuzz", "2026-04-07 09:50:00.000000",
		`[{"Query":{"text":"Check the deploy script","context":[]}}]`,
		"/home/user/code/app", `"Completed"`, "auto-genius")
	require.NoError(t, db.Close())
}

// placeZCodeFuzzInput stores the input as part.data under a user message
// and an assistant message.
func placeZCodeFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, "db", zcodeDBName), zcodeTestSchema)
	fixture := &zcodeTestFixture{database: db}
	fixture.insertSession(t, "session-fuzz", "/home/user/code/app", "Fuzz session",
		"2026-07-06T13:00:01Z", "2026-07-06T13:05:00Z", "", "")
	fixture.insertMessage(t, "msg-1", "session-fuzz", "2026-07-06T13:00:01Z", `{"role":"user"}`)
	fixture.insertPart(t, "part-1", "msg-1", "session-fuzz", string(data))
	fixture.insertMessage(t, "msg-2", "session-fuzz", "2026-07-06T13:00:02Z",
		`{"role":"assistant","model":{"modelID":"claude-sonnet-4-6"}}`)
	fixture.insertPart(t, "part-2", "msg-2", "session-fuzz", string(data))
	require.NoError(t, db.Close())
}

// placeCrushFuzzInput stores the input as messages.parts for a user, an
// assistant, and a tool row; Crush interprets parts by role.
func placeCrushFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	const created = int64(1_789_093_626)
	db := openProviderFuzzDB(t, filepath.Join(root, CrushDBName), crushTestSchema)
	fixture := &crushTestFixture{database: db}
	fixture.insertSession(t, "sess-fuzz", "Fuzz session", "", created, created+120, 100, 20, 0.01)
	fixture.insertMessage(t, "msg-user", "sess-fuzz", "user", string(data), created, "", "")
	fixture.insertMessage(t, "msg-assistant", "sess-fuzz", "assistant", string(data),
		created+1, "glm-5.3-flash", "hyper")
	fixture.insertMessage(t, "msg-tool", "sess-fuzz", "tool", string(data), created+2, "", "")
	require.NoError(t, db.Close())
}

// placeZedFuzzInput stores the input as threads.data with data_type json.
// Zed also writes zstd-compressed rows; mutated bytes would almost never
// decompress, so the target fuzzes the decoded JSON document.
func placeZedFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	dbPath := filepath.Join(root, filepath.FromSlash(zedThreadsDBRelPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:          "0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10",
		summary:     "Fuzz thread",
		createdAt:   "2026-06-08T09:12:41Z",
		updatedAt:   "2026-06-08T09:14:10Z",
		folderPaths: "/home/user/code/app",
		dataType:    "json",
		data:        data,
	}})
}

// placeShelleyFuzzInput stores the input as messages.llm_data for a user,
// an agent, and a tool row.
func placeShelleyFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, shelleyDBName), shelleySchema)
	seedShelleyConversation(t, db, "cFUZZ1", "Fuzz conversation",
		"/home/user/code/app", "claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:05:00Z")
	seedShelleyMessage(t, db, "cFUZZ1", 1, 1, "user", string(data), "", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cFUZZ1", 2, 1, "agent", string(data), "", "", "2026-06-15T10:00:05Z")
	seedShelleyMessage(t, db, "cFUZZ1", 3, 1, "tool", string(data), "", "", "2026-06-15T10:00:06Z")
	require.NoError(t, db.Close())
}

// placeKiroFuzzInput stores the input as conversations_v2.value in Kiro
// CLI's data.sqlite3. The JSONL session layouts Kiro also reads are not
// covered by this target.
func placeKiroFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	db := openProviderFuzzDB(t, filepath.Join(root, kiroSQLiteDBName), kiroSQLiteSchema)
	seedKiroSQLiteSession(t, db, "/home/user/code/kiro-app", "fuzz-session",
		string(data), 1_779_012_000_000, 1_779_012_030_000)
	require.NoError(t, db.Close())
}

// placeKiroIDEFuzzInput writes the input in both Kiro IDE layouts: an old
// <workspace-hash>/<file-hash>.chat file and a new
// workspace-sessions/<workspace>/<session>.json file. The provider picks the
// parser by extension, so one input exercises both.
func placeKiroIDEFuzzInput(t *testing.T, root string, data []byte) {
	t.Helper()
	writeProviderFuzzFile(t, filepath.Join(root,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.chat"), data)
	writeProviderFuzzFile(t, filepath.Join(root,
		"workspace-sessions", "encoded-workspace", "0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.json"), data)
}
