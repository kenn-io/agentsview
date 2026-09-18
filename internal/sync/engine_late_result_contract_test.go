package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestIncrementalResultSanitizationRespectsCategory(t *testing.T) {
	for _, tt := range []struct {
		name          string
		blocked, link bool
		want          string
		wantLength    int
	}{
		{name: "result_link", link: true, want: "xyz", wantLength: 3},
		{name: "blocked_late_event", blocked: true, want: "", wantLength: 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := openTestDB(t)
			cfg := EngineConfig{DisableSignalRecomputation: true}
			if tt.blocked {
				cfg.BlockedResultCategories = []string{"Bash"}
			}
			engine := NewEngine(database, cfg)
			t.Cleanup(engine.Close)
			require.NoError(database.UpsertSession(db.Session{ID: "s1", Project: "project-a", Agent: "codex", MessageCount: 1}))
			require.NoError(database.InsertMessages([]db.Message{{
				SessionID: "s1", Ordinal: 0, Role: "assistant", HasToolUse: true,
				ToolCalls: []db.ToolCall{{SessionID: "s1", ToolUseID: "call", ToolName: "exec_command", Category: "Bash"}},
			}}))
			inc := &incrementalUpdate{sessionID: "s1", agent: parser.AgentCodex, msgCount: 1, nextOrdinal: 1}
			if tt.link {
				inc.links = []parser.ClaudeSubagentLink{{ToolUseID: "call", HasResult: true, ResultContentRaw: `"x\u0000y\u0001z"`, ResultContentLen: 5}}
			} else {
				inc.toolCallUpdates = []parser.ParsedToolCallUpdate{{
					ToolUseID: "call", TargetKnown: true,
					ResultEvents: []parser.ParsedToolResultEvent{{ToolUseID: "call", Source: "function_call_output", Content: "x\x00y\x01z"}},
				}}
			}
			require.NoError(engine.writeIncremental(inc))
			messages, err := database.GetAllMessages(t.Context(), "s1")
			require.NoError(err)
			require.Len(messages, 1)
			require.Len(messages[0].ToolCalls, 1)
			assert.Equal(tt.want, messages[0].ToolCalls[0].ResultContent)
			assert.Equal(tt.wantLength, messages[0].ToolCalls[0].ResultContentLength)
		})
	}
}

func TestCodexAppendedResultRetainsRawIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{DisableSignalRecomputation: true})
	t.Cleanup(engine.Close)
	require.NoError(database.UpsertSession(db.Session{ID: "s1", Project: "project-a", Agent: "codex"}))
	require.NoError(engine.writeIncremental(&incrementalUpdate{
		sessionID: "s1", agent: parser.AgentCodex, msgCount: 1, nextOrdinal: 1,
		msgs: []parser.ParsedMessage{{
			Ordinal: 0, Role: parser.RoleAssistant, HasToolUse: true,
			ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
				ResultEvents: []parser.ParsedToolResultEvent{{ToolUseID: "call", Source: "function_call_output", Content: "x\x01"}},
			}},
		}},
	}))
	missing, err := database.HasMissingToolResultMetadata(t.Context(), "s1", []db.ToolCallPosition{{}})
	require.NoError(err)
	assert.False(missing, "newly appended Codex events must remain eligible for incremental updates")
	require.NoError(engine.writeIncremental(&incrementalUpdate{
		sessionID: "s1", agent: parser.AgentCodex, msgCount: 1, nextOrdinal: 1,
		toolCallUpdates: []parser.ParsedToolCallUpdate{{
			ToolUseID: "call", TargetKnown: true,
			ResultEvents: []parser.ParsedToolResultEvent{{ToolUseID: "call", Source: "function_call_output", Content: "x\x02"}},
		}},
	}))
	messages, err := database.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(messages, 1)
	require.Len(messages[0].ToolCalls, 1)
	assert.Len(messages[0].ToolCalls[0].ResultEvents, 2)
	assert.Equal("x", messages[0].ToolCalls[0].ResultContent)
}
