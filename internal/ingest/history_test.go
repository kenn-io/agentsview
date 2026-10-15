package ingest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
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
		t.Context(), candidate, prior, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryPreserve, result.Action)
	assert.True(t, result.PriorContributed)
	require.Len(t, result.Candidate.Messages, 1)
	assert.Equal(t, "keep this", result.Candidate.Messages[0].Content)
}

func TestReconcileProviderHistoryHermesGroupKey(t *testing.T) {
	tests := []struct {
		name          string
		keepStored    bool
		noPrior       bool
		fresh, stored string
		want          string
	}{
		{name: "pruned ancestry", keepStored: true, stored: "job-a", want: "job-a"},
		{name: "no preservation intent", stored: "job-a"},
		{name: "resolved job", fresh: "job-b", stored: "job-a", want: "job-b"},
		{name: "first seen after prune", keepStored: true, noPrior: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := ingest.Candidate{
				Session: db.Session{ID: "hermes:middle", Agent: "hermes", Project: "ops", GroupKey: tt.fresh},
				Parsed: parser.ParseResult{Session: parser.ParsedSession{
					Agent: parser.AgentHermes, KeepStoredGroupKey: tt.keepStored,
				}},
			}
			prior := &ingest.PriorSession{Session: db.Session{GroupKey: tt.stored}}
			if tt.noPrior {
				prior = nil
			}
			result, err := ingest.ReconcileProviderHistory(t.Context(), candidate, prior, ingest.ContentOptions{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, result.Candidate.Session.GroupKey)
			assert.Equal(t, "ops", result.Candidate.Session.Project)
			assert.Equal(t, ingest.HistoryReplace, result.Action)
			assert.Equal(t, tt.keepStored && !tt.noPrior, result.PriorContributed)
		})
	}
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
			t.Context(), candidate, prior, ingest.ContentOptions{},
		)
		require.NoError(t, err)
		assert.Equal(t, ingest.HistoryPreserve, result.Action)
		assert.True(t, result.PriorContributed)
		assert.Equal(t, "complete answer", result.Candidate.Messages[0].Content)
	})

	t.Run("changed fingerprint with complete rewrite replaces", func(t *testing.T) {
		candidate := openCodeHistoryCandidate(currentFingerprint, "a longer rewritten answer")
		result, err := ingest.ReconcileProviderHistory(
			t.Context(), candidate, prior, ingest.ContentOptions{},
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
		t.Context(), candidate, prior, ingest.ContentOptions{
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
		t.Context(), candidate, prior, ingest.ContentOptions{},
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

func TestReconcileProviderHistoryClaudeRestoresSingleOccurrence(t *testing.T) {
	ts := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	tsStr := timeutil.Format(ts)
	otherTsStr := timeutil.Format(time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC))
	placeholder := "Full output saved to: /x/tool-results/a.txt"
	// Captured under a "keep" policy before this session's archive was
	// written under the live "drop" policy some cases below use.
	withImage := `[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]`

	tests := []struct {
		name                 string
		priorTimestamp       string
		priorContent         string
		priorContentLength   int
		options              ingest.ContentOptions
		wantAction           ingest.HistoryAction
		wantPriorContributed bool
		check                func(t *testing.T, call db.ToolCall)
	}{
		{
			name: "plain restore", priorTimestamp: tsStr,
			priorContent: "full archived output", priorContentLength: len("full archived output"),
			wantAction: ingest.HistoryMerge, wantPriorContributed: true,
			check: func(t *testing.T, call db.ToolCall) {
				t.Helper()
				require.Len(t, call.ResultEvents, 1)
				assert.Equal(t, "full archived output", call.ResultEvents[0].Content)
				assert.Equal(t, len("full archived output"), call.ResultEvents[0].ContentLength)
				assert.Equal(t, "full archived output", call.ResultContent)
				assert.Equal(t, len("full archived output"), call.ResultContentLength)
			},
		},
		{
			name: "length only for blocked category", priorTimestamp: tsStr,
			priorContent: "", priorContentLength: 500,
			options: ingest.ContentOptions{
				BlockedResultCategories: map[string]bool{"bash": true},
				ArchiveContent:          config.ArchiveContentUsage,
			},
			wantAction: ingest.HistoryMerge, wantPriorContributed: true,
			check: func(t *testing.T, call db.ToolCall) {
				t.Helper()
				assert.Empty(t, call.ResultEvents[0].Content)
				assert.Equal(t, 500, call.ResultEvents[0].ContentLength)
				assert.Empty(t, call.ResultContent)
				assert.Equal(t, 500, call.ResultContentLength)
			},
		},
		{
			name: "no unique match leaves it alone", priorTimestamp: otherTsStr,
			priorContent:       "archived under a different timestamp",
			priorContentLength: len("archived under a different timestamp"),
			wantAction:         ingest.HistoryReplace,
			check: func(t *testing.T, call db.ToolCall) {
				t.Helper()
				assert.Equal(t, placeholder, call.ResultEvents[0].Content)
			},
		},
		{
			name: "image policy reapplied after restore", priorTimestamp: tsStr,
			priorContent: withImage, priorContentLength: len(withImage),
			options:    ingest.ContentOptions{ToolResultImages: config.ToolResultImagesDrop},
			wantAction: ingest.HistoryMerge, wantPriorContributed: true,
			check: func(t *testing.T, call db.ToolCall) {
				t.Helper()
				assert.NotContains(t, call.ResultEvents[0].Content, "input_image")
				assert.NotContains(t, call.ResultEvents[0].Content, "aGVsbG8")
				assert.NotContains(t, call.ResultContent, "input_image")
				assert.NotContains(t, call.ResultContent, "aGVsbG8")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prior := &ingest.PriorSession{
				Messages: []db.Message{{
					Ordinal: 0, Role: "user", Timestamp: tc.priorTimestamp,
					ToolCalls: []db.ToolCall{{
						ToolUseID: "toolu_1", Category: "bash",
						ResultEvents: []db.ToolResultEvent{{
							ToolUseID: "toolu_1", Source: "tool_result", Timestamp: tc.priorTimestamp,
							Content: tc.priorContent, ContentLength: tc.priorContentLength,
						}},
					}},
				}},
			}
			candidate := ingest.Candidate{
				Session: db.Session{ID: "claude:session-1", Agent: string(parser.AgentClaude)},
				Messages: []db.Message{{
					Ordinal: 0, Role: "user", Timestamp: tsStr,
					ToolCalls: []db.ToolCall{{
						ToolUseID: "toolu_1", Category: "bash",
						ResultEvents: []db.ToolResultEvent{{
							ToolUseID: "toolu_1", Source: "tool_result", Timestamp: tsStr,
							Content: placeholder, ContentLength: len(placeholder),
						}},
					}},
				}},
				Parsed: parser.ParseResult{
					Session: parser.ParsedSession{
						ID: "session-1", Agent: parser.AgentClaude,
						File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
					},
					Messages: []parser.ParsedMessage{{
						Ordinal: 0, Timestamp: ts,
						ToolResults: []parser.ParsedToolResult{{
							ToolUseID: "toolu_1", Source: "tool_result",
							PersistedOutputUnresolved: true,
						}},
					}},
				},
			}

			result, err := ingest.ReconcileProviderHistory(
				t.Context(), candidate, prior, tc.options,
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAction, result.Action)
			assert.Equal(t, tc.wantPriorContributed, result.PriorContributed)
			tc.check(t, result.Candidate.Messages[0].ToolCalls[0])
		})
	}
}

// TestReconcileProviderHistoryClaudeRestoresArchivedDigestForLengthOnly
// covers a length-only restore under a blocked category: the event's
// RawContentDigest must come from the archive, not stay the current parse's
// placeholder digest, so a later reparse that again restores the same
// archived length sees no change and never falsely advances a
// digest-derived revision.
func TestReconcileProviderHistoryClaudeRestoresArchivedDigestForLengthOnly(t *testing.T) {
	ts := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	tsStr := timeutil.Format(ts)
	placeholder := "Full output saved to: /x/tool-results/a.txt"
	archivedDigest := []byte("archived-content-digest")

	prior := &ingest.PriorSession{
		Messages: []db.Message{{
			Ordinal: 0, Role: "user", Timestamp: tsStr,
			ToolCalls: []db.ToolCall{{
				ToolUseID: "toolu_1", Category: "bash",
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID: "toolu_1", Source: "tool_result", Timestamp: tsStr,
					Content: "", ContentLength: 500, RawContentDigest: archivedDigest,
				}},
			}},
		}},
	}
	parsedMessages := []parser.ParsedMessage{{
		Ordinal: 0, Timestamp: ts,
		ToolResults: []parser.ParsedToolResult{{
			ToolUseID: "toolu_1", Source: "tool_result",
			PersistedOutputUnresolved: true,
		}},
	}}
	options := ingest.ContentOptions{
		BlockedResultCategories: map[string]bool{"bash": true},
		ArchiveContent:          config.ArchiveContentUsage,
	}
	candidate := ingest.Candidate{
		Session: db.Session{ID: "claude:session-1", Agent: string(parser.AgentClaude)},
		Messages: []db.Message{{
			Ordinal: 0, Role: "user", Timestamp: tsStr,
			ToolCalls: []db.ToolCall{{
				ToolUseID: "toolu_1", Category: "bash",
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID: "toolu_1", Source: "tool_result", Timestamp: tsStr,
					Content: placeholder, ContentLength: len(placeholder),
					RawContentDigest: []byte("placeholder-content-digest"),
				}},
			}},
		}},
		Parsed: parser.ParseResult{
			Session: parser.ParsedSession{
				ID: "session-1", Agent: parser.AgentClaude,
				File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
			},
			Messages: parsedMessages,
		},
	}

	result, err := ingest.ReconcileProviderHistory(
		t.Context(), candidate, prior, options,
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryMerge, result.Action)
	event := result.Candidate.Messages[0].ToolCalls[0].ResultEvents[0]
	assert.Equal(t, archivedDigest, event.RawContentDigest)

	// A later reparse that again sees only the placeholder, and again
	// restores from the same unchanged archive, must find nothing left to
	// change: the restored event already matches the archive in content,
	// length, and digest.
	second := result.Candidate
	second.Parsed.Messages = parsedMessages
	secondResult, err := ingest.ReconcileProviderHistory(
		t.Context(), second, prior, options,
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryReplace, secondResult.Action)
	assert.False(t, secondResult.PriorContributed)
}

func TestReconcileProviderHistoryClaudeRestoresOnlyFlaggedOccurrence(t *testing.T) {
	t1 := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 11, 9, 5, 0, 0, time.UTC)
	t1Str, t2Str := timeutil.Format(t1), timeutil.Format(t2)
	placeholder := "Full output saved to: /x/tool-results/a.txt"

	prior := &ingest.PriorSession{
		Messages: []db.Message{{
			Ordinal: 0, Role: "user", Timestamp: t1Str,
			ToolCalls: []db.ToolCall{{
				ToolUseID: "toolu_1", Category: "bash",
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID: "toolu_1", Source: "tool_result", Timestamp: t1Str,
					Content: "archived first occurrence", ContentLength: len("archived first occurrence"),
				}},
			}},
		}},
	}
	candidate := ingest.Candidate{
		Session: db.Session{ID: "claude:session-1", Agent: string(parser.AgentClaude)},
		Messages: []db.Message{
			{
				Ordinal: 0, Role: "user", Timestamp: t1Str,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "toolu_1", Category: "bash",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "toolu_1", Source: "tool_result", Timestamp: t1Str,
						Content: placeholder, ContentLength: len(placeholder),
					}},
				}},
			},
			{
				Ordinal: 1, Role: "user", Timestamp: t2Str,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "toolu_1", Category: "bash",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "toolu_1", Source: "tool_result", Timestamp: t2Str,
						Content: "corrected retry output", ContentLength: len("corrected retry output"),
					}},
				}},
			},
		},
		Parsed: parser.ParseResult{
			Session: parser.ParsedSession{
				ID: "session-1", Agent: parser.AgentClaude,
				File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
			},
			Messages: []parser.ParsedMessage{
				{
					Ordinal: 0, Timestamp: t1,
					ToolResults: []parser.ParsedToolResult{{
						ToolUseID: "toolu_1", Source: "tool_result",
						PersistedOutputUnresolved: true,
					}},
				},
				{
					Ordinal: 1, Timestamp: t2,
					ToolResults: []parser.ParsedToolResult{{
						ToolUseID: "toolu_1", Source: "tool_result",
						PersistedOutputUnresolved: false,
					}},
				},
			},
		},
	}

	result, err := ingest.ReconcileProviderHistory(
		t.Context(), candidate, prior, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, ingest.HistoryMerge, result.Action)
	require.Len(t, result.Candidate.Messages, 2)
	assert.Equal(t, "archived first occurrence",
		result.Candidate.Messages[0].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "corrected retry output",
		result.Candidate.Messages[1].ToolCalls[0].ResultEvents[0].Content)
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
