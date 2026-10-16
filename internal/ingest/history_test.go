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
			// Accepted limit: when the archive held only a length
			// (ArchiveContentUsage/ArchiveContentTranscripts, or a blocked
			// category), restoration leaves the reparsed event alone rather
			// than reproducing "blank content, kept length" — that shape
			// already means something else (a deduped summary) to every
			// reader, so there's no way to restore just a length without
			// colliding with it. The placeholder stands and its length is
			// not recovered.
			name: "length only for blocked category: nothing to restore", priorTimestamp: tsStr,
			priorContent: "", priorContentLength: 500,
			options: ingest.ContentOptions{
				BlockedResultCategories: map[string]bool{"bash": true},
				ArchiveContent:          config.ArchiveContentUsage,
			},
			wantAction: ingest.HistoryReplace,
			check: func(t *testing.T, call db.ToolCall) {
				t.Helper()
				assert.Equal(t, placeholder, call.ResultEvents[0].Content)
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

func TestReconcileProviderHistoryClaudeRestoresOnlyFlaggedOccurrence(t *testing.T) {
	t1 := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 11, 9, 5, 0, 0, time.UTC)
	t1Str, t2Str := timeutil.Format(t1), timeutil.Format(t2)
	placeholder := "Full output saved to: /x/tool-results/a.txt"

	prior := &ingest.PriorSession{
		Messages: []db.Message{
			{
				Ordinal: 0, Role: "user", Timestamp: t1Str,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "toolu_1", Category: "bash",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "toolu_1", Source: "tool_result", Timestamp: t1Str,
						Content: "archived first occurrence", ContentLength: len("archived first occurrence"),
					}},
				}},
			},
			{
				// A real archived counterpart at t2 with stale content. The
				// candidate's own t2 occurrence is unflagged (already
				// resolved with a correction), so this must never surface:
				// restoring it would silently undo a correction the current
				// parse already made.
				Ordinal: 1, Role: "user", Timestamp: t2Str,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "toolu_1", Category: "bash",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "toolu_1", Source: "tool_result", Timestamp: t2Str,
						Content:       "stale archived second occurrence",
						ContentLength: len("stale archived second occurrence"),
					}},
				}},
			},
		},
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

func TestReconcileProviderHistoryClaudeRestoreGuards(t *testing.T) {
	ts := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	tsStr := timeutil.Format(ts)
	placeholder := "Full output saved to: /x/tool-results/a.txt"
	event := func(id, content string, length int) db.ToolResultEvent {
		return db.ToolResultEvent{
			ToolUseID: id, Source: "tool_result", Timestamp: tsStr,
			Content: content, ContentLength: length,
		}
	}
	flaggedResult := parser.ParsedToolResult{
		ToolUseID: "toolu_1", Source: "tool_result",
		PersistedOutputUnresolved: true,
	}
	prior := &ingest.PriorSession{Messages: []db.Message{{
		Ordinal: 0, Role: "user", Timestamp: tsStr,
		ToolCalls: []db.ToolCall{{
			ToolUseID: "toolu_1", Category: "bash",
			ResultEvents: []db.ToolResultEvent{
				event("toolu_1", "full archived output", len("full archived output")),
			},
		}},
	}}}

	tests := []struct {
		name    string
		calls   []db.ToolCall
		options ingest.ContentOptions
		check   func(t *testing.T, calls []db.ToolCall)
	}{
		{
			name: "two current events sharing one identity are not restored",
			calls: []db.ToolCall{
				{ToolUseID: "toolu_1", Category: "bash", ResultEvents: []db.ToolResultEvent{
					event("toolu_1", placeholder, len(placeholder)),
				}},
				{ToolUseID: "toolu_1", Category: "bash", ResultEvents: []db.ToolResultEvent{
					event("toolu_1", placeholder, len(placeholder)),
				}},
			},
			check: func(t *testing.T, calls []db.ToolCall) {
				t.Helper()
				for _, call := range calls {
					assert.Equal(t, placeholder, call.ResultEvents[0].Content)
				}
			},
		},
		{
			name: "restore leaves an unrelated blocked call's length alone",
			calls: []db.ToolCall{
				{ToolUseID: "toolu_1", Category: "bash", ResultEvents: []db.ToolResultEvent{
					event("toolu_1", placeholder, len(placeholder)),
				}},
				{
					ToolUseID: "toolu_2", Category: "Read",
					ResultContentLength: 300,
					ResultEvents: []db.ToolResultEvent{
						event("toolu_2", "", 300),
					},
				},
			},
			options: ingest.ContentOptions{
				BlockedResultCategories: map[string]bool{"Read": true},
			},
			check: func(t *testing.T, calls []db.ToolCall) {
				t.Helper()
				assert.Equal(t, "full archived output", calls[0].ResultEvents[0].Content)
				assert.Equal(t, 300, calls[1].ResultContentLength)
				assert.Empty(t, calls[1].ResultContent)
				assert.Empty(t, calls[1].ResultEvents[0].Content)
			},
		},
		{
			name: "restore under the drop-images policy recomputes the returned call",
			calls: []db.ToolCall{
				{ToolUseID: "toolu_1", Category: "bash", ResultEvents: []db.ToolResultEvent{
					event("toolu_1", placeholder, len(placeholder)),
				}},
			},
			options: ingest.ContentOptions{
				ToolResultImages: config.ToolResultImagesDrop,
			},
			check: func(t *testing.T, calls []db.ToolCall) {
				t.Helper()
				assert.Equal(t, "full archived output", calls[0].ResultEvents[0].Content)
				assert.Equal(t, "full archived output", calls[0].ResultContent)
				assert.Equal(t, len("full archived output"), calls[0].ResultContentLength)
			},
		},
		{
			name: "restore on a blocked call keeps its pre-reconcile length",
			calls: []db.ToolCall{
				{
					ToolUseID: "toolu_1", Category: "Read",
					ResultContentLength: 777,
					ResultEvents: []db.ToolResultEvent{
						event("toolu_1", placeholder, len(placeholder)),
						{
							ToolUseID: "toolu_1", Source: "tool_result",
							Timestamp: timeutil.Format(ts.Add(time.Second)),
							Content:   "", ContentLength: 777,
						},
					},
				},
			},
			options: ingest.ContentOptions{
				BlockedResultCategories: map[string]bool{"Read": true},
			},
			check: func(t *testing.T, calls []db.ToolCall) {
				t.Helper()
				assert.Equal(t, 777, calls[0].ResultContentLength)
				assert.Empty(t, calls[0].ResultContent)
				for _, ev := range calls[0].ResultEvents {
					assert.Empty(t, ev.Content)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ingest.Candidate{
				Session: db.Session{ID: "claude:session-1", Agent: string(parser.AgentClaude)},
				Messages: []db.Message{{
					Ordinal: 0, Role: "user", Timestamp: tsStr, ToolCalls: tc.calls,
				}},
				Parsed: parser.ParseResult{
					Session: parser.ParsedSession{
						ID: "session-1", Agent: parser.AgentClaude,
						File: parser.FileInfo{Path: "/synthetic/project/session-1.jsonl"},
					},
					Messages: []parser.ParsedMessage{{
						Ordinal: 0, Timestamp: ts,
						ToolResults: []parser.ParsedToolResult{flaggedResult},
					}},
				},
			}
			result, err := ingest.ReconcileProviderHistory(
				t.Context(), candidate, prior, tc.options,
			)
			require.NoError(t, err)
			tc.check(t, result.Candidate.Messages[0].ToolCalls)
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
