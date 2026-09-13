package rawtest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ParityFixture contains source-authored expectations, independent of both
// hosted preparation and the migration comparator.
type ParityFixture struct {
	Root     string
	Sessions []ParitySession
}
type ParitySession struct {
	ID, Title, Parent, First string
	Roles                    []string
	Output                   int
	HasOutput                bool
	ToolName, ToolResult     string
	Usage                    []ParityUsage
	MessageUsage             map[int]ParityMessageUsage
}

// Nil cost differs from an explicitly reported zero. Message usage presence is
// asserted separately from zero-valued counters.
type ParityUsage struct {
	Input, Output int
	Cost          *int64
}
type ParityMessageUsage struct {
	Context, Output int
	Present         bool
}

func ParityFixtures() map[parser.AgentType]func(*testing.T, string) ParityFixture {
	return map[parser.AgentType]func(*testing.T, string) ParityFixture{
		parser.AgentClaude: func(t *testing.T, root string) ParityFixture {
			Claude(t, root)
			ClaudeChild(t, root)
			return ParityFixture{root, []ParitySession{
				{ID: ClaudeID, MessageUsage: map[int]ParityMessageUsage{0: {}, 1: {Context: 116, Output: 7, Present: true}, 2: {Present: true}}, Roles: []string{"user", "assistant", "assistant"}, Output: 7, HasOutput: true, ToolName: "Bash", ToolResult: "Error: synthetic build failure"},
				{ID: ClaudeChildID, Parent: ClaudeID, First: "Inspect the child task.", Roles: []string{"user", "assistant"}},
			}}
		},
		parser.AgentCodex: func(t *testing.T, root string) ParityFixture {
			CodexTools(t, root)
			CodexFork(t, root)
			CodexParent(t, root)
			appendParityCodexParent(t, root)
			return ParityFixture{root, []ParitySession{
				{ID: "codex:" + CodexToolsID, First: "Run the build.", Roles: []string{"user", "assistant", "assistant"}, Output: 9, HasOutput: true, ToolName: "exec_command", ToolResult: "Error: synthetic failure"},
				{ID: "codex:" + CodexParentID, First: "Inspect the parent.", Roles: []string{"user", "assistant"}},
				{ID: "codex:" + CodexChildID, First: "Inspect the fork.", Roles: []string{"user", "assistant"}},
			}}
		},
		parser.AgentZCode: func(t *testing.T, root string) ParityFixture {
			ZCode(t, root)
			return ParityFixture{root, []ParitySession{
				{ID: ZCodeID, Title: "Captured database session", First: "Captured database session", Roles: []string{"user", "assistant"}, Output: 7, HasOutput: true},
				{ID: ZCodeBillableID, Title: "Billable without transcript", Output: 17, HasOutput: true},
			}}
		},
		parser.AgentEvener: EvenerParity, parser.AgentGoose: GooseParity, parser.AgentForge: ForgeParity, parser.AgentPiebald: PiebaldParity, parser.AgentWarp: WarpParity,
	}
}

func (f ParityFixture) IDs() []string {
	ids := make([]string, 0, len(f.Sessions))
	for _, s := range f.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}
func (f ParityFixture) AssertOracle(t *testing.T, archive *db.DB) {
	t.Helper()
	require.NotEmpty(t, f.Sessions)
	rows, err := archive.Reader().QueryContext(t.Context(), `SELECT id FROM sessions ORDER BY id`)
	require.NoError(t, err)
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, f.IDs(), ids, "archive session inventory differs from source-authored fixture")
	for _, want := range f.Sessions {
		session, err := archive.GetSessionFull(t.Context(), want.ID)
		require.NoError(t, err)
		require.NotNil(t, session, want.ID)
		if want.Title != "" {
			require.NotNil(t, session.DisplayName)
			assert.Equal(t, want.Title, *session.DisplayName)
		}
		if want.Parent != "" {
			require.NotNil(t, session.ParentSessionID)
			assert.Equal(t, want.Parent, *session.ParentSessionID)
		}
		if want.First != "" {
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, want.First, *session.FirstMessage)
		}
		assert.Equal(t, want.Output, session.TotalOutputTokens, want.ID)
		assert.Equal(t, want.HasOutput, session.HasTotalOutputTokens, want.ID)
		messages, err := archive.GetAllMessages(t.Context(), want.ID)
		require.NoError(t, err)
		roles := make([]string, 0, len(messages))
		var calls []db.ToolCall
		for _, m := range messages {
			roles = append(roles, m.Role)
			calls = append(calls, m.ToolCalls...)
		}
		assert.Equal(t, append([]string{}, want.Roles...), roles, want.ID)

		for ordinal, usage := range want.MessageUsage {
			require.Less(t, ordinal, len(messages))
			message := messages[ordinal]
			assert.Equal(t, usage.Present, message.HasOutputTokens)
			assert.Equal(t, usage.Present, message.HasContextTokens)
			assert.Equal(t, usage.Context, message.ContextTokens)
			assert.Equal(t, usage.Output, message.OutputTokens)
			assert.Equal(t, !usage.Present, len(message.TokenUsage) == 0)
		}
		if want.Usage != nil {
			events, err := archive.GetUsageEvents(t.Context(), want.ID)
			require.NoError(t, err)
			require.Len(t, events, len(want.Usage))
			for i, usage := range want.Usage {
				assert.Equal(t, usage.Input, events[i].InputTokens)
				assert.Equal(t, usage.Output, events[i].OutputTokens)
				if usage.Cost == nil {
					assert.Nil(t, events[i].Cost)
				} else {
					require.NotNil(t, events[i].Cost)
					assert.Equal(t, *usage.Cost, events[i].Cost.Microdollars)
				}
			}
		}
		if want.ToolName != "" {
			require.NotEmpty(t, calls)
			assert.Equal(t, want.ToolName, calls[0].ToolName)
			assert.Contains(t, calls[0].ResultContent, want.ToolResult)
		}
	}
}

func paritySQLite(t *testing.T, root, name, schema string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(root, name))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec("PRAGMA journal_mode=WAL;" + schema)
	require.NoError(t, err)
	return database
}
func paritySQL(t *testing.T, database *sql.DB, statement string, args ...any) {
	t.Helper()
	_, err := database.Exec(statement, args...)
	require.NoError(t, err)
}

func appendParityCodexParent(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "2026", "07", "06", "rollout-2026-07-06T11-00-00-"+CodexParentID+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = file.WriteString(`{"type":"response_item","timestamp":"2026-07-06T11:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the parent."}]}}
{"type":"response_item","timestamp":"2026-07-06T11:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Parent inspected."}]}}
`)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
