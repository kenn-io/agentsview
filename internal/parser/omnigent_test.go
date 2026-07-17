// ABOUTME: Tests for the omnigent chat.db parser: cross-generation schema
// ABOUTME: equivalence, item decode, fingerprinting, usage, and a real-copy run.
package parser

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// omnigentSeedItem is one logical conversation_items row, referenced by its
// testdata payload file. The gen-specific builders translate `typeName` into a
// VARCHAR name (old) or SMALLINT code (split).
type omnigentSeedItem struct {
	conv, typeName, fixture, search string
	pos                             int
}

var omnigentSeedItems = []omnigentSeedItem{
	{"conv_root", omnigentTypeMessage, "message_user.json", "do the thing", 0},
	{"conv_root", omnigentTypeMessage, "message_assistant.json", "on it", 1},
	{"conv_root", omnigentTypeFuncCall, "function_call.json", "sys_os_shell", 2},
	{"conv_root", omnigentTypeFuncOutput, "function_call_output.json",
		"/Users/alice/code/proj", 3},
	{"conv_root", omnigentTypeReasoning, "reasoning.json", "weighing options", 4},
	{"conv_root", omnigentTypeError, "error.json", "inner executor error", 5},
	{"conv_root", omnigentTypeCompaction, "compaction.json",
		"context was compacted", 6},
	{"conv_root", omnigentTypeSlashCommand, "slash_command.json", "bulletproof", 7},
	{"conv_root", omnigentTypeTerminal, "terminal_command.json", "git push", 8},
	{"conv_kid", omnigentTypeMessage, "message_subagent.json", "scout report", 0},
}

var omnigentItemTypeCode = map[string]int{
	omnigentTypeMessage:      1,
	omnigentTypeFuncCall:     2,
	omnigentTypeFuncOutput:   3,
	omnigentTypeReasoning:    4,
	omnigentTypeError:        5,
	omnigentTypeCompaction:   6,
	omnigentTypeSlashCommand: 10,
	omnigentTypeTerminal:     11,
}

const omnigentTestUsage = `{"input_tokens":100,"output_tokens":50,` +
	`"total_cost_usd":1.5,"by_model":{"claude-opus-4-8":` +
	`{"input_tokens":100,"output_tokens":50,"total_cost_usd":1.5}}}`

const omnigentOldGenDDL = `
CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
CREATE TABLE conversations (
	id VARCHAR(64) PRIMARY KEY,
	created_at INTEGER, updated_at INTEGER, title TEXT,
	kind VARCHAR(32), model_override VARCHAR(128),
	parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
	sub_agent_name VARCHAR(128), workspace VARCHAR(2048),
	git_branch VARCHAR(255), session_usage TEXT
);
CREATE TABLE conversation_items (
	id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type VARCHAR(32) NOT NULL,
	data TEXT NOT NULL, search_text TEXT NOT NULL
);
`

const omnigentSplitGenDDL = `
CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
CREATE TABLE conversations (
	workspace_id BIGINT NOT NULL DEFAULT 0,
	id VARCHAR(64), created_at INTEGER, updated_at INTEGER, title TEXT,
	parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
	next_position INTEGER, PRIMARY KEY (workspace_id, id)
);
CREATE TABLE omnigent_conversation_metadata (
	workspace_id BIGINT NOT NULL DEFAULT 0, id VARCHAR(64),
	kind SMALLINT, sub_agent_name VARCHAR(128),
	external_session_id VARCHAR(128), session_usage TEXT,
	workspace VARCHAR(2048), git_branch VARCHAR(255),
	archived BOOLEAN DEFAULT 0, PRIMARY KEY (workspace_id, id)
);
CREATE TABLE agent_configuration (
	workspace_id BIGINT NOT NULL DEFAULT 0, conversation_id VARCHAR(64),
	agent_id VARCHAR(64), reasoning_effort VARCHAR(32),
	model_override VARCHAR(128), harness_override VARCHAR(64),
	PRIMARY KEY (workspace_id, conversation_id)
);
CREATE TABLE conversation_items (
	workspace_id BIGINT NOT NULL DEFAULT 0,
	conversation_id VARCHAR(64) NOT NULL, id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type SMALLINT NOT NULL, status SMALLINT DEFAULT 1,
	data TEXT NOT NULL, search_text TEXT NOT NULL,
	PRIMARY KEY (workspace_id, conversation_id, id)
);
`

func execOmnigentDDL(t *testing.T, db *sql.DB, ddl string) {
	t.Helper()
	for stmt := range strings.SplitSeq(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		_, err := db.Exec(stmt)
		require.NoError(t, err, "exec ddl stmt")
	}
}

func readOmnigentFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "omnigent", name))
	require.NoError(t, err, "read fixture %s", name)
	return string(data)
}

func seedOmnigentItems(t *testing.T, db *sql.DB, useCodes bool) {
	t.Helper()
	for i, it := range omnigentSeedItems {
		typeVal := it.typeName
		if useCodes {
			typeVal = fmt.Sprintf("%d", omnigentItemTypeCode[it.typeName])
		}
		data := readOmnigentFixture(t, it.fixture)
		if useCodes {
			_, err := db.Exec(
				`INSERT INTO conversation_items
				 (conversation_id, id, position, type, data, search_text)
				 VALUES (?,?,?,?,?,?)`,
				it.conv, fmt.Sprintf("item_%d", i), it.pos, typeVal, data, it.search)
			require.NoError(t, err)
			continue
		}
		_, err := db.Exec(
			`INSERT INTO conversation_items
			 (id, conversation_id, position, type, data, search_text)
			 VALUES (?,?,?,?,?,?)`,
			fmt.Sprintf("item_%d", i), it.conv, it.pos, typeVal, data, it.search)
		require.NoError(t, err)
	}
}

// writeOmnigentOldGenDB builds a single-table, VARCHAR-enum chat.db.
func writeOmnigentOldGenDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()

	execOmnigentDDL(t, db, omnigentOldGenDDL)
	_, err = db.Exec(`INSERT INTO alembic_version VALUES ('n1a2b3c4d5e6')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO conversations
		(id, created_at, updated_at, title, kind, model_override,
		 parent_conversation_id, root_conversation_id, sub_agent_name,
		 workspace, git_branch, session_usage)
		VALUES
		('conv_root', 1783716327, 1783718231, 'top task', 'default',
		 'claude-opus-4-8', '', 'conv_root', '', '/Users/alice/code/proj', 'main', ?),
		('conv_kid', 1783716400, 1783716701, 'claude_code:scout', 'sub_agent',
		 '', 'conv_root', 'conv_root', 'claude_code', '', '', '')`,
		omnigentTestUsage)
	require.NoError(t, err)
	seedOmnigentItems(t, db, false)
	return path
}

// writeOmnigentSplitGenDB builds a split-table, SMALLINT-enum chat.db.
func writeOmnigentSplitGenDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()

	execOmnigentDDL(t, db, omnigentSplitGenDDL)
	_, err = db.Exec(`INSERT INTO alembic_version VALUES ('bb2c3d4e5f6a')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO conversations
		(id, created_at, updated_at, title, parent_conversation_id,
		 root_conversation_id)
		VALUES
		('conv_root', 1783716327, 1783718231, 'top task', '', 'conv_root'),
		('conv_kid', 1783716400, 1783716701, 'claude_code:scout', 'conv_root',
		 'conv_root')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO omnigent_conversation_metadata
		(id, kind, sub_agent_name, workspace, git_branch, session_usage)
		VALUES
		('conv_root', 1, '', '/Users/alice/code/proj', 'main', ?),
		('conv_kid', 2, 'claude_code', '', '', '')`, omnigentTestUsage)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO agent_configuration
		(conversation_id, model_override)
		VALUES ('conv_root', 'claude-opus-4-8')`)
	require.NoError(t, err)
	seedOmnigentItems(t, db, true)
	return path
}

// assertOmnigentParse checks the invariants both generations must satisfy.
func assertOmnigentParse(t *testing.T, results []ParseResult, workspacePrefix string) {
	t.Helper()
	require.Len(t, results, 2)
	byID := map[string]ParseResult{}
	for _, r := range results {
		byID[r.Session.ID] = r
	}

	rootID := "omnigent:" + workspacePrefix + "conv_root"
	kidID := "omnigent:" + workspacePrefix + "conv_kid"
	root, ok := byID[rootID]
	require.True(t, ok, "root session present")
	assert.Equal(t, omnigentAgent, root.Session.Agent)
	assert.Equal(t, "top task", root.Session.SessionName)
	assert.Equal(t, "proj", root.Session.Project)
	assert.Equal(t, "/Users/alice/code/proj", root.Session.Cwd)
	assert.Equal(t, "main", root.Session.GitBranch)
	assert.Equal(t, "do the thing", root.Session.FirstMessage)
	assert.Equal(t, 1, root.Session.UserMessageCount)
	assert.Equal(t, RelNone, root.Session.RelationshipType)
	assert.Empty(t, root.Session.ParentSessionID)
	require.NotEmpty(t, root.Session.File.Hash, "fingerprint stored")

	// 9 items; function_call_output folds onto its call -> 8 messages.
	require.Len(t, root.Messages, 8)
	assert.Equal(t, RoleUser, root.Messages[0].Role)
	assert.Equal(t, RoleAssistant, root.Messages[1].Role)
	assert.Equal(t, "on it", root.Messages[1].Content)

	call := root.Messages[2]
	assert.True(t, call.HasToolUse)
	require.Len(t, call.ToolCalls, 1)
	assert.Equal(t, "sys_os_shell", call.ToolCalls[0].ToolName)
	assert.Equal(t, "toolu_1", call.ToolCalls[0].ToolUseID)
	require.Len(t, call.ToolResults, 1, "output folded onto the call message")
	assert.Equal(t, "toolu_1", call.ToolResults[0].ToolUseID)
	assert.Equal(t, "/Users/alice/code/proj", call.ToolResults[0].ContentRaw)

	reasoning := root.Messages[3]
	assert.True(t, reasoning.HasThinking)
	assert.Contains(t, reasoning.ThinkingText, "shell out")

	assert.Contains(t, root.Messages[4].Content, "[error]")
	assert.Contains(t, root.Messages[4].Content, "terminated")
	assert.Contains(t, root.Messages[5].Content, "[compaction]")
	assert.Contains(t, root.Messages[6].Content, "[skill] bulletproof")
	assert.Contains(t, root.Messages[7].Content, "[terminal_command]")

	require.Len(t, root.UsageEvents, 1)
	assert.Equal(t, "claude-opus-4-8", root.UsageEvents[0].Model)
	assert.Equal(t, 100, root.UsageEvents[0].InputTokens)
	assert.Equal(t, 50, root.UsageEvents[0].OutputTokens)
	require.NotNil(t, root.UsageEvents[0].CostUSD)
	assert.InDelta(t, 1.5, *root.UsageEvents[0].CostUSD, 0.0001)
	assert.True(t, root.Session.HasTotalOutputTokens)
	assert.Equal(t, 50, root.Session.TotalOutputTokens)
	assert.True(t, root.Session.HasPeakContextTokens)
	assert.Equal(t, 100, root.Session.PeakContextTokens)

	kid, ok := byID[kidID]
	require.True(t, ok, "sub-agent session present")
	assert.Equal(t, RelSubagent, kid.Session.RelationshipType)
	assert.Equal(t, rootID, kid.Session.ParentSessionID)
	// cwd/branch inherited from the root conversation.
	assert.Equal(t, "/Users/alice/code/proj", kid.Session.Cwd)
	assert.Equal(t, "main", kid.Session.GitBranch)
}

func TestParseOmnigentDB_OldGen(t *testing.T) {
	results, err := ParseOmnigentDB(writeOmnigentOldGenDB(t), "testhost")
	require.NoError(t, err)
	assertOmnigentParse(t, results, "")
}

func TestParseOmnigentDB_SplitGen(t *testing.T) {
	results, err := ParseOmnigentDB(writeOmnigentSplitGenDB(t), "testhost")
	require.NoError(t, err)
	assertOmnigentParse(t, results, "0:")
}

// TestParseOmnigentDB_CrossGenEquivalence asserts the two generations produce
// the same transcript, so the schema adapter is the only difference.
func TestParseOmnigentDB_CrossGenEquivalence(t *testing.T) {
	oldRes, err := ParseOmnigentDB(writeOmnigentOldGenDB(t), "h")
	require.NoError(t, err)
	splitRes, err := ParseOmnigentDB(writeOmnigentSplitGenDB(t), "h")
	require.NoError(t, err)
	require.Equal(t, len(oldRes), len(splitRes))

	summarize := func(rs []ParseResult) map[string]string {
		m := map[string]string{}
		for _, r := range rs {
			var b strings.Builder
			for _, msg := range r.Messages {
				fmt.Fprintf(&b, "%s|%s|%v|", msg.Role, msg.Content, msg.HasToolUse)
			}
			m[r.Session.ID] = b.String()
		}
		return m
	}
	normalizeSplitIDs := func(rs []ParseResult) []ParseResult {
		for i := range rs {
			rs[i].Session.ID = strings.Replace(
				rs[i].Session.ID, "omnigent:0:", "omnigent:", 1)
		}
		return rs
	}
	assert.Equal(t, summarize(oldRes), summarize(normalizeSplitIDs(splitRes)))
}

func TestParseOmnigentDB_SplitWorkspaceIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), omnigentDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, db, omnigentSplitGenDDL)
	_, err = db.Exec(`INSERT INTO alembic_version VALUES ('workspace-test')`)
	require.NoError(t, err)
	for _, workspaceID := range []int64{7, 8} {
		_, err = db.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (?, 'same', 10, ?, ?, 'same')`, workspaceID,
			20+workspaceID, fmt.Sprintf("workspace-%d", workspaceID))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (?, 'same', 1, ?)`, workspaceID,
			fmt.Sprintf("/work/%d", workspaceID))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (?, 'same', 'msg', 0, 1, ?, '')`, workspaceID,
			fmt.Sprintf(`{"role":"user","content":[{"type":"input_text","text":"hello %d"}]}`, workspaceID))
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	results, err := ParseOmnigentDB(path, "host")
	require.NoError(t, err)
	require.Len(t, results, 2)
	byID := make(map[string]ParseResult, len(results))
	for _, result := range results {
		byID[result.Session.ID] = result
	}
	for _, workspaceID := range []int64{7, 8} {
		id := fmt.Sprintf("omnigent:%d:same", workspaceID)
		result, ok := byID[id]
		require.True(t, ok, "workspace session %s", id)
		assert.Equal(t, fmt.Sprintf("/work/%d", workspaceID), result.Session.Cwd)
		require.Len(t, result.Messages, 1)
		assert.Equal(t, fmt.Sprintf("hello %d", workspaceID), result.Messages[0].Content)
		assert.Contains(t, result.Session.File.Path,
			fmt.Sprintf("#%d:same", workspaceID))
	}
}

func TestParseOmnigentDB_UnsupportedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), omnigentDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	// conversations without a kind column and no metadata table -> the split
	// generation with metadata relocated to another physical DB.
	execOmnigentDDL(t, db, `
		CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
		CREATE TABLE conversations (id VARCHAR(64) PRIMARY KEY,
			created_at INTEGER, updated_at INTEGER, title TEXT,
			root_conversation_id VARCHAR(64));
		CREATE TABLE conversation_items (id VARCHAR(64) PRIMARY KEY,
			conversation_id VARCHAR(64), position INTEGER, type SMALLINT,
			data TEXT, search_text TEXT);`)
	require.NoError(t, db.Close())

	_, err = ParseOmnigentDB(path, "h")
	require.Error(t, err)
	var unsupported ErrOmnigentUnsupportedSchema
	require.ErrorAs(t, err, &unsupported)
}

func TestOmnigentProviderUnsupportedSchemaIsNonDestructive(t *testing.T) {
	path := filepath.Join(t.TempDir(), omnigentDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, db, `
		CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
		CREATE TABLE conversations (id VARCHAR(64) PRIMARY KEY,
			created_at INTEGER, updated_at INTEGER, title TEXT,
			root_conversation_id VARCHAR(64));
		CREATE TABLE conversation_items (id VARCHAR(64) PRIMARY KEY,
			conversation_id VARCHAR(64), position INTEGER, type SMALLINT,
			data TEXT, search_text TEXT);`)
	require.NoError(t, db.Close())

	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	assert.Equal(t, SkipUnsupportedSource, outcome.SkipReason)
	assert.True(t, outcome.ResultSetComplete)
	assert.False(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Results)
}

func TestOmnigentFingerprintChangesWithContent(t *testing.T) {
	path := writeOmnigentOldGenDB(t)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	require.NoError(t, err)

	before, err := listOmnigentConversationMetas(conn, schema)
	require.NoError(t, err)
	fpBefore := omnigentMetaByID(before, "conv_root").fingerprint()

	// Stable across repeated reads.
	again, err := listOmnigentConversationMetas(conn, schema)
	require.NoError(t, err)
	assert.Equal(t, fpBefore,
		omnigentMetaByID(again, "conv_root").fingerprint())

	// Appending an item changes the fingerprint (write via a separate
	// read-write handle; openOmnigentDB is read-only).
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('extra', 'conv_root', 99, 'message',
			'{"role":"user","content":[{"type":"input_text","text":"more"}]}',
			'more')`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	after, err := listOmnigentConversationMetas(conn, schema)
	require.NoError(t, err)
	assert.NotEqual(t, fpBefore,
		omnigentMetaByID(after, "conv_root").fingerprint())
}

// TestOmnigentChangedPathParsingIsBounded is the production-path cardinality
// regression: the provider resolves and parses the same one-member batch when
// the unchanged archive grows from a handful of conversations to hundreds.
func TestOmnigentChangedPathParsingIsBounded(t *testing.T) {
	for _, archiveSize := range []int{5, 200} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentCardinalityDB(t, archiveSize)
			provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
				Roots: []string{filepath.Dir(path)}, Machine: "host",
			})
			require.True(t, ok)
			discovered, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			initial, err := provider.Parse(context.Background(), ParseRequest{
				Source: discovered[0],
			})
			require.NoError(t, err)
			require.Len(t, initial.Results, archiveSize)

			writer, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			const changedAt = int64(1_800_000_000)
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ? WHERE id = 'conv_000'`,
				changedAt)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES ('conv_000_i1', 'conv_000', 1, 'message',
					'{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
					'changed')`)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			changed, err := provider.SourcesForChangedPath(
				context.Background(), ChangedPathRequest{
					Path: path + "-wal", EventKind: "write",
				})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, VirtualSourcePath(path, "conv_000"), changed[0].DisplayPath)
			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: changed[0],
			})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, changedAt*int64(time.Second),
				outcome.Results[0].Result.Session.File.Mtime)
		})
	}
}

func writeOmnigentCardinalityDB(t *testing.T, count int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentOldGenDDL)
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('cardinality')`)
	require.NoError(t, err)
	for i := range count {
		id := fmt.Sprintf("conv_%03d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = database.Exec(`INSERT INTO conversations
			(id, created_at, updated_at, title, kind, root_conversation_id)
			VALUES (?, ?, ?, ?, 'default', ?)`, id, updatedAt-1, updatedAt, id, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(id, conversation_id, position, type, data, search_text)
			VALUES (?, ?, 0, 'message',
				'{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
				'hi')`, id+"_i0", id)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func omnigentMetaByID(metas []omnigentMeta, id string) omnigentMeta {
	for _, m := range metas {
		if m.rawID == id {
			return m
		}
	}
	return omnigentMeta{}
}

func TestDecodeOmnigentCompressed(t *testing.T) {
	// Legacy unframed plaintext passes through.
	got, err := decodeOmnigentCompressed([]byte(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, got)

	// Empty -> empty.
	got, err = decodeOmnigentCompressed(nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Raw-framed (sentinel + codec 0x00 + payload).
	raw := append([]byte{omnigentCompressSentinel, omnigentCodecRaw}, []byte("hi")...)
	got, err = decodeOmnigentCompressed(raw)
	require.NoError(t, err)
	assert.Equal(t, "hi", got)

	// zstd-framed (sentinel + codec 0x01 + zstd payload).
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	payload := enc.EncodeAll([]byte(omnigentTestUsage), nil)
	framed := append([]byte{omnigentCompressSentinel, omnigentCodecZstd}, payload...)
	got, err = decodeOmnigentCompressed(framed)
	require.NoError(t, err)
	assert.Equal(t, omnigentTestUsage, got)
}

// TestParseOmnigentDB_RealCopy is an opt-in eyeball against a real snapshot.
// Set OMNIGENT_POC_DB to a *copy* of a chat.db (never the live file).
func TestParseOmnigentDB_RealCopy(t *testing.T) {
	dbPath := os.Getenv("OMNIGENT_POC_DB")
	if dbPath == "" {
		t.Skip("set OMNIGENT_POC_DB to a chat.db copy to run this eyeball test")
	}
	results, err := ParseOmnigentDB(dbPath, "local")
	require.NoError(t, err)
	require.NotEmpty(t, results)

	var roots, subs, msgs int
	for _, r := range results {
		if r.Session.RelationshipType == RelSubagent {
			subs++
		} else {
			roots++
		}
		msgs += len(r.Messages)
	}
	t.Logf("omnigent: %d sessions (%d root, %d sub-agent), %d messages",
		len(results), roots, subs, msgs)
}
