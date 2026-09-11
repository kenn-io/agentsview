package ingest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReconcileProviderHistoryPreservesEmptyRooArchive(t *testing.T) {
	prior := &ingest.PriorSession{
		Session: db.Session{ID: "roocode:task-1", Agent: string(parser.AgentRooCode)},
		Messages: []db.Message{{
			SessionID: "roocode:task-1", Ordinal: 0, Role: "user",
			Content: "keep this", ContentLength: len("keep this"),
		}},
	}
	candidate := ingest.Candidate{
		Session: db.Session{ID: "roocode:task-1", Agent: string(parser.AgentRooCode)},
		Parsed: parser.ParseResult{Session: parser.ParsedSession{
			ID: "task-1", Agent: parser.AgentRooCode,
		}},
	}

	result, err := ingest.ReconcileProviderHistory(
		context.Background(), candidate, prior, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryPreserve, result.Action)
	assert.True(t, result.PriorContributed)
	require.Len(t, result.Candidate.Messages, 1)
	assert.Equal(t, "keep this", result.Candidate.Messages[0].Content)
}

func TestReconcileProviderHistoryOpenCodeFingerprintAndContent(t *testing.T) {
	const storedFingerprint = `opencode-storage:v1:{"messages":[{"id":"message-1","time":1,"hash":"old"}]}`
	const currentFingerprint = `opencode-storage:v1:{"messages":[{"id":"message-1","time":1,"hash":"new"}]}`
	storedPath := "/synthetic/storage/session/session-1.json"
	prior := &ingest.PriorSession{
		Session: db.Session{
			ID: "opencode:session-1", Agent: string(parser.AgentOpenCode),
			FilePath: &storedPath, FileHash: new(storedFingerprint),
		},
		Messages: []db.Message{{
			Ordinal: 0, Role: "assistant", SourceUUID: "message-1",
			Content: "complete answer", ContentLength: len("complete answer"),
		}},
	}

	t.Run("missing fingerprint and weaker content preserves", func(t *testing.T) {
		candidate := openCodeHistoryCandidate("", "short")
		result, err := ingest.ReconcileProviderHistory(
			context.Background(), candidate, prior, ingest.ContentOptions{},
		)
		require.NoError(t, err)
		assert.Equal(t, ingest.HistoryPreserve, result.Action)
		assert.True(t, result.PriorContributed)
		assert.Equal(t, "complete answer", result.Candidate.Messages[0].Content)
	})

	t.Run("changed fingerprint with complete rewrite replaces", func(t *testing.T) {
		candidate := openCodeHistoryCandidate(currentFingerprint, "a longer rewritten answer")
		result, err := ingest.ReconcileProviderHistory(
			context.Background(), candidate, prior, ingest.ContentOptions{},
		)
		require.NoError(t, err)
		assert.Equal(t, ingest.HistoryReplace, result.Action)
		assert.False(t, result.PriorContributed)
		assert.Equal(t, "a longer rewritten answer", result.Candidate.Messages[0].Content)
	})
}

func TestReconcileProviderHistoryOpenCodeUsageOnlyComparesUsage(t *testing.T) {
	storedPath := "/synthetic/storage/session/session-1.json"
	prior := &ingest.PriorSession{
		Session: db.Session{
			ID: "opencode:session-1", Agent: string(parser.AgentOpenCode),
			FilePath: &storedPath,
		},
		Messages: []db.Message{{
			Ordinal: 0, Role: "assistant", SourceUUID: "message-1",
			Model:      "model-a",
			TokenUsage: []byte(`{"input_tokens":300,"output_tokens":200}`),
		}},
	}
	candidate := openCodeHistoryCandidate("", "content is projected away")
	candidate.Messages[0].Model = "model-a"
	candidate.Messages[0].TokenUsage = []byte(
		`{"input_tokens":300,"output_tokens":199}`,
	)

	result, err := ingest.ReconcileProviderHistory(
		context.Background(), candidate, prior, ingest.ContentOptions{
			ArchiveContent: config.ArchiveContentUsage,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryPreserve, result.Action)
	assert.True(t, result.PriorContributed)
	assert.JSONEq(t, `{"input_tokens":300,"output_tokens":200}`,
		string(result.Candidate.Messages[0].TokenUsage))
}

func TestReconcileProviderHistoryMergesVisualStudioCopilotArchive(t *testing.T) {
	archivedFirst := "Archived prompt"
	archivedName := "Archived title"
	archivedStart := "2026-09-11T09:00:00Z"
	archivedEnd := "2026-09-11T09:30:00Z"
	prior := &ingest.PriorSession{
		Session: db.Session{
			ID: "copilot:conversation-1", Agent: string(parser.AgentVSCopilot),
			FirstMessage: &archivedFirst, SessionName: &archivedName,
			StartedAt: &archivedStart, EndedAt: &archivedEnd,
		},
		Messages: []db.Message{
			{
				SessionID: "copilot:conversation-1", Ordinal: 0, Role: "user",
				Content: archivedFirst, ContentLength: len(archivedFirst),
				Timestamp: archivedStart,
			},
			{
				SessionID: "copilot:conversation-1", Ordinal: 1, Role: "assistant",
				Content: "running", ContentLength: len("running"),
				Timestamp: "2026-09-11T09:10:00Z",
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call-1", ToolName: "run_command_in_terminal",
				}},
			},
		},
	}
	candidate := ingest.Candidate{
		Session: db.Session{
			ID: "copilot:conversation-1", Agent: string(parser.AgentVSCopilot),
			StartedAt: new("2026-09-11T09:10:00Z"),
			EndedAt:   new("2026-09-11T09:20:00Z"),
		},
		Messages: []db.Message{
			{
				SessionID: "copilot:conversation-1", Ordinal: 0, Role: "assistant",
				Content: "running", ContentLength: len("running"),
				Timestamp: "2026-09-11T09:10:00Z", OutputTokens: 10,
				HasOutputTokens: true,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call-1", ToolName: "run_command_in_terminal",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "call-1", Status: "completed",
						Content: "passed", ContentLength: len("passed"),
					}},
				}},
			},
			{
				SessionID: "copilot:conversation-1", Ordinal: 1, Role: "assistant",
				Content: "new answer", ContentLength: len("new answer"),
				Timestamp: "2026-09-11T09:20:00Z", OutputTokens: 5,
				HasOutputTokens: true,
			},
		},
		Parsed: parser.ParseResult{Session: parser.ParsedSession{
			ID: "conversation-1", Agent: parser.AgentVSCopilot,
		}},
	}

	result, err := ingest.ReconcileProviderHistory(
		context.Background(), candidate, prior, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryMerge, result.Action)
	assert.True(t, result.PriorContributed)
	require.Len(t, result.Candidate.Messages, 3)
	assert.Equal(t, archivedFirst, result.Candidate.Messages[0].Content)
	require.Len(t, result.Candidate.Messages[1].ToolCalls, 1)
	require.Len(t, result.Candidate.Messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "passed",
		result.Candidate.Messages[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "new answer", result.Candidate.Messages[2].Content)
	assert.Equal(t, &archivedFirst, result.Candidate.Session.FirstMessage)
	assert.Equal(t, &archivedName, result.Candidate.Session.SessionName)
	assert.Equal(t, &archivedStart, result.Candidate.Session.StartedAt)
	assert.Equal(t, &archivedEnd, result.Candidate.Session.EndedAt)
	assert.Equal(t, 3, result.Candidate.Session.MessageCount)
	assert.Equal(t, 1, result.Candidate.Session.UserMessageCount)
	assert.Equal(t, 15, result.Candidate.Session.TotalOutputTokens)
	assert.True(t, result.Candidate.Session.HasTotalOutputTokens)
}

func TestProviderHistorySourceClassifiers(t *testing.T) {
	tests := []struct {
		name          string
		agent         parser.AgentType
		path          string
		claude        bool
		storage       bool
		sqliteVirtual bool
	}{
		{
			name: "claude transcript", agent: parser.AgentClaude,
			path: "/synthetic/project/session-1.jsonl", claude: true,
		},
		{
			name: "icodemate cli transcript", agent: parser.AgentIcodemate,
			path: "/synthetic/project/session-1.jsonl", claude: true,
		},
		{
			name: "icodemate storage json", agent: parser.AgentIcodemate,
			path: "/synthetic/storage/session/session-1.json", storage: true,
		},
		{
			name: "opencode sqlite member", agent: parser.AgentOpenCode,
			path: "/synthetic/opencode.db#session-1", sqliteVirtual: true,
		},
		{
			name: "kilo sqlite member", agent: parser.AgentKilo,
			path: "/synthetic/kilo.db#session-1", sqliteVirtual: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.claude,
				ingest.IsClaudeFormatTranscript(test.agent, test.path))
			assert.Equal(t, test.storage,
				ingest.IsOpenCodeFormatStoragePath(test.agent, test.path))
			assert.Equal(t, test.sqliteVirtual,
				ingest.IsOpenCodeFormatSQLiteVirtualPath(test.agent, test.path))
		})
	}
}

func openCodeHistoryCandidate(hash, content string) ingest.Candidate {
	return ingest.Candidate{
		Session: db.Session{
			ID: "opencode:session-1", Agent: string(parser.AgentOpenCode),
		},
		Messages: []db.Message{{
			Ordinal: 0, Role: "assistant", SourceUUID: "message-1",
			Content: content, ContentLength: len(content),
		}},
		Parsed: parser.ParseResult{Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentOpenCode,
			File: parser.FileInfo{
				Path: "/synthetic/storage/session/session-1.json", Hash: hash,
			},
		}},
	}
}
