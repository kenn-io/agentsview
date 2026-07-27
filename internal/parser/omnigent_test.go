// ABOUTME: Tests for the omnigent chat.db parser: cross-generation schema
// ABOUTME: equivalence, item decode, fingerprinting, usage, and a real-copy run.
package parser

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type omnigentParseCancellationContext struct {
	context.Context
	firstCheck chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (c *omnigentParseCancellationContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() {
		close(c.firstCheck)
		<-c.release
	})
	return err
}

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
		"/work/proj", 3},
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
CREATE INDEX ix_conversations_updated_at ON conversations(updated_at, id);
CREATE TABLE conversation_items (
	id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL,
	position INTEGER NOT NULL, type VARCHAR(32) NOT NULL,
	data TEXT NOT NULL, search_text TEXT NOT NULL
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(conversation_id, position);
`

const omnigentSplitGenDDL = `
CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
CREATE TABLE conversations (
	workspace_id BIGINT NOT NULL DEFAULT 0,
	id VARCHAR(64), created_at INTEGER, updated_at INTEGER, title TEXT,
	parent_conversation_id VARCHAR(64), root_conversation_id VARCHAR(64),
	next_position INTEGER, PRIMARY KEY (workspace_id, id)
);
CREATE INDEX ix_conversations_updated_at
	ON conversations(workspace_id, updated_at, id);
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
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(workspace_id, conversation_id, position);
`

// omnigentBinaryIDGenDDL mirrors the current pinned Omnigent generation: id
// columns are 16-byte UUID BLOBs, enums are SMALLINT codes, session metadata is
// split into omnigent_conversation_metadata, and session overrides live on the
// conversations row.
const omnigentBinaryIDGenDDL = `
CREATE TABLE alembic_version (version_num VARCHAR(32) NOT NULL);
CREATE TABLE conversations (
	id BLOB NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	title VARCHAR(768) DEFAULT ('') NOT NULL,
	parent_conversation_id BLOB, root_conversation_id BLOB NOT NULL,
	next_position INTEGER, workspace_id BIGINT DEFAULT '0' NOT NULL,
	agent_id BLOB, session_overrides VARCHAR(512),
	archived BOOLEAN DEFAULT 0 NOT NULL,
	PRIMARY KEY (workspace_id, id)
);
CREATE INDEX ix_conversations_archived_updated
	ON conversations(workspace_id, archived, updated_at, id);
CREATE TABLE omnigent_conversation_metadata (
	workspace_id BIGINT DEFAULT '0' NOT NULL, id BLOB NOT NULL,
	kind SMALLINT NOT NULL, sub_agent_name VARCHAR(128),
	external_session_id VARCHAR(128), session_usage BLOB,
	workspace VARCHAR(2048), git_branch VARCHAR(255),
	PRIMARY KEY (workspace_id, id)
);
CREATE TABLE conversation_items (
	id BLOB NOT NULL, conversation_id BLOB NOT NULL,
	response_id VARCHAR(64) NOT NULL, created_at INTEGER NOT NULL,
	position INTEGER NOT NULL, type SMALLINT NOT NULL,
	status SMALLINT NOT NULL, data TEXT NOT NULL, search_text TEXT NOT NULL,
	workspace_id BIGINT DEFAULT '0' NOT NULL,
	PRIMARY KEY (workspace_id, conversation_id, id, created_at)
);
CREATE INDEX ix_conversation_items_conversation_id_position
	ON conversation_items(workspace_id, conversation_id, position);
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
		 'claude-opus-4-8', '', 'conv_root', '', '/work/proj', 'main', ?),
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
		('conv_root', 1, '', '/work/proj', 'main', ?),
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
	assert.Equal(t, "/work/proj", root.Session.Cwd)
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
	expectedToolOutput := omnigentSeedItems[3].search
	assert.Equal(t, fmt.Sprintf("%q", expectedToolOutput),
		call.ToolResults[0].ContentRaw)
	assert.Equal(t, expectedToolOutput,
		DecodeContent(call.ToolResults[0].ContentRaw))

	reasoning := root.Messages[3]
	assert.True(t, reasoning.HasThinking)
	assert.Contains(t, reasoning.ThinkingText, "shell out")

	assert.Contains(t, root.Messages[4].Content, "[error]")
	assert.Contains(t, root.Messages[4].Content, "terminated")
	assert.Contains(t, root.Messages[5].Content, "[compaction]")
	assert.Contains(t, root.Messages[6].Content, "[skill] bulletproof")
	assert.Contains(t, root.Messages[7].Content, "[terminal_command]")

	require.Len(t, root.UsageEvents, 1)
	assert.Equal(t, "session", root.UsageEvents[0].Source)
	assert.Equal(t, "claude-opus-4-8", root.UsageEvents[0].Model)
	assert.Equal(t, 100, root.UsageEvents[0].InputTokens)
	assert.Equal(t, 50, root.UsageEvents[0].OutputTokens)
	require.NotNil(t, root.UsageEvents[0].CostUSD)
	assert.InDelta(t, 1.5, *root.UsageEvents[0].CostUSD, 0.0001)
	assert.True(t, root.Session.HasTotalOutputTokens)
	assert.Equal(t, 50, root.Session.TotalOutputTokens)
	assert.False(t, root.Session.HasPeakContextTokens)
	assert.Zero(t, root.Session.PeakContextTokens)

	kid, ok := byID[kidID]
	require.True(t, ok, "sub-agent session present")
	assert.Equal(t, RelSubagent, kid.Session.RelationshipType)
	assert.Equal(t, rootID, kid.Session.ParentSessionID)
	// cwd/branch inherited from the root conversation.
	assert.Equal(t, "/work/proj", kid.Session.Cwd)
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

func TestOmnigentProviderMemberParseInfersContinuationRelationship(t *testing.T) {
	path := writeOmnigentOldGenDB(t)
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations SET kind = 'default' WHERE id = 'conv_kid'`,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), FindSourceRequest{FullSessionID: "omnigent:conv_kid"},
	)
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(
		context.Background(), ParseRequest{Source: source},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, RelContinuation,
		outcome.Results[0].Result.Session.RelationshipType)
	assert.Equal(t, "omnigent:conv_root",
		outcome.Results[0].Result.Session.ParentSessionID)
}

func TestDecodeOmnigentFunctionOutputPreservesJSONString(t *testing.T) {
	const output = "{\"ok\":true}\x00\x1b"
	messages := []ParsedMessage{{Role: RoleAssistant}}
	decodeOmnigentItem(
		1, omnigentTypeFuncOutput,
		`{"call_id":"call-json","output":"{\"ok\":true}\u0000\u001b"}`,
		"", &messages, map[string]int{"call-json": 0},
	)

	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolResults, 1)
	result := messages[0].ToolResults[0]
	assert.True(t, json.Valid([]byte(result.ContentRaw)))
	assert.Equal(t, output, DecodeContent(result.ContentRaw))
	assert.Equal(t, len(output), result.ContentLength)
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

func TestDetectOmnigentSchemaPropagatesDatabaseErrors(t *testing.T) {
	path := writeOmnigentOldGenDB(t)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	_, err = detectOmnigentSchema(conn)
	require.Error(t, err)
	var unsupported ErrOmnigentUnsupportedSchema
	assert.False(t, errors.As(err, &unsupported),
		"operational database errors must remain retryable")
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
	outcome, err := provider.Parse(
		context.Background(), ParseRequest{Source: sources[0]},
	)
	require.NoError(t, err)
	assert.Equal(t, SkipUnsupportedSource, outcome.SkipReason,
		"an unsupported schema must skip, not retire, the archive")
	assert.False(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Results)
}

func TestOmnigentProviderPartialItemIndexIsNonDestructive(t *testing.T) {
	path := writeOmnigentOldGenDB(t)
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`DROP INDEX ix_conversation_items_conversation_id_position`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(`
		CREATE INDEX partial_conversation_items_lookup
			ON conversation_items(conversation_id, position)
			WHERE position >= 0`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)

	outcome, err := provider.Parse(
		t.Context(), ParseRequest{Source: sources[0]},
	)
	require.NoError(t, err)
	assert.Equal(t, SkipUnsupportedSource, outcome.SkipReason)
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

	before, err := listOmnigentConversationMetas(t.Context(), conn, schema)
	require.NoError(t, err)
	fpBefore := omnigentMetaByID(before, "conv_root").fingerprint()

	// Stable across repeated reads.
	again, err := listOmnigentConversationMetas(t.Context(), conn, schema)
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
	after, err := listOmnigentConversationMetas(t.Context(), conn, schema)
	require.NoError(t, err)
	assert.NotEqual(t, fpBefore,
		omnigentMetaByID(after, "conv_root").fingerprint())
}

// TestOmnigentChangedPathParsingIsBounded is the production-path cardinality
// regression: a warm changed-path event resolves only the changed member, and
// the fan-out stays the same when the unchanged archive grows from one hundred
// conversations to two hundred.
func TestOmnigentChangedPathParsingIsBounded(t *testing.T) {
	for _, archiveSize := range []int{100, 200} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentCardinalityDB(t, archiveSize)
			changedID := fmt.Sprintf("conv_%03d", archiveSize/2)
			provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
				Roots: []string{filepath.Dir(path)}, Machine: "host",
			})
			require.True(t, ok)
			initializeOmnigentProvider(t, provider, archiveSize)

			writer, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			changedAt := time.Now().Unix()
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ? WHERE id = ?`,
				changedAt, changedID)
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(id, conversation_id, position, type, data, search_text)
				VALUES (?, ?, 1, 'message',
					'{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
					'changed')`, changedID+"_i1", changedID)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			changed, err := provider.SourcesForChangedPath(
				context.Background(), ChangedPathRequest{
					Path: path + "-wal", EventKind: "write",
				})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			changedIndex := slices.IndexFunc(changed, func(source SourceRef) bool {
				return source.DisplayPath == VirtualSourcePath(path, changedID)
			})
			require.NotEqual(t, -1, changedIndex)
			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: changed[changedIndex],
			})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, changedAt*int64(time.Second),
				outcome.Results[0].Result.Session.File.Mtime)
		})
	}
}

func TestOmnigentWarmDiscoveryIsBoundedUnlessForced(t *testing.T) {
	for _, archiveSize := range []int{100, 200} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentCardinalityDB(t, archiveSize)
			changedID := fmt.Sprintf("conv_%03d", archiveSize/2)
			factory, ok := ProviderFactoryByType(AgentOmnigent)
			require.True(t, ok)
			cfg := ProviderConfig{
				Roots: []string{filepath.Dir(path)}, Machine: "host",
			}
			initializeOmnigentProvider(t, factory.NewProvider(cfg), archiveSize)

			writer, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ? WHERE id = ?`,
				time.Now().Unix(), changedID,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			sources, err := factory.NewProvider(cfg).Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, VirtualSourcePath(path, changedID), sources[0].DisplayPath)

			cfg.ForceFullDiscovery = true
			sources, err = factory.NewProvider(cfg).Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, path, sources[0].DisplayPath)
		})
	}
}

func TestOmnigentColdEmptyChangedPathReconcilesAuthoritatively(t *testing.T) {
	path := writeOmnigentCardinalityDB(t, 0)
	factory, ok := ProviderFactoryByType(AgentOmnigent)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	changed, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
	outcome, err := provider.Parse(
		context.Background(), ParseRequest{Source: changed[0]},
	)
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
}

func TestOmnigentChangedPathCancellationDoesNotAdvanceFloor(t *testing.T) {
	path := writeOmnigentCardinalityDB(t, 5)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	schema, err := detectOmnigentSchema(conn)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	tracker := newOmnigentChangeTracker()
	tracker.containers[path] = omnigentTrackedContainer{
		schema: schema, checkedAt: 5,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	changed, err := tracker.changedMembers(
		ctx, filepath.Dir(path), ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, changed)
	tracker.mu.Lock()
	floor := tracker.containers[path].checkedAt
	tracker.mu.Unlock()
	assert.EqualValues(t, 5, floor,
		"a failed sweep must not advance past unobserved changes")
}

func TestOmnigentContainerParseHonorsCancellationAfterStart(t *testing.T) {
	path := writeOmnigentCardinalityDB(t, 5)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	base, cancel := context.WithCancel(t.Context())
	parseContext := &omnigentParseCancellationContext{
		Context:    base,
		firstCheck: make(chan struct{}),
		release:    make(chan struct{}),
	}
	type parseResult struct {
		outcome ParseOutcome
		err     error
	}
	result := make(chan parseResult, 1)
	go func() {
		outcome, parseErr := provider.Parse(
			parseContext, ParseRequest{Source: sources[0]},
		)
		result <- parseResult{outcome: outcome, err: parseErr}
	}()

	<-parseContext.firstCheck
	cancel()
	close(parseContext.release)
	select {
	case got := <-result:
		require.ErrorIs(t, got.err, context.Canceled)
		assert.Empty(t, got.outcome.Results)
	case <-time.After(2 * time.Second):
		t.Fatal("Omnigent container parse did not stop after cancellation")
	}
}

func TestOmnigentWarmEventsDeferStoredHintDeletionReconciliation(t *testing.T) {
	path := writeOmnigentCardinalityDB(t, 65)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	initializeOmnigentProvider(t, provider, 65)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	for _, id := range []string{"conv_031", "conv_032"} {
		_, err = writer.Exec(
			`DELETE FROM conversation_items WHERE conversation_id = ?`, id,
		)
		require.NoError(t, err)
		_, err = writer.Exec(`DELETE FROM conversations WHERE id = ?`, id)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	first, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: path, EventKind: "write",
			StoredSourcePaths: []string{
				VirtualSourcePath(path, "conv_031"),
				VirtualSourcePath(path, "conv_032"),
			},
		},
	)
	require.NoError(t, err)
	assert.Empty(t, first,
		"warm events must not scan stored hints to prove deletions")
}

func TestOmnigentPresentStoredHintsDoNotFanOut(t *testing.T) {
	path := writeOmnigentCardinalityDB(t, 200)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	schema, err := detectOmnigentSchema(conn)
	require.NoError(t, err)
	metas, err := listOmnigentConversationMetas(t.Context(), conn, schema)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Len(t, metas, 200)

	hints := make([]string, 0, len(metas))
	for _, meta := range metas {
		hints = append(hints, VirtualSourcePath(path, meta.member().key(schema)))
	}

	tracker := newOmnigentChangeTracker()
	tracker.containers[path] = omnigentTrackedContainer{
		schema: schema, checkedAt: time.Now().Unix(),
	}
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = 'conv_005'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	changed, err := tracker.changedMembers(
		context.Background(), filepath.Dir(path), ChangedPathRequest{
			Path: path, EventKind: "write", StoredSourcePaths: hints,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1,
		"stored hints for present members must not scale event fan-out "+
			"with archive size")
	assert.Equal(t, "conv_005", changed[0].MemberID)
}

func TestOmnigentSplitWorkspaceChangedPathClassification(t *testing.T) {
	path := writeOmnigentSplitWorkspaceCardinalityDB(t, 100)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	schema, err := detectOmnigentSchema(conn)
	require.NoError(t, err)
	metas, err := listOmnigentConversationMetas(t.Context(), conn, schema)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NotEmpty(t, metas)
	tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	changedAt := time.Now().Unix()
	_, err = writer.Exec(
		`UPDATE conversations SET updated_at = ? WHERE workspace_id = 99 AND id = 'conv'`,
		changedAt)
	require.NoError(t, err)
	_, err = writer.Exec(`
		INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
		VALUES
			(99, 'conv', 'changed', 1, 1,
			 '{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
			 'changed')`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	changed, err := tracker.changedMembers(
		context.Background(), filepath.Dir(path), ChangedPathRequest{
			Path: path, EventKind: "write",
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, "99:conv", changed[0].MemberID)
}

func TestOmnigentSplitMetadataOnlyChangesAreDiscovered(t *testing.T) {
	path := writeOmnigentSplitGenDB(t)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	initializeOmnigentProvider(t, provider, 2)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations
		    SET title = 'renamed task', updated_at = ?
		  WHERE workspace_id = 0 AND id = 'conv_root'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE omnigent_conversation_metadata
		    SET workspace = '/work/renamed', git_branch = 'review',
		        session_usage = '{"input_tokens":7,"output_tokens":3}'
		  WHERE workspace_id = 0 AND id = 'conv_root'`,
	)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE agent_configuration
		    SET model_override = 'metadata-model'
		  WHERE workspace_id = 0 AND conversation_id = 'conv_root'`,
	)
	require.NoError(t, err)

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, len(sources), omnigentRecentMemberLimit,
		"metadata fallback fan-out must stay capped")
	sourceIndex := slices.IndexFunc(sources, func(source SourceRef) bool {
		return strings.HasSuffix(source.Key, "#0:conv_root")
	})
	require.NotEqual(t, -1, sourceIndex,
		"an in-place conversation update must identify its existing member")
	outcome, err := provider.Parse(
		t.Context(), ParseRequest{Source: sources[sourceIndex]},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "renamed task", result.Session.SessionName)
	assert.Equal(t, "/work/renamed", result.Session.Cwd)
	assert.Equal(t, "review", result.Session.GitBranch)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, "metadata-model", result.UsageEvents[0].Model)

	_, err = writer.Exec(
		`UPDATE omnigent_conversation_metadata
		    SET session_usage = ?
		  WHERE workspace_id = 0 AND id = 'conv_root'`,
		`{"input_tokens":9,"output_tokens":4}`,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	sources, err = provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, len(sources), omnigentRecentMemberLimit,
		"metadata fallback fan-out must stay capped")
	sourceIndex = slices.IndexFunc(sources, func(source SourceRef) bool {
		return strings.HasSuffix(source.Key, "#0:conv_root")
	})
	require.NotEqual(t, -1, sourceIndex,
		"a recent member must be replayed for metadata commits without a cursor")
	outcome, err = provider.Parse(
		t.Context(), ParseRequest{Source: sources[sourceIndex]},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	require.Len(t, outcome.Results[0].Result.UsageEvents, 1)
	assert.Equal(t, 9, outcome.Results[0].Result.UsageEvents[0].InputTokens)
	assert.Equal(t, 4, outcome.Results[0].Result.UsageEvents[0].OutputTokens)
}

func TestOmnigentSplitWorkspaceChangeWorkIsArchiveIndependent(t *testing.T) {
	for _, archiveSize := range []int{100, 600} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentSplitWorkspaceCardinalityDB(t, archiveSize)
			conn, err := openOmnigentDB(path)
			require.NoError(t, err)
			schema, err := detectOmnigentSchema(conn)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)
			workspaceID := archiveSize - 100

			writer, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE conversations SET updated_at = ?
				 WHERE workspace_id = ? AND id = 'conv'`,
				time.Now().Unix(), workspaceID,
			)
			require.NoError(t, err)
			_, err = writer.Exec(`
				INSERT INTO conversation_items
					(workspace_id, conversation_id, id, position, type, data, search_text)
				VALUES
					(?, 'conv', 'changed', 1, 1,
					 '{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
					 'changed')`, workspaceID)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			changed, err := tracker.changedMembers(
				t.Context(), filepath.Dir(path), ChangedPathRequest{
					Path: path, EventKind: "write",
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1,
				"one appended item must fan out one member at every archive size")
			assert.Equal(t, fmt.Sprintf("%d:conv", workspaceID), changed[0].MemberID)
		})
	}
}

func TestOmnigentSplitMetadataChangeWorkIsArchiveIndependent(t *testing.T) {
	for _, archiveSize := range []int{100, 600} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentSplitSingleWorkspaceCardinalityDB(t, archiveSize)
			conn, err := openOmnigentDB(path)
			require.NoError(t, err)
			schema, err := detectOmnigentSchema(conn)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)

			writer, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			target := fmt.Sprintf("conv_%03d", archiveSize/2)
			_, err = writer.Exec(
				`UPDATE conversations SET title = 'changed', updated_at = ?
				  WHERE workspace_id = 0 AND id = ?`,
				time.Now().Unix(), target,
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			changed, err := tracker.changedMembers(
				t.Context(), filepath.Dir(path), ChangedPathRequest{
					Path: path, EventKind: "write",
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1,
				"one metadata update must fan out one member at every archive size")
			assert.Equal(t, "0:"+target, changed[0].MemberID)
		})
	}
}

func TestOmnigentSplitMultiWorkspaceMetadataUpdateOutsideRecentReplay(
	t *testing.T,
) {
	for _, archiveSize := range []int{140, 600} {
		for _, warmState := range []string{"full_parse", "cache_restore"} {
			t.Run(fmt.Sprintf("%s/archive_%d", warmState, archiveSize),
				func(t *testing.T) {
					path := writeOmnigentSplitTwoWorkspaceCardinalityDB(
						t, archiveSize,
					)
					provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
						Roots: []string{filepath.Dir(path)}, Machine: "host",
					})
					require.True(t, ok)
					sources, err := provider.Discover(t.Context())
					require.NoError(t, err)
					require.Len(t, sources, 1)
					if warmState == "full_parse" {
						outcome, parseErr := provider.Parse(
							t.Context(), ParseRequest{Source: sources[0]},
						)
						require.NoError(t, parseErr)
						require.Len(t, outcome.Results, archiveSize)
					} else {
						restorer, ok := provider.(CachedSourceStateRestorer)
						require.True(t, ok)
						restored, restoreErr := restorer.RestoreCachedSourceState(
							t.Context(), sources[0],
						)
						require.NoError(t, restoreErr)
						require.True(t, restored)
					}
					ageOmnigentTrackerPastRecentReplay(t, provider, path)

					writer, err := sql.Open("sqlite3", path)
					require.NoError(t, err)
					changedAt := time.Now().Unix()
					targets := []struct {
						workspaceID int64
						id          string
						title       string
						model       string
					}{
						{
							workspaceID: 0,
							id:          "conv_000",
							title:       "renamed workspace zero",
							model:       "updated-model-zero",
						},
						{
							workspaceID: 1,
							id:          "conv_001",
							title:       "renamed workspace one",
							model:       "updated-model-one",
						},
					}
					for _, target := range targets {
						_, err = writer.Exec(
							`UPDATE conversations
							    SET title = ?,
							        session_overrides = ?,
							        updated_at = ?
							  WHERE workspace_id = ? AND id = ?`,
							target.title,
							fmt.Sprintf(
								`{"model_override":%q}`, target.model,
							),
							changedAt, target.workspaceID, target.id,
						)
						require.NoError(t, err)
					}
					require.NoError(t, writer.Close())

					sources, err = provider.SourcesForChangedPath(
						t.Context(), ChangedPathRequest{
							Path: path, EventKind: "write",
						},
					)
					require.NoError(t, err)
					require.Len(t, sources, len(targets),
						"two metadata updates must emit two members "+
							"independently of conversation cardinality")
					for _, target := range targets {
						memberKey := fmt.Sprintf(
							"#%d:%s", target.workspaceID, target.id,
						)
						targetIndex := slices.IndexFunc(
							sources, func(source SourceRef) bool {
								return strings.HasSuffix(
									source.Key, memberKey,
								)
							},
						)
						require.NotEqual(t, -1, targetIndex,
							"the workspace cursor must find metadata-only "+
								"updates outside the capped recent replay")
						outcome, err := provider.Parse(
							t.Context(), ParseRequest{
								Source: sources[targetIndex],
							},
						)
						require.NoError(t, err)
						require.Len(t, outcome.Results, 1)
						result := outcome.Results[0].Result
						assert.Equal(t, target.title,
							result.Session.SessionName)
						require.Len(t, result.UsageEvents, 1)
						assert.Equal(t, target.model,
							result.UsageEvents[0].Model)
					}
				},
			)
		}
	}
}

func TestOmnigentSplitWorkspaceRoundRobinEventuallyFindsMetadataUpdate(
	t *testing.T,
) {
	workspaceCount := 2*omnigentWorkspaceProbeLimit + 5
	targetWorkspace := int64(2*omnigentWorkspaceProbeLimit + 1)
	path := writeOmnigentSplitWorkspaceCardinalityDB(t, workspaceCount)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	initializeOmnigentProvider(t, provider, workspaceCount)
	ageOmnigentTrackerPastRecentReplay(t, provider, path)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations
		    SET title = 'eventually found', updated_at = ?
		  WHERE workspace_id = ? AND id = 'conv'`,
		time.Now().Unix(), targetWorkspace,
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	cyclePasses := (workspaceCount + omnigentWorkspaceProbeLimit - 1) /
		omnigentWorkspaceProbeLimit
	foundPass := -1
	for pass := range cyclePasses {
		var probes []int64
		ctx := withOmnigentWorkspaceProbeObserver(
			t.Context(), func(workspaceID int64) {
				probes = append(probes, workspaceID)
			},
		)
		sources, changedErr := provider.SourcesForChangedPath(
			ctx, ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(t, changedErr)
		assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
			"every round-robin pass must have fixed workspace work")
		targetIndex := slices.IndexFunc(sources, func(source SourceRef) bool {
			return strings.HasSuffix(
				source.Key, fmt.Sprintf("#%d:conv", targetWorkspace),
			)
		})
		if pass == 0 {
			assert.Equal(t, -1, targetIndex,
				"a workspace beyond the first probe window must wait "+
					"for a later bounded pass")
		}
		if targetIndex >= 0 && foundPass < 0 {
			foundPass = pass
		}
	}
	assert.Greater(t, foundPass, 0,
		"the updated workspace must not be emitted by the first window")
	assert.Less(t, foundPass, cyclePasses,
		"the updated workspace must be emitted within one bounded cycle")
}

func TestOmnigentSplitWorkspaceSweepWrapsDespiteSustainedInsertion(
	t *testing.T,
) {
	const initialWorkspaceCount = omnigentWorkspaceProbeLimit + 1
	path := writeOmnigentSplitWorkspaceCardinalityDB(
		t, initialWorkspaceCount,
	)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	initializeOmnigentProvider(t, provider, initialWorkspaceCount)
	ageOmnigentTrackerPastRecentReplay(t, provider, path)

	var firstProbes []int64
	ctx := withOmnigentWorkspaceProbeObserver(
		t.Context(), func(workspaceID int64) {
			firstProbes = append(firstProbes, workspaceID)
		},
	)
	sources, err := provider.SourcesForChangedPath(
		ctx, ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.NoError(t, err)
	assert.Empty(t, sources)
	require.Len(t, firstProbes, omnigentWorkspaceProbeLimit)
	assert.EqualValues(t, 0, firstProbes[0])
	assert.EqualValues(t, omnigentWorkspaceProbeLimit-1,
		firstProbes[len(firstProbes)-1])

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE conversations
		    SET title = 'must survive workspace churn', updated_at = ?
		  WHERE workspace_id = 0 AND id = 'conv'`,
		time.Now().Unix(),
	)
	require.NoError(t, err)

	nextWorkspaceID := int64(initialWorkspaceCount)
	foundPass := -1
	for pass := range 4 {
		for range omnigentWorkspaceProbeLimit {
			workspaceID := nextWorkspaceID
			nextWorkspaceID++
			_, err = writer.Exec(`INSERT INTO conversations
				(workspace_id, id, created_at, updated_at, title,
				 root_conversation_id)
				VALUES (?, 'conv', 1, ?, 'inserted', 'conv')`,
				workspaceID, time.Now().Unix())
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO omnigent_conversation_metadata
				(workspace_id, id, kind, workspace)
				VALUES (?, 'conv', 1, ?)`,
				workspaceID, fmt.Sprintf("/work/%d", workspaceID))
			require.NoError(t, err)
			_, err = writer.Exec(`INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data,
				 search_text)
				VALUES (?, 'conv', 'item', 0, 1,
				 '{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
				 'hi')`, workspaceID)
			require.NoError(t, err)
		}

		var probes []int64
		ctx = withOmnigentWorkspaceProbeObserver(
			t.Context(), func(workspaceID int64) {
				probes = append(probes, workspaceID)
			},
		)
		sources, err = provider.SourcesForChangedPath(
			ctx, ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
			"pass %d must keep workspace probes bounded", pass)
		if slices.ContainsFunc(sources, func(source SourceRef) bool {
			return strings.HasSuffix(source.Key, "#0:conv")
		}) {
			foundPass = pass
			break
		}
	}
	require.NoError(t, writer.Close())
	assert.NotEqual(t, -1, foundPass,
		"a cycle boundary must revisit an already-probed workspace even "+
			"when every later pass adds a full window of higher workspace IDs")
}

func TestOmnigentCurrentBinaryWorkspaceSweepAcrossCacheRestoreAndWrap(
	t *testing.T,
) {
	const (
		workspaceCount    = 2*omnigentWorkspaceProbeLimit + 3
		archivedWorkspace = int64(17)
		liveWorkspace     = int64(34)
		deletedWorkspace  = int64(12)
		insertedWorkspace = int64(100)
	)
	path := writeOmnigentCurrentWorkspaceSweepDB(t, workspaceCount)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	restorer, ok := provider.(CachedSourceStateRestorer)
	require.True(t, ok)
	restored, err := restorer.RestoreCachedSourceState(
		t.Context(), sources[0],
	)
	require.NoError(t, err)
	require.True(t, restored)
	ageOmnigentTrackerPastRecentReplay(t, provider, path)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	for _, update := range []struct {
		workspaceID int64
		title       string
	}{
		{workspaceID: archivedWorkspace, title: "archived metadata update"},
		{workspaceID: liveWorkspace, title: "live metadata update"},
	} {
		_, err = writer.Exec(
			`UPDATE conversations
			    SET title = ?, updated_at = ?
			  WHERE workspace_id = ? AND id = ?`,
			update.title, time.Now().Unix(), update.workspaceID,
			omnigentHexBytes(
				t, omnigentCurrentWorkspaceConversationHex(update.workspaceID),
			),
		)
		require.NoError(t, err)
	}
	for _, table := range []string{
		"conversation_items",
		"omnigent_conversation_metadata",
		"conversations",
	} {
		_, err = writer.Exec(
			`DELETE FROM `+table+` WHERE workspace_id = ?`,
			deletedWorkspace,
		)
		require.NoError(t, err)
	}
	insertOmnigentCurrentWorkspace(
		t, writer, insertedWorkspace, true, time.Now().Unix(),
		"inserted workspace",
	)
	require.NoError(t, writer.Close())

	wantTitles := map[int64]string{
		archivedWorkspace: "archived metadata update",
		liveWorkspace:     "live metadata update",
		insertedWorkspace: "inserted workspace",
	}
	deletedMemberSuffix := fmt.Sprintf(
		"#%d:%s", deletedWorkspace,
		omnigentCurrentWorkspaceConversationHex(deletedWorkspace),
	)
	found := make(map[int64]SourceRef, len(wantTitles))
	var passProbes [][]int64
	for pass := range 4 {
		var probes []int64
		ctx := withOmnigentWorkspaceProbeObserver(
			t.Context(), func(workspaceID int64) {
				probes = append(probes, workspaceID)
			},
		)
		sources, err = provider.SourcesForChangedPath(
			ctx, ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
			"pass %d must keep current-schema probes bounded", pass)
		assert.NotContains(t, probes, deletedWorkspace,
			"deleted workspaces must leave the live sweep immediately")
		passProbes = append(passProbes, probes)
		assert.Equal(t, -1, slices.IndexFunc(
			sources, func(source SourceRef) bool {
				return strings.HasSuffix(source.Key, deletedMemberSuffix)
			},
		), "a warm bounded pass must not emit a deleted member as present")
		for workspaceID := range wantTitles {
			memberSuffix := fmt.Sprintf(
				"#%d:%s", workspaceID,
				omnigentCurrentWorkspaceConversationHex(workspaceID),
			)
			if index := slices.IndexFunc(
				sources, func(source SourceRef) bool {
					return strings.HasSuffix(source.Key, memberSuffix)
				},
			); index >= 0 {
				found[workspaceID] = sources[index]
			}
		}
	}

	require.Len(t, passProbes, 4)
	require.NotEmpty(t, passProbes[0])
	require.NotEmpty(t, passProbes[3])
	assert.EqualValues(t, 0, passProbes[0][0])
	assert.EqualValues(t, 0, passProbes[3][0],
		"the fourth pass must prove the workspace cursor wrapped")
	for workspaceID, wantTitle := range wantTitles {
		source, exists := found[workspaceID]
		require.Truef(t, exists,
			"workspace %d must be emitted by the bounded sweep", workspaceID)
		outcome, parseErr := provider.Parse(
			t.Context(), ParseRequest{Source: source},
		)
		require.NoError(t, parseErr)
		require.Len(t, outcome.Results, 1)
		assert.Equal(t, wantTitle,
			outcome.Results[0].Result.Session.SessionName)
	}
}

func TestOmnigentSplitWorkspaceProbeWorkIsCardinalityIndependent(
	t *testing.T,
) {
	var probeCounts []int
	for _, workspaceCount := range []int{40, 600} {
		t.Run(fmt.Sprintf("workspaces_%d", workspaceCount), func(t *testing.T) {
			path := writeOmnigentSplitWorkspaceCardinalityDB(t, workspaceCount)
			provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
				Roots: []string{filepath.Dir(path)}, Machine: "host",
			})
			require.True(t, ok)
			initializeOmnigentProvider(t, provider, workspaceCount)
			ageOmnigentTrackerPastRecentReplay(t, provider, path)

			var probes []int64
			ctx := withOmnigentWorkspaceProbeObserver(
				t.Context(), func(workspaceID int64) {
					probes = append(probes, workspaceID)
				},
			)
			sources, err := provider.SourcesForChangedPath(
				ctx, ChangedPathRequest{Path: path, EventKind: "write"},
			)
			require.NoError(t, err)
			assert.Empty(t, sources)
			assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
				"unchanged event work must be capped as workspaces grow")
			probeCounts = append(probeCounts, len(probes))
		})
	}
	require.Len(t, probeCounts, 2)
	assert.Equal(t, probeCounts[0], probeCounts[1],
		"small and large current workspace sets must do equal bounded work")
}

func TestOmnigentSplitWorkspaceProbeDropsDeletedWorkspaces(t *testing.T) {
	const workspaceCount = 300
	remainingWorkspace := int64(workspaceCount - 1)
	path := writeOmnigentSplitWorkspaceCardinalityDB(t, workspaceCount)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	initializeOmnigentProvider(t, provider, workspaceCount)
	ageOmnigentTrackerPastRecentReplay(t, provider, path)

	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	for _, table := range []string{
		"conversation_items",
		"omnigent_conversation_metadata",
		"conversations",
	} {
		_, err = writer.Exec(
			`DELETE FROM `+table+` WHERE workspace_id <> ?`,
			remainingWorkspace,
		)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	for pass := range 3 {
		var probes []int64
		ctx := withOmnigentWorkspaceProbeObserver(
			t.Context(), func(workspaceID int64) {
				probes = append(probes, workspaceID)
			},
		)
		_, err = provider.SourcesForChangedPath(
			ctx, ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
			"pass %d must not revisit the historical workspace archive", pass)
		assert.Equal(t, 1, len(probes),
			"pass %d must retain only the live workspace", pass)
		if len(probes) > 0 {
			assert.Equal(t, remainingWorkspace, probes[0])
		}
	}
}

func TestOmnigentSplitWorkspaceCacheRestoreProbeIsBounded(t *testing.T) {
	const workspaceCount = 600
	path := writeOmnigentSplitWorkspaceCardinalityDB(t, workspaceCount)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	restorer, ok := provider.(CachedSourceStateRestorer)
	require.True(t, ok)

	var probes []int64
	ctx := withOmnigentWorkspaceProbeObserver(
		t.Context(), func(workspaceID int64) {
			probes = append(probes, workspaceID)
		},
	)
	restored, err := restorer.RestoreCachedSourceState(ctx, sources[0])
	require.NoError(t, err)
	require.True(t, restored)
	assert.LessOrEqual(t, len(probes), omnigentWorkspaceProbeLimit,
		"cache restoration must not probe the whole workspace archive")
}

func TestOmnigentSplitRecentMetadataReplayIsCapped(t *testing.T) {
	for _, archiveSize := range []int{200, 600} {
		t.Run(fmt.Sprintf("archive_%d", archiveSize), func(t *testing.T) {
			path := writeOmnigentSplitSingleWorkspaceCardinalityDB(t, archiveSize)
			provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
				Roots: []string{filepath.Dir(path)}, Machine: "host",
			})
			require.True(t, ok)
			initializeOmnigentProvider(t, provider, archiveSize)

			sources, err := provider.SourcesForChangedPath(
				t.Context(), ChangedPathRequest{
					Path: path, EventKind: "write",
				},
			)
			require.NoError(t, err)
			assert.Len(t, sources, omnigentRecentMemberLimit,
				"warm metadata fallback must stay fixed as the archive grows")
		})
	}
}

func TestOmnigentSplitWorkspaceReusedTailRowIDsAreRecovered(t *testing.T) {
	t.Run("item", func(t *testing.T) {
		path := writeOmnigentSplitWorkspaceCardinalityDB(t, 10)
		conn, err := openOmnigentDB(path)
		require.NoError(t, err)
		schema, err := detectOmnigentSchema(conn)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)
		tracked := tracker.containers[path]

		writer, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = writer.Exec(
			`DELETE FROM conversation_items WHERE workspace_id >= 5`,
		)
		require.NoError(t, err)
		_, err = writer.Exec(`
			INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES
				(0, 'conv', 'replacement', 1, 1,
				 '{"role":"assistant","content":[{"type":"output_text","text":"replacement"}]}',
				 'replacement'),
				(1, 'conv', 'replacement-2', 1, 1,
				 '{"role":"assistant","content":[{"type":"output_text","text":"replacement-2"}]}',
				 'replacement-2')`)
		require.NoError(t, err)
		var replacementRowID int64
		require.NoError(t, writer.QueryRow(
			`SELECT rowid FROM conversation_items WHERE id = 'replacement'`,
		).Scan(&replacementRowID))
		require.Less(t, replacementRowID, tracked.itemRowID,
			"the regression requires a shortened tail with a reused rowid")
		require.Equal(t, tracked.itemRowID-4, replacementRowID)
		require.NoError(t, writer.Close())

		changed, err := tracker.changedMembers(
			t.Context(), filepath.Dir(path), ChangedPathRequest{
				Path: path, EventKind: "write",
			},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Empty(t, changed[0].MemberID,
			"a lowered rowid epoch requires authoritative container reconciliation")
		assert.Equal(t, path, changed[0].Container)
	})

	t.Run("conversation", func(t *testing.T) {
		path := writeOmnigentSplitWorkspaceCardinalityDB(t, 2)
		conn, err := openOmnigentDB(path)
		require.NoError(t, err)
		schema, err := detectOmnigentSchema(conn)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)
		tracked := tracker.containers[path]

		writer, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = writer.Exec(
			`DELETE FROM conversation_items WHERE workspace_id = 1`,
		)
		require.NoError(t, err)
		_, err = writer.Exec(
			`DELETE FROM omnigent_conversation_metadata WHERE workspace_id = 1`,
		)
		require.NoError(t, err)
		_, err = writer.Exec(`DELETE FROM conversations WHERE workspace_id = 1`)
		require.NoError(t, err)
		_, err = writer.Exec(`
			INSERT INTO conversations
				(workspace_id, id, created_at, updated_at, title,
				 root_conversation_id, next_position)
			VALUES (99, 'new', 1, 2, 'new', 'new', 1)`)
		require.NoError(t, err)
		_, err = writer.Exec(`
			INSERT INTO omnigent_conversation_metadata
				(workspace_id, id, kind, workspace)
			VALUES (99, 'new', 1, '/work/99')`)
		require.NoError(t, err)
		_, err = writer.Exec(`
			INSERT INTO conversation_items
				(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES
				(99, 'new', 'replacement', 0, 1,
				 '{"role":"user","content":[{"type":"input_text","text":"new"}]}',
				 'new')`)
		require.NoError(t, err)
		var replacementRowID int64
		require.NoError(t, writer.QueryRow(
			`SELECT rowid FROM conversations WHERE workspace_id = 99`,
		).Scan(&replacementRowID))
		require.Equal(t, tracked.conversationRowID, replacementRowID,
			"the regression requires SQLite to reuse the deleted tail rowid")
		require.NoError(t, writer.Close())

		changed, err := tracker.changedMembers(
			t.Context(), filepath.Dir(path), ChangedPathRequest{
				Path: path, EventKind: "write",
			},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, "99:new", changed[0].MemberID)
	})
}

func omnigentTrackerAtCurrentHighWater(
	t *testing.T, path string, schema omnigentSchema,
) *omnigentChangeTracker {
	t.Helper()
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	defer conn.Close()
	conversationRowID, conversationTail, err := omnigentLatestConversationRow(
		t.Context(), conn, schema,
	)
	require.NoError(t, err)
	itemRowID, itemTail, err := omnigentLatestItemRow(t.Context(), conn, schema)
	require.NoError(t, err)
	checkedAt := time.Now().Unix()
	tracker := newOmnigentChangeTracker()
	tracker.containers[path] = omnigentTrackedContainer{
		schema:                  schema,
		checkedAt:               checkedAt,
		workspaceSweepFloor:     checkedAt,
		workspaceSweepStartedAt: checkedAt,
		conversationRowID:       conversationRowID,
		conversationTail:        conversationTail,
		itemRowID:               itemRowID,
		itemTail:                itemTail,
	}
	return tracker
}

func TestOmnigentIncrementalQueriesUseSeekableIndexes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeDB    func(*testing.T, int) string
		query      func(omnigentSchema) string
		args       []any
		wantDetail string
		wantItems  string
	}{
		{
			name: "old schema updated_at range",
			writeDB: func(t *testing.T, count int) string {
				return writeOmnigentCardinalityDB(t, count)
			},
			query:      omnigentChangedMetaQuery,
			args:       []any{int64(1), int64(2)},
			wantDetail: "ix_conversations_updated_at",
			wantItems:  "ix_conversation_items_conversation_id_position",
		},
		{
			name:       "split schema new conversation rows",
			writeDB:    writeOmnigentSplitWorkspaceCardinalityDB,
			query:      omnigentNewConversationQuery,
			args:       []any{int64(0), 128},
			wantDetail: "INTEGER PRIMARY KEY",
			wantItems:  "ix_conversation_items_conversation_id_position",
		},
		{
			name:       "split schema updated_at range",
			writeDB:    writeOmnigentSplitSingleWorkspaceCardinalityDB,
			query:      omnigentSplitChangedMetaQuery,
			args:       []any{int64(0), int64(1), int64(2)},
			wantDetail: "ix_conversations_updated_at",
			wantItems:  "ix_conversation_items_conversation_id_position",
		},
		{
			name: "current split schema updated_at range",
			writeDB: func(t *testing.T, _ int) string {
				return writeOmnigentBinaryIDDB(t)
			},
			query: omnigentSplitChangedMetaQuery,
			args: []any{
				int64(0), int64(1), int64(2),
				int64(0), int64(1), int64(2),
			},
			wantDetail: "ix_conversations_archived_updated",
			wantItems:  "ix_conversation_items_conversation_id_position",
		},
		{
			name: "current split schema workspace successor",
			writeDB: func(t *testing.T, _ int) string {
				return writeOmnigentBinaryIDDB(t)
			},
			query: func(schema omnigentSchema) string {
				return omnigentNextWorkspaceIDQuery(schema, true)
			},
			args:       []any{int64(0), int64(100)},
			wantDetail: "ix_conversations_archived_updated",
		},
		{
			name: "current split schema workspace cycle ceiling",
			writeDB: func(t *testing.T, _ int) string {
				return writeOmnigentBinaryIDDB(t)
			},
			query:      omnigentWorkspaceCycleCeilingQuery,
			wantDetail: "ix_conversations_archived_updated",
		},
		{
			name:       "split schema new item rows",
			writeDB:    writeOmnigentSplitWorkspaceCardinalityDB,
			query:      omnigentNewItemQuery,
			args:       []any{int64(0), 128},
			wantDetail: "INTEGER PRIMARY KEY",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.writeDB(t, 300)
			conn, err := openOmnigentDB(path)
			require.NoError(t, err)
			defer conn.Close()
			schema, err := detectOmnigentSchema(conn)
			require.NoError(t, err)

			rows, err := conn.QueryContext(
				t.Context(), "EXPLAIN QUERY PLAN "+tc.query(schema), tc.args...,
			)
			require.NoError(t, err)
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			plan := strings.ToUpper(strings.Join(details, "\n"))
			assert.Contains(t, plan, strings.ToUpper(tc.wantDetail))
			if tc.wantItems != "" {
				assert.Contains(t, plan, strings.ToUpper(tc.wantItems))
			}
			assert.NotContains(t, plan, "SCAN CONVERSATIONS")
			assert.NotContains(t, plan, "SCAN CONVERSATION_ITEMS",
				"incremental discovery must not scan the full item archive")
			assert.NotContains(t, plan, "AUTOMATIC",
				"an ephemeral item index would still scan the archive to build")
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
		if i == count-1 {
			updatedAt = 4_000_000_000
		}
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

func initializeOmnigentProvider(t *testing.T, provider Provider, want int) {
	t.Helper()
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(
		context.Background(), ParseRequest{Source: sources[0]},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, want)
}

func ageOmnigentTrackerPastRecentReplay(
	t *testing.T, provider Provider, path string,
) {
	t.Helper()
	wrapped, ok := provider.(*SourceSetProvider)
	require.True(t, ok)
	sourceSet, ok := wrapped.sources.(omnigentSourceSet)
	require.True(t, ok)
	tracker := sourceSet.tracker
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracked, ok := tracker.containers[path]
	require.True(t, ok)
	agedAt := time.Now().Unix() -
		int64(omnigentRecentMemberTTL/time.Second) - 1
	tracked.checkedAt = agedAt
	for i := range tracked.recentMembers {
		tracked.recentMembers[i].observedAt = agedAt
	}
	tracker.containers[path] = tracked
}

func writeOmnigentSplitWorkspaceCardinalityDB(t *testing.T, count int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentSplitGenDDL)
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('workspace-cardinality')`)
	require.NoError(t, err)
	for workspaceID := range count {
		updatedAt := int64(1_700_000_000 + workspaceID)
		_, err = database.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (?, 'conv', ?, ?, 'conversation', 'conv')`,
			workspaceID, updatedAt-1, updatedAt)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (?, 'conv', 1, '/work/project')`, workspaceID)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (?, 'conv', 'item', 0, 1,
				'{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
				'hi')`, workspaceID)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func writeOmnigentSplitSingleWorkspaceCardinalityDB(
	t *testing.T, count int,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentSplitGenDDL)
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('single-workspace-cardinality')`)
	require.NoError(t, err)
	for i := range count {
		id := fmt.Sprintf("conv_%03d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = database.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title, root_conversation_id)
			VALUES (0, ?, ?, ?, 'conversation', ?)`,
			id, updatedAt-1, updatedAt, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, workspace)
			VALUES (0, ?, 1, '/work/project')`, id)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (0, ?, 'item', 0, 1,
				'{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
				'hi')`, id)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func writeOmnigentSplitTwoWorkspaceCardinalityDB(
	t *testing.T, count int,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentSplitGenDDL)
	_, err = database.Exec(
		`ALTER TABLE conversations ADD COLUMN session_overrides TEXT`,
	)
	require.NoError(t, err)
	_, err = database.Exec(
		`INSERT INTO alembic_version VALUES ('two-workspace-cardinality')`,
	)
	require.NoError(t, err)
	for i := range count {
		workspaceID := i % 2
		id := fmt.Sprintf("conv_%03d", i)
		updatedAt := int64(1_700_000_000 + i)
		_, err = database.Exec(`INSERT INTO conversations
			(workspace_id, id, created_at, updated_at, title,
			 root_conversation_id, session_overrides)
			VALUES (?, ?, ?, ?, 'conversation', ?, ?)`,
			workspaceID, id, updatedAt-1, updatedAt, id,
			`{"model_override":"original-model"}`)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
			(workspace_id, id, kind, session_usage, workspace)
			VALUES (?, ?, 1, ?, '/work/project')`,
			workspaceID, id, `{"input_tokens":1,"output_tokens":2}`)
		require.NoError(t, err)
		_, err = database.Exec(`INSERT INTO conversation_items
			(workspace_id, conversation_id, id, position, type, data, search_text)
			VALUES (?, ?, 'item', 0, 1,
				'{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
				'hi')`, workspaceID, id)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func omnigentCurrentWorkspaceConversationHex(workspaceID int64) string {
	return fmt.Sprintf("%032x", workspaceID+1)
}

func omnigentCurrentWorkspaceItemHex(workspaceID int64) string {
	return fmt.Sprintf("%032x", 1_000_000+workspaceID)
}

func insertOmnigentCurrentWorkspace(
	t *testing.T,
	database *sql.DB,
	workspaceID int64,
	archived bool,
	updatedAt int64,
	title string,
) {
	t.Helper()
	conversationID := omnigentHexBytes(
		t, omnigentCurrentWorkspaceConversationHex(workspaceID),
	)
	_, err := database.Exec(`INSERT INTO conversations
		(workspace_id, id, created_at, updated_at, title,
		 root_conversation_id, session_overrides, archived)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		workspaceID, conversationID, updatedAt-1, updatedAt, title,
		conversationID, `{"model_override":"current-model"}`, archived)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
		(workspace_id, id, kind, workspace)
		VALUES (?, ?, 1, ?)`,
		workspaceID, conversationID, fmt.Sprintf("/work/%d", workspaceID))
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversation_items
		(workspace_id, id, conversation_id, response_id, created_at,
		 position, type, status, data, search_text)
		VALUES (?, ?, ?, 'response', ?, 0, 1, 1,
		 '{"role":"user","content":[{"type":"input_text","text":"hi"}]}',
		 'hi')`,
		workspaceID,
		omnigentHexBytes(t, omnigentCurrentWorkspaceItemHex(workspaceID)),
		conversationID, updatedAt)
	require.NoError(t, err)
}

func writeOmnigentCurrentWorkspaceSweepDB(
	t *testing.T, workspaceCount int,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentBinaryIDGenDDL)
	_, err = database.Exec(
		`INSERT INTO alembic_version VALUES ('current-workspace-sweep')`,
	)
	require.NoError(t, err)
	for workspaceID := range workspaceCount {
		insertOmnigentCurrentWorkspace(
			t, database, int64(workspaceID), workspaceID%2 == 1,
			int64(1_700_000_000+workspaceID), "original",
		)
	}
	require.NoError(t, database.Close())
	return path
}

// omnigentHexBytes decodes a 32-char hex conversation ID into the 16 raw
// bytes the binary-id generation stores.
func omnigentHexBytes(t *testing.T, hexID string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(hexID)
	require.NoError(t, err)
	return decoded
}

const (
	omnigentBinaryConvHex   = "3ca53ab9e60540a8aef3c1f152555889"
	omnigentBinarySubHex    = "cf1eb494e015495abebd096b3ff3ab5e"
	omnigentBinaryGoneHex   = "d6a78eb8cd7e4a6080f3ebef4346168a"
	omnigentBinaryItemHexA  = "00000000000000000000000000000001"
	omnigentBinaryItemHexB  = "00000000000000000000000000000002"
	omnigentBinaryItemHexC  = "00000000000000000000000000000003"
	omnigentBinaryItemHexD  = "00000000000000000000000000000004"
	omnigentBinaryItemHexE  = "00000000000000000000000000000005"
	omnigentBinaryAgentHex  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	omnigentBinaryUsageJSON = `{"input_tokens":1500,"output_tokens":350}`
)

// writeOmnigentBinaryIDDB builds a newest-generation database: BLOB uuid ids,
// int enum codes, split metadata, framed session_usage, and a sub-agent child.
func writeOmnigentBinaryIDDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), omnigentDBName)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	execOmnigentDDL(t, database, omnigentBinaryIDGenDDL)
	_, err = database.Exec(`INSERT INTO alembic_version VALUES ('d1e2f3a4b5c6')`)
	require.NoError(t, err)

	conv := omnigentHexBytes(t, omnigentBinaryConvHex)
	sub := omnigentHexBytes(t, omnigentBinarySubHex)
	insertConv := func(
		id, parent, root []byte, title string, updatedAt int64,
		sessionOverrides string,
	) {
		_, err = database.Exec(`INSERT INTO conversations
			(id, created_at, updated_at, title, parent_conversation_id,
			 root_conversation_id, agent_id, session_overrides)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, updatedAt-1, updatedAt, title, parent, root,
			omnigentHexBytes(t, omnigentBinaryAgentHex), sessionOverrides)
		require.NoError(t, err)
	}
	insertConv(
		conv, nil, conv, "Fix the flaky retry test", 1_700_000_010,
		`{"reasoning_effort":"high","model_override":"omnigent-large"}`,
	)
	insertConv(sub, conv, conv, "explore:codebase-map", 1_700_000_011, "")

	// session_usage uses the compression framing: sentinel + raw codec.
	framedUsage := append([]byte{0x00, 0x00}, []byte(omnigentBinaryUsageJSON)...)
	_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
		(id, kind, workspace, git_branch, session_usage)
		VALUES (?, 1, '/workspace/project-a', 'main', ?)`,
		conv, framedUsage)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO omnigent_conversation_metadata
		(id, kind, sub_agent_name, workspace)
		VALUES (?, 2, 'explorer', '/workspace/project-a')`, sub)
	require.NoError(t, err)
	insertItem := func(convID []byte, itemHex string, position, typeCode int, data, search string) {
		_, err = database.Exec(`INSERT INTO conversation_items
			(id, conversation_id, response_id, created_at, position, type,
			 status, data, search_text)
			VALUES (?, ?, 'resp_001', 1700000010, ?, ?, 1, ?, ?)`,
			omnigentHexBytes(t, itemHex), convID, position, typeCode, data, search)
		require.NoError(t, err)
	}
	insertItem(conv, omnigentBinaryItemHexA, 0, 1,
		`{"role":"user","content":[{"type":"input_text","text":"Why is the retry test flaky?"}]}`,
		"Why is the retry test flaky?")
	insertItem(conv, omnigentBinaryItemHexB, 1, 2,
		`{"model":"omnigent-large","name":"shell.run","arguments":"{\"cmd\":\"go test\"}","call_id":"call_abc123"}`,
		"")
	insertItem(conv, omnigentBinaryItemHexC, 2, 3,
		`{"call_id":"call_abc123","output":"--- FAIL: TestRetry"}`,
		"")
	insertItem(conv, omnigentBinaryItemHexD, 3, 1,
		`{"role":"assistant","model":"omnigent-large","content":[{"type":"output_text","text":"Raise the timeout."}]}`,
		"Raise the timeout.")
	insertItem(sub, omnigentBinaryItemHexE, 0, 1,
		`{"role":"user","content":[{"type":"input_text","text":"Map the retry package"}]}`,
		"Map the retry package")

	require.NoError(t, database.Close())
	return path
}

func TestOmnigentBinaryIDGenerationParses(t *testing.T) {
	path := writeOmnigentBinaryIDDB(t)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(
		context.Background(), ParseRequest{Source: sources[0]},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 2)
	assert.True(t, outcome.ResultSetComplete)

	byID := map[string]ParseResult{}
	for _, res := range outcome.Results {
		byID[res.Result.Session.ID] = res.Result
	}
	main, ok := byID[omnigentIDPrefix+"0:"+omnigentBinaryConvHex]
	require.True(t, ok, "main conversation must parse under its hex ID")
	assert.Equal(t, "Fix the flaky retry test", main.Session.SessionName)
	require.Len(t, main.Messages, 3,
		"function_call_output must fold onto its call")
	assert.Equal(t, "main", main.Session.GitBranch)
	require.Len(t, main.UsageEvents, 1,
		"framed session_usage must decode into usage events")
	assert.Equal(t, "omnigent-large", main.UsageEvents[0].Model)
	assert.Nil(t, main.UsageEvents[0].CostUSD,
		"absent total_cost_usd must stay nil so catalog pricing applies")

	sub, ok := byID[omnigentIDPrefix+"0:"+omnigentBinarySubHex]
	require.True(t, ok, "sub-agent conversation must parse under its hex ID")
	assert.Equal(t, omnigentIDPrefix+"0:"+omnigentBinaryConvHex,
		sub.Session.ParentSessionID,
		"parent linkage must survive the binary-id hex conversion")
}

func TestOmnigentBinaryIDParseOutcomeIncludesEveryTextPredecessor(t *testing.T) {
	path := writeOmnigentBinaryIDDB(t)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)

	wantExcluded := []string{
		"omnigent:" + omnigentBinaryConvHex,
		"omnigent:conv_" + omnigentBinaryConvHex,
		"omnigent:0:conv_" + omnigentBinaryConvHex,
		"omnigent:" + omnigentBinarySubHex,
		"omnigent:conv_" + omnigentBinarySubHex,
		"omnigent:0:conv_" + omnigentBinarySubHex,
	}
	assert.ElementsMatch(t, wantExcluded, outcome.ExcludedSessionIDs)

	wantMigrations := []SessionIdentityMigration{
		{
			PreviousID: "omnigent:" + omnigentBinaryConvHex,
			CurrentID:  "omnigent:0:" + omnigentBinaryConvHex,
		},
		{
			PreviousID: "omnigent:conv_" + omnigentBinaryConvHex,
			CurrentID:  "omnigent:0:" + omnigentBinaryConvHex,
		},
		{
			PreviousID: "omnigent:0:conv_" + omnigentBinaryConvHex,
			CurrentID:  "omnigent:0:" + omnigentBinaryConvHex,
		},
		{
			PreviousID: "omnigent:" + omnigentBinarySubHex,
			CurrentID:  "omnigent:0:" + omnigentBinarySubHex,
		},
		{
			PreviousID: "omnigent:conv_" + omnigentBinarySubHex,
			CurrentID:  "omnigent:0:" + omnigentBinarySubHex,
		},
		{
			PreviousID: "omnigent:0:conv_" + omnigentBinarySubHex,
			CurrentID:  "omnigent:0:" + omnigentBinarySubHex,
		},
	}
	assert.ElementsMatch(t, wantMigrations, outcome.SessionIdentityMigrations)
}

func TestOmnigentMetaMessageIsHiddenButChangesFingerprint(t *testing.T) {
	path := writeOmnigentOldGenDB(t)
	before, err := ParseOmnigentDB(path, "host")
	require.NoError(t, err)
	require.Len(t, before, 2)
	beforeByID := make(map[string]ParseResult, len(before))
	for _, result := range before {
		beforeByID[result.Session.ID] = result
	}
	beforeRoot := beforeByID["omnigent:conv_root"]

	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO conversation_items
		(id, conversation_id, position, type, data, search_text)
		VALUES ('meta_item', 'conv_root', -1, 'message', ?, ?)`,
		readOmnigentFixture(t, "message_meta_user.json"),
		"secret injected skill instructions",
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	after, err := ParseOmnigentDB(path, "host")
	require.NoError(t, err)
	require.Len(t, after, 2)
	afterByID := make(map[string]ParseResult, len(after))
	for _, result := range after {
		afterByID[result.Session.ID] = result
	}
	afterRoot := afterByID["omnigent:conv_root"]

	assert.Equal(t, beforeRoot.Session.FirstMessage, afterRoot.Session.FirstMessage)
	assert.Equal(t, beforeRoot.Session.MessageCount, afterRoot.Session.MessageCount)
	assert.Equal(t, beforeRoot.Session.UserMessageCount,
		afterRoot.Session.UserMessageCount)
	assert.Equal(t, beforeRoot.Messages, afterRoot.Messages)
	assert.NotEqual(t, beforeRoot.Session.File.Hash, afterRoot.Session.File.Hash,
		"hidden durable context must still invalidate the semantic fingerprint")
	for _, message := range afterRoot.Messages {
		assert.NotContains(t, message.Content, "secret injected skill instructions")
	}
}

func TestOmnigentBinaryIDChangedPathSweepAndTombstones(t *testing.T) {
	path := writeOmnigentBinaryIDDB(t)
	conn, err := openOmnigentDB(path)
	require.NoError(t, err)
	schema, err := detectOmnigentSchema(conn)
	require.NoError(t, err)
	require.True(t, schema.binaryIDs)
	require.NoError(t, conn.Close())

	tracker := omnigentTrackerAtCurrentHighWater(t, path, schema)
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = writer.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		time.Now().Unix(), omnigentHexBytes(t, omnigentBinaryConvHex))
	require.NoError(t, err)
	_, err = writer.Exec(`INSERT INTO conversation_items
		(id, conversation_id, response_id, created_at, position, type,
		 status, data, search_text)
		VALUES (?, ?, 'resp_changed', 1700000020, 4, 1, 1,
		 '{"role":"assistant","content":[{"type":"output_text","text":"changed"}]}',
		 'changed')`,
		omnigentHexBytes(t, "00000000000000000000000000000006"),
		omnigentHexBytes(t, omnigentBinaryConvHex))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	changed, err := tracker.changedMembers(
		context.Background(), filepath.Dir(path), ChangedPathRequest{
			Path: path, EventKind: "write",
			StoredSourcePaths: []string{
				VirtualSourcePath(path, "0:"+omnigentBinaryConvHex),
				VirtualSourcePath(path, "0:"+omnigentBinarySubHex),
				VirtualSourcePath(path, "0:"+omnigentBinaryGoneHex),
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1,
		"warm events emit only changed members and defer deletion proof")
	assert.Equal(t, "0:"+omnigentBinaryConvHex, changed[0].MemberID)
}

func TestOmnigentUsageEventsTrackCostPresence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		model    string
		wantCost *float64
	}{
		{
			name:    "aggregate without cost stays nil",
			payload: `{"input_tokens":10,"output_tokens":5}`,
			model:   "fallback",
		},
		{
			name: "by_model without cost stays nil",
			payload: `{"by_model":{"m1":` +
				`{"input_tokens":10,"output_tokens":5}}}`,
			model: "m1",
		},
		{
			name: "explicit zero cost is preserved",
			payload: `{"by_model":{"m1":` +
				`{"input_tokens":10,"output_tokens":5,"total_cost_usd":0}}}`,
			model:    "m1",
			wantCost: new(float64),
		},
		{
			name: "recorded cost is preserved",
			payload: `{"input_tokens":10,"output_tokens":5,` +
				`"total_cost_usd":1.25}`,
			model:    "fallback",
			wantCost: func() *float64 { v := 1.25; return &v }(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := omnigentUsageEvents(
				"omnigent:s1", "fallback", []byte(tc.payload),
			)
			require.Len(t, events, 1)
			assert.Equal(t, tc.model, events[0].Model)
			assert.Equal(t, 10, events[0].InputTokens)
			if tc.wantCost == nil {
				assert.Nil(t, events[0].CostUSD,
					"unknown cost must stay NULL for catalog pricing")
				return
			}
			require.NotNil(t, events[0].CostUSD)
			assert.InDelta(t, *tc.wantCost, *events[0].CostUSD, 0.0001)
		})
	}
}

func TestOmnigentUsageEventsAllocateAggregateCostAcrossModels(t *testing.T) {
	events := omnigentUsageEvents(
		"omnigent:s1",
		"fallback",
		[]byte(`{
			"total_cost_usd": 4,
			"by_model": {
				"large": {"input_tokens": 60, "output_tokens": 15},
				"small": {"input_tokens": 20, "output_tokens": 5}
			}
		}`),
	)

	require.Len(t, events, 2)
	assert.Equal(t, "large", events[0].Model)
	require.NotNil(t, events[0].CostUSD)
	assert.InDelta(t, 3, *events[0].CostUSD, 0.0001)
	assert.Equal(t, "small", events[1].Model)
	require.NotNil(t, events[1].CostUSD)
	assert.InDelta(t, 1, *events[1].CostUSD, 0.0001)
	assert.InDelta(t, 4, *events[0].CostUSD+*events[1].CostUSD, 0.0001,
		"per-model events must retain Omnigent's authoritative aggregate cost")
}

func TestOmnigentUsageEventsAllocateAggregateRemainder(t *testing.T) {
	events := omnigentUsageEvents(
		"omnigent:s1",
		"fallback",
		[]byte(`{
			"total_cost_usd": 3,
			"by_model": {
				"priced": {
					"input_tokens": 10,
					"output_tokens": 5,
					"total_cost_usd": 1
				},
				"unpriced": {"input_tokens": 10, "output_tokens": 5}
			}
		}`),
	)

	require.Len(t, events, 2)
	assert.Equal(t, "priced", events[0].Model)
	require.NotNil(t, events[0].CostUSD)
	assert.InDelta(t, 1, *events[0].CostUSD, 0.0001)
	assert.Equal(t, "unpriced", events[1].Model)
	require.NotNil(t, events[1].CostUSD)
	assert.InDelta(t, 2, *events[1].CostUSD, 0.0001)
}

func TestOmnigentShmEventDoesNotResolveToContainer(t *testing.T) {
	path := writeOmnigentBinaryIDDB(t)
	root := filepath.Dir(path)

	_, ok := omnigentClassifyPath(root, path+"-shm", true)
	assert.False(t, ok,
		"-shm events come from the provider's own read connections and "+
			"must not schedule a sweep")
	match, ok := omnigentClassifyPath(root, path+"-wal", true)
	require.True(t, ok, "-wal events carry real commits")
	assert.Equal(t, path, match.Container)
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

func TestParseOmnigentDB_SourceGenerated(t *testing.T) {
	path := os.Getenv("OMNIGENT_SOURCE_DB")
	if path == "" {
		t.Skip("set OMNIGENT_SOURCE_DB to an Omnigent benchmark-seeded chat.db")
	}

	results, err := ParseOmnigentDB(path, "source-generated")
	require.NoError(t, err)
	require.Len(t, results, 3)

	expected := map[string][]string{
		"bench session 2: investigate the failing migration": {
			"investigate the failing migration (item 0)",
			"investigate the failing migration (item 1)",
			"benchmark the list endpoints against postgres (item 2)",
			"reproduce the elicitation race on reconnect (item 3)",
		},
		"bench session 1: investigate the failing migration": {
			"the runner keeps disconnecting under load (item 0)",
			"the runner keeps disconnecting under load (item 1)",
			"benchmark the list endpoints against postgres (item 2)",
			"why does the policy classifier time out (item 3)",
		},
		"bench session 0: trace the tunnel handshake for this runner id": {
			"the runner keeps disconnecting under load (item 0)",
			"investigate the failing migration (item 1)",
			"the runner keeps disconnecting under load (item 2)",
			"reproduce the elicitation race on reconnect (item 3)",
		},
	}
	idPattern := regexp.MustCompile(
		`^omnigent:0:[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`,
	)
	seenIDs := make(map[string]struct{}, len(results))

	totalUserMessages := 0
	for _, result := range results {
		assert.Regexp(t, idPattern, result.Session.ID)
		_, duplicate := seenIDs[result.Session.ID]
		assert.Falsef(t, duplicate, "duplicate session ID %q", result.Session.ID)
		seenIDs[result.Session.ID] = struct{}{}

		wantMessages, ok := expected[result.Session.SessionName]
		require.Truef(t, ok, "unexpected session title %q",
			result.Session.SessionName)
		delete(expected, result.Session.SessionName)
		require.Len(t, result.Messages, len(wantMessages))
		for ordinal, message := range result.Messages {
			assert.Equal(t, RoleUser, message.Role)
			assert.Equal(t, ordinal, message.Ordinal)
			assert.Equal(t, wantMessages[ordinal], message.Content)
			totalUserMessages++
		}
	}
	assert.Empty(t, expected, "all seeded sessions must be present")
	assert.Len(t, seenIDs, 3, "seeded sessions must have unique UUID4 identities")
	assert.Equal(t, 12, totalUserMessages)
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
