package sync_test

// Integration coverage for parse-diff over the DB-backed
// provider-authoritative agents (Forge, Piebald, Warp). Each provider
// discovers a shared SQLite store as one virtual source per session and
// re-parses it through the provider facade, so these tests prove the
// report-only run actually re-parses and compares those sessions rather than
// bucketing them as skipped/"database-backed agent". Assertions go through the
// exported DiffClass/ParseDiffTotals contract, never rendered strings.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// warpTestDB is a minimal Warp SQLite store for the sync test package,
// mirroring the schema the parser reads (agent_conversations + ai_queries).
type warpTestDB struct {
	path string
	db   *sql.DB
}

func createWarpDB(t *testing.T, dir string) *warpTestDB {
	t.Helper()
	path := filepath.Join(dir, "warp.sqlite")
	d, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "opening warp test db")
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE agent_conversations (
			id INTEGER PRIMARY KEY NOT NULL,
			conversation_id TEXT NOT NULL,
			conversation_data TEXT NOT NULL,
			last_modified_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE UNIQUE INDEX ux_agent_conversations_conversation_id
			ON agent_conversations (conversation_id);
		CREATE TABLE ai_queries (
			id INTEGER PRIMARY KEY NOT NULL,
			exchange_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			start_ts DATETIME NOT NULL,
			input TEXT NOT NULL,
			working_directory TEXT,
			output_status TEXT NOT NULL,
			model_id TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX ux_ai_queries_exchange_id
			ON ai_queries(exchange_id);
	`)
	require.NoError(t, err, "creating warp schema")
	return &warpTestDB{path: path, db: d}
}

// addConversation seeds one Warp conversation with one user-query exchange per
// prompt. conversation_data stays "{}" so no aggregate tool messages are
// synthesized and the message count equals the number of prompts.
func (w *warpTestDB) addConversation(
	t *testing.T, convID, lastModified string, prompts ...string,
) {
	t.Helper()
	_, err := w.db.Exec(
		`INSERT INTO agent_conversations
			(conversation_id, conversation_data, last_modified_at)
		 VALUES (?, '{}', ?)`,
		convID, lastModified,
	)
	require.NoError(t, err, "insert warp conversation")
	for i, p := range prompts {
		input := fmt.Sprintf(`[{"Query":{"text":%q,"context":[]}}]`, p)
		_, err := w.db.Exec(
			`INSERT INTO ai_queries
				(exchange_id, conversation_id, start_ts, input,
				 working_directory, output_status, model_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("%s-ex-%d", convID, i), convID,
			fmt.Sprintf("2026-04-07 09:5%d:00.000000", i), input,
			"/Users/alice/code/myproject", `"Completed"`, "auto-genius",
		)
		require.NoError(t, err, "insert warp exchange")
	}
}

// TestParseDiffCoversForge proves Forge's shared .forge.db, discovered as one
// virtual source per conversation, is re-parsed and compared by parse-diff.
// Examined:1/Identical:1 means the stored conversation was matched and vetted,
// not bucketed as skipped/"database-backed agent".
func TestParseDiffCoversForge(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t, "forge-clean-1", "Forge Clean",
		forgeTestContext("Add Forge coverage.", "Added Forge coverage."),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentForge},
	})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Identical: 1},
		report.Totals, "forge conversation must be examined, not skipped")
	assert.Equal(t, 1, report.FilesExamined, ".forge.db examined")
	assert.False(t, report.HasFailures(), "clean forge run")
}

// TestParseDiffCoversPiebald proves Piebald's shared app.db is re-parsed and
// compared per chat.
func TestParseDiffCoversPiebald(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentPiebald)
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChat(t, 7, "Piebald Clean",
		"Add Piebald coverage.", "Added Piebald coverage.",
		"2026-05-01T10:05:00Z")
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentPiebald},
	})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Identical: 1},
		report.Totals, "piebald chat must be examined, not skipped")
	assert.Equal(t, 1, report.FilesExamined, "app.db examined")
	assert.False(t, report.HasFailures(), "clean piebald run")
}

// TestParseDiffCoversWarp proves Warp's shared warp.sqlite is re-parsed and
// compared per conversation.
func TestParseDiffCoversWarp(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentWarp)
	warp := createWarpDB(t, env.warpDir)
	warp.addConversation(t, "warp-clean-1", "2026-04-07 10:00:00",
		"Fix the JSON parsing bug.")
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentWarp},
	})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Identical: 1},
		report.Totals, "warp conversation must be examined, not skipped")
	assert.Equal(t, 1, report.FilesExamined, "warp.sqlite examined")
	assert.False(t, report.HasFailures(), "clean warp run")
}

// TestParseDiffForgeDetectsStoredDrift mutates a stored Forge row after a sync
// and verifies the re-parse from the unchanged .forge.db surfaces the drift as
// DiffChanged. This proves the DB-backed re-parse+compare path is real, not a
// no-op that always reports identical, and that the raced guard does not mask
// the change (the virtual DB source is held out of the guard, so it keeps its
// real changed verdict).
func TestParseDiffForgeDetectsStoredDrift(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t, "forge-drift-1", "Forge Drift",
		forgeTestContext("Original prompt.", "Original answer."),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1})

	// Stored-row drift against the unchanged source.
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "forge:forge-drift-1")

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentForge},
	})
	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "forge drift must be detected as changed")

	sd := findSessionDiff(report, "forge:forge-drift-1")
	require.NotNil(t, sd, "drifted forge session must be listed")
	assert.Equal(t, sync.DiffChanged, sd.Class, "class")
	assert.ElementsMatch(t, []string{sync.FieldFirstMessage},
		sessionDiffFieldNames(sd, false), "non-informational fields")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"FieldCounts[first_message]")
	assert.True(t, report.HasFailures(), "drift must trip --fail-on-change")
}

// TestParseDiffDBBackedLimitScopesPerSession is the per-session-keying acid
// test. A shared .forge.db holds two conversations; --limit 1 samples one and
// cuts the other. Because these providers discover one virtual source per
// session, the cut conversation must be reported as an ordinary
// not-sampled skip -- NOT fanned into a presence "changed" false positive
// (which a shared-DB base key would produce) and NOT bucketed as
// "database-backed agent" (which the pre-relaxation sweep would produce for a
// FileBased=false agent). Totals.Changed must stay 0.
func TestParseDiffDBBackedLimitScopesPerSession(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t, "forge-a", "Conversation A",
		forgeTestContext("Prompt A.", "Answer A."),
		"2026-05-02 09:00:00", "2026-05-02 09:01:00",
		`{"input_tokens":100,"output_tokens":20}`,
	)
	forge.addConversation(
		t, "forge-b", "Conversation B",
		forgeTestContext("Prompt B.", "Answer B."),
		"2026-05-02 09:00:00", "2026-05-02 09:02:00",
		`{"input_tokens":120,"output_tokens":25}`,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentForge},
		Limit:  1,
	})

	assert.True(t, report.FilesLimited, "files limited")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1, Skipped: 1,
	}, report.Totals, "one conversation sampled, one cut")
	// The cut conversation must never be counted as parser drift.
	assert.Zero(t, report.Totals.Changed,
		"cut sibling conversation must not become a presence change")
	assert.Empty(t, report.FieldCounts,
		"no field drift from an unsampled sibling")

	// Exactly one session is listed: the cut one, as a not-sampled skip.
	var skipped []sync.SessionDiff
	for _, s := range report.Sessions {
		if s.Class == sync.DiffSkipped {
			skipped = append(skipped, s)
		}
	}
	require.Len(t, skipped, 1, "exactly one skipped session listed")
	assert.Contains(t, skipped[0].Reason, "limit",
		"cut DB-backed session must read as not-sampled, "+
			"not 'database-backed agent'")
}

// TestParseDiffDBBackedLimitOrdersByPerSessionMtime is the ordering acid test.
// A shared .forge.db holds two conversations whose virtual-path order (by
// conversation ID) is the opposite of their updated_at order: forge-a sorts
// lexicographically first but is older, forge-b sorts later but is newer. Under
// --limit 1 the newest session must be the one sampled, so forge-a is cut.
// Before the ordering fix both virtual sources stat to mtime 0, --limit
// tie-breaks on the lexicographically-earlier path, forge-a is sampled instead,
// and the newer forge-b is dropped -- so this asserted skip would be
// "forge:forge-b" and the test would fail.
func TestParseDiffDBBackedLimitOrdersByPerSessionMtime(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	// forge-a: sorts first by path, older updated_at.
	forge.addConversation(
		t, "forge-a", "Older A",
		forgeTestContext("Prompt A.", "Answer A."),
		"2026-05-01 09:00:00", "2026-05-01 09:00:00",
		`{"input_tokens":100,"output_tokens":20}`,
	)
	// forge-b: sorts later by path, newer updated_at.
	forge.addConversation(
		t, "forge-b", "Newer B",
		forgeTestContext("Prompt B.", "Answer B."),
		"2026-06-01 09:00:00", "2026-06-01 09:00:00",
		`{"input_tokens":120,"output_tokens":25}`,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentForge},
		Limit:  1,
	})

	assert.True(t, report.FilesLimited, "files limited")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1, Skipped: 1,
	}, report.Totals, "the newer conversation is sampled, the older cut")

	// A clean identical sample is not listed, so the sole listed session is the
	// cut (older) one -- and it must be forge-a, proving the newer forge-b won
	// the --limit slot by mtime rather than by lexicographic path.
	var skipped []sync.SessionDiff
	for _, s := range report.Sessions {
		if s.Class == sync.DiffSkipped {
			skipped = append(skipped, s)
		}
	}
	require.Len(t, skipped, 1, "exactly one skipped session listed")
	assert.Equal(t, "forge:forge-a", skipped[0].SessionID,
		"the older conversation must be the one cut by --limit")
	assert.Contains(t, skipped[0].Reason, "limit",
		"cut session reads as not-sampled")
}
