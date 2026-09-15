package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestStagedBatchHonorsArchiveContentPolicy(t *testing.T) {
	for _, policy := range []config.ArchiveContent{
		config.ArchiveContentTranscripts, config.ArchiveContentUsage,
	} {
		t.Run(string(policy), func(t *testing.T) {
			database := testDB(t)
			database.SetArchiveContent(policy)
			const sessionID = "codex:projected-staged"
			const prompt = "You are a code reviewer. Review the code changes shown below."
			staged := newScratchStagedResults(t)
			staged.AddEvent(t, "call_1", "tool payload in scratch")
			write := SessionBatchWrite{
				Session: Session{ID: sessionID, Project: "project", Machine: "local", Agent: "codex",
					MessageCount: 2, UserMessageCount: 1, FirstMessage: new(prompt)},
				Messages: []Message{
					{SessionID: sessionID, Ordinal: 0, Role: "user", Content: prompt},
					{SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "review complete",
						Model: "model-a", ReasoningEffort: "high", OutputTokens: 7, HasOutputTokens: true,
						HasToolUse: true, ToolCalls: []ToolCall{{
							ToolUseID: "call_1", ToolName: "exec_command", Category: "Bash",
							InputJSON: `{"command":"cat file"}`, ResultContentLength: 23,
							ResultEvents: []ToolResultEvent{{Source: "function_call_output", Status: "completed",
								Content: "staged placeholder", ContentLength: 23}},
						}}},
				},
				Staged: staged, ReplaceMessages: true,
				StagedSignals: func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
					return SessionSignalUpdate{Outcome: "success"}, nil, nil
				},
				Checkpoint:      &ParserCheckpoint{SessionID: sessionID, Version: ParserCheckpointVersion},
				CheckpointBlobs: &ParserCheckpointBlobs{SessionID: sessionID, Cursor: []byte("cursor")},
			}
			var result SessionBatchResult
			var err error
			if policy == config.ArchiveContentTranscripts {
				result, err = database.WriteSessionAtomic(write)
			} else {
				result, err = database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
			}
			require.NoError(t, err)
			require.Empty(t, result.Errors)
			require.Equal(t, 1, result.WrittenSessions)
			messages, err := database.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			session, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.True(t, session.IsAutomated, "classify the original prompt before omitting text")
			_, hasCheckpoint, err := database.GetParserCheckpoint(sessionID)
			require.NoError(t, err)
			assert.False(t, hasCheckpoint, "omitted tool history cannot retain an incremental cursor")
			_, hasBlobs, err := database.GetParserCheckpointBlobs(sessionID)
			require.NoError(t, err)
			assert.False(t, hasBlobs)

			if policy == config.ArchiveContentTranscripts {
				require.Len(t, messages, 2)
				assert.Equal(t, prompt, messages[0].Content)
				assert.Equal(t, "review complete", messages[1].Content)
				require.Len(t, messages[1].ToolCalls, 1)
				call := messages[1].ToolCalls[0]
				assert.Equal(t, "exec_command", call.ToolName)
				assert.Empty(t, call.InputJSON)
				assert.Empty(t, call.ResultContent)
				require.Len(t, call.ResultEvents, 1)
				assert.Empty(t, call.ResultEvents[0].Content)
				assert.Equal(t, "completed", call.ResultEvents[0].Status)
				assert.Equal(t, 23, call.ResultEvents[0].ContentLength)
				assert.Equal(t, "success", session.Outcome)
			} else {
				require.Len(t, messages, 1)
				assert.Empty(t, messages[0].Content)
				assert.Empty(t, messages[0].ToolCalls)
				assert.Nil(t, session.FirstMessage)
				assert.Empty(t, session.Outcome)
				assert.Equal(t, CurrentQualitySignalVersion, session.QualitySignalVersion)
			}
			assistant := messages[len(messages)-1]
			assert.Equal(t, "model-a", assistant.Model)
			assert.Equal(t, "high", assistant.ReasoningEffort)
			assert.Equal(t, 7, assistant.OutputTokens)
		})
	}
}
