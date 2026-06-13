package sync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// pdBaseSession returns a stored/prepared session pair that compares
// as identical, for tests to mutate.
func pdBaseSession() db.Session {
	return db.Session{
		ID:                   "pd-sess",
		Agent:                "claude",
		MessageCount:         4,
		UserMessageCount:     2,
		FirstMessage:         new("hello there"),
		SessionName:          new("My Session"),
		TotalOutputTokens:    120,
		PeakContextTokens:    9000,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
		TerminationStatus:    new("clean"),
	}
}

func TestCompareSessionFields(t *testing.T) {
	type want struct {
		field         string
		stored        string
		parsed        string
		informational bool
	}
	tests := []struct {
		name     string
		stored   func(*db.Session)
		prepared func(*db.Session)
		want     []want
	}{
		{
			name: "identical sessions produce no diffs",
		},
		{
			name:   "message count drift",
			stored: func(s *db.Session) { s.MessageCount = 3 },
			want: []want{{
				field: FieldMessageCount, stored: "3", parsed: "4",
			}},
		},
		{
			name:   "user message count drift",
			stored: func(s *db.Session) { s.UserMessageCount = 1 },
			want: []want{{
				field: FieldUserMessageCount, stored: "1", parsed: "2",
			}},
		},
		{
			name: "first message nil stored vs empty parsed is no diff",
			stored: func(s *db.Session) {
				s.FirstMessage = nil
			},
			prepared: func(s *db.Session) {
				s.FirstMessage = new("")
			},
		},
		{
			name: "first message empty stored vs nil parsed is no diff",
			stored: func(s *db.Session) {
				s.FirstMessage = new("")
			},
			prepared: func(s *db.Session) {
				s.FirstMessage = nil
			},
		},
		{
			name: "first message drift",
			prepared: func(s *db.Session) {
				s.FirstMessage = new("different opener")
			},
			want: []want{{
				field:  FieldFirstMessage,
				stored: "hello there",
				parsed: "different opener",
			}},
		},
		{
			name: "first message null vs value renders (null)",
			stored: func(s *db.Session) {
				s.FirstMessage = nil
			},
			want: []want{{
				field:  FieldFirstMessage,
				stored: "(null)",
				parsed: "hello there",
			}},
		},
		{
			name: "session name nil vs empty is no diff",
			stored: func(s *db.Session) {
				s.SessionName = new("")
			},
			prepared: func(s *db.Session) {
				s.SessionName = nil
			},
		},
		{
			name: "session name drift is informational for incremental agents",
			prepared: func(s *db.Session) {
				s.SessionName = new("Renamed")
			},
			want: []want{{
				field:         FieldSessionName,
				stored:        "My Session",
				parsed:        "Renamed",
				informational: true,
			}},
		},
		{
			name: "session name drift is a real diff for full-replace agents",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.SessionName = new("Renamed")
			},
			want: []want{{
				field:  FieldSessionName,
				stored: "My Session",
				parsed: "Renamed",
			}},
		},
		{
			name: "total output tokens value drift",
			prepared: func(s *db.Session) {
				s.TotalOutputTokens = 121
			},
			want: []want{{
				field:  FieldTotalOutputTokens,
				stored: "120",
				parsed: "121",
			}},
		},
		{
			name: "total output coverage-flag-only flip is a real diff",
			prepared: func(s *db.Session) {
				s.HasTotalOutputTokens = false
			},
			want: []want{{
				field:  FieldTotalOutputTokens,
				stored: "120",
				parsed: "absent",
			}},
		},
		{
			name: "absent-on-both-sides values are not compared",
			stored: func(s *db.Session) {
				s.HasTotalOutputTokens = false
				s.TotalOutputTokens = 0
			},
			prepared: func(s *db.Session) {
				s.HasTotalOutputTokens = false
				s.TotalOutputTokens = 7
			},
		},
		{
			name: "peak context tokens drift",
			stored: func(s *db.Session) {
				s.PeakContextTokens = 8000
			},
			want: []want{{
				field:  FieldPeakContextTokens,
				stored: "8000",
				parsed: "9000",
			}},
		},
		{
			name: "termination stored null parsed value is informational",
			stored: func(s *db.Session) {
				s.TerminationStatus = nil
			},
			want: []want{{
				field:         FieldTerminationStatus,
				stored:        "(null)",
				parsed:        "clean",
				informational: true,
			}},
		},
		{
			name: "termination stored value parsed null is a real diff",
			prepared: func(s *db.Session) {
				s.TerminationStatus = nil
			},
			want: []want{{
				field:  FieldTerminationStatus,
				stored: "clean",
				parsed: "(null)",
			}},
		},
		{
			name: "termination value drift is a real diff",
			prepared: func(s *db.Session) {
				s.TerminationStatus = new("awaiting_user")
			},
			want: []want{{
				field:  FieldTerminationStatus,
				stored: "clean",
				parsed: "awaiting_user",
			}},
		},
		{
			name: "termination null on both sides is no diff",
			stored: func(s *db.Session) {
				s.TerminationStatus = nil
			},
			prepared: func(s *db.Session) {
				s.TerminationStatus = nil
			},
		},
		{
			name: "termination stored null parsed value is real drift for full-replace agents",
			stored: func(s *db.Session) {
				s.Agent = "antigravity-cli"
				s.TerminationStatus = nil
			},
			prepared: func(s *db.Session) {
				s.Agent = "antigravity-cli"
			},
			want: []want{{
				field:         FieldTerminationStatus,
				stored:        "(null)",
				parsed:        "clean",
				informational: false,
			}},
		},
		{
			name: "started_at drift is a real diff",
			stored: func(s *db.Session) {
				s.StartedAt = new("2026-01-01T00:00:00Z")
			},
			prepared: func(s *db.Session) {
				s.StartedAt = new("2026-01-02T00:00:00Z")
			},
			want: []want{{
				field:  FieldStartedAt,
				stored: "2026-01-01T00:00:00Z",
				parsed: "2026-01-02T00:00:00Z",
			}},
		},
		{
			name: "ended_at drift is a real diff",
			stored: func(s *db.Session) {
				s.EndedAt = new("2026-01-01T01:00:00Z")
			},
			prepared: func(s *db.Session) {
				s.EndedAt = new("2026-01-01T02:00:00Z")
			},
			want: []want{{
				field:  FieldEndedAt,
				stored: "2026-01-01T01:00:00Z",
				parsed: "2026-01-01T02:00:00Z",
			}},
		},
		{
			name: "cwd drift is informational for incremental agents",
			stored: func(s *db.Session) {
				s.Cwd = "/home/me/old"
			},
			prepared: func(s *db.Session) {
				s.Cwd = "/home/me/new"
			},
			want: []want{{
				field:         FieldCwd,
				stored:        "/home/me/old",
				parsed:        "/home/me/new",
				informational: true,
			}},
		},
		{
			name: "cwd drift is a real diff for full-replace agents",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.Cwd = "/home/me/old"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.Cwd = "/home/me/new"
			},
			want: []want{{
				field:  FieldCwd,
				stored: "/home/me/old",
				parsed: "/home/me/new",
			}},
		},
		{
			name: "git_branch drift is a real diff for full-replace agents",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.GitBranch = "main"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.GitBranch = "feature"
			},
			want: []want{{
				field:  FieldGitBranch,
				stored: "main",
				parsed: "feature",
			}},
		},
		{
			name: "relationship_type drift is a real diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.RelationshipType = "continuation"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.RelationshipType = "fork"
			},
			want: []want{{
				field:  FieldRelationshipType,
				stored: "continuation",
				parsed: "fork",
			}},
		},
		{
			name: "source_session_id drift is a real diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.SourceSessionID = "abc"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.SourceSessionID = "xyz"
			},
			want: []want{{
				field:  FieldSourceSessionID,
				stored: "abc",
				parsed: "xyz",
			}},
		},
		{
			name: "source_version drift is a real diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.SourceVersion = "1.0.0"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.SourceVersion = "1.1.0"
			},
			want: []want{{
				field:  FieldSourceVersion,
				stored: "1.0.0",
				parsed: "1.1.0",
			}},
		},
		{
			name: "parent_session_id nil stored vs empty parsed is no diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParentSessionID = nil
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParentSessionID = new("")
			},
		},
		{
			name: "parent_session_id drift renders (null) for nil stored",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParentSessionID = nil
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParentSessionID = new("parent-1")
			},
			want: []want{{
				field:  FieldParentSessionID,
				stored: "(null)",
				parsed: "parent-1",
			}},
		},
		{
			name: "parser_malformed_lines drift is a real diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParserMalformedLines = 0
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.ParserMalformedLines = 3
			},
			want: []want{{
				field:  FieldParserMalformedLines,
				stored: "0",
				parsed: "3",
			}},
		},
		{
			name: "parser_malformed_lines drift is informational for incremental agents",
			prepared: func(s *db.Session) {
				s.ParserMalformedLines = 2
			},
			want: []want{{
				field:         FieldParserMalformedLines,
				stored:        "0",
				parsed:        "2",
				informational: true,
			}},
		},
		{
			name: "is_truncated drift is a real diff",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.IsTruncated = false
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.IsTruncated = true
			},
			want: []want{{
				field:  FieldIsTruncated,
				stored: "false",
				parsed: "true",
			}},
		},
		{
			name: "project is not compared (resolver-derived)",
			stored: func(s *db.Session) {
				s.Agent = "gemini"
				s.Project = "project-old"
			},
			prepared: func(s *db.Session) {
				s.Agent = "gemini"
				s.Project = "project-new"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := pdBaseSession()
			prepared := pdBaseSession()
			if tt.stored != nil {
				tt.stored(&stored)
			}
			if tt.prepared != nil {
				tt.prepared(&prepared)
			}
			diffs := compareSessionFields(&stored, prepared)
			require.Len(t, diffs, len(tt.want))
			for i, w := range tt.want {
				assert.Equal(t, w.field, diffs[i].Field)
				assert.Equal(t, w.stored, diffs[i].Stored)
				assert.Equal(t, w.parsed, diffs[i].Parsed)
				assert.Equal(
					t, w.informational, diffs[i].Informational,
				)
			}
		})
	}
}

func TestCompareSessionFieldsTruncatesLongValues(t *testing.T) {
	long := strings.Repeat("x", 200)
	stored := pdBaseSession()
	prepared := pdBaseSession()
	prepared.FirstMessage = new(long)

	diffs := compareSessionFields(&stored, prepared)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldFirstMessage, diffs[0].Field)
	assert.Equal(
		t, strings.Repeat("x", maxRenderedValueRunes)+"...",
		diffs[0].Parsed,
	)
	assert.Contains(t, diffs[0].Detail, "parsed 200 runes")
}

func TestCompareSessionFieldsInformationalTerminationDetail(t *testing.T) {
	stored := pdBaseSession()
	stored.TerminationStatus = nil
	prepared := pdBaseSession()

	diffs := compareSessionFields(&stored, prepared)
	require.Len(t, diffs, 1)
	assert.True(t, diffs[0].Informational)
	assert.Equal(t, "incremental-append history", diffs[0].Detail)
}

func pdMsg(ordinal int, model string, ctx, out int) db.Message {
	return db.Message{
		Ordinal:          ordinal,
		Role:             "assistant",
		Model:            model,
		ContextTokens:    ctx,
		OutputTokens:     out,
		HasContextTokens: ctx != 0,
		HasOutputTokens:  out != 0,
	}
}

func TestCompareMessageMetadata(t *testing.T) {
	t.Run("identical slices produce no diffs", func(t *testing.T) {
		stored := []db.Message{
			pdMsg(0, "", 0, 0), pdMsg(1, "model-a", 100, 5),
		}
		parsed := []db.Message{
			pdMsg(0, "", 0, 0), pdMsg(1, "model-a", 100, 5),
		}
		assert.Empty(t, compareMessageMetadata(stored, parsed, false, false, false))
	})

	t.Run("model drift reports count and first ordinal", func(t *testing.T) {
		stored := []db.Message{
			pdMsg(0, "model-a", 0, 0),
			pdMsg(1, "model-a", 0, 0),
			pdMsg(2, "model-a", 0, 0),
		}
		parsed := []db.Message{
			pdMsg(0, "model-a", 0, 0),
			pdMsg(1, "model-b", 0, 0),
			pdMsg(2, "model-b", 0, 0),
		}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldModels, diffs[0].Field)
		assert.Equal(t, "model-a", diffs[0].Stored)
		assert.Equal(t, "model-a, model-b", diffs[0].Parsed)
		assert.Equal(
			t,
			"2/3 messages differ; first at ordinal 1: model-a -> model-b",
			diffs[0].Detail,
		)
	})

	t.Run("token value drift reports message tokens", func(t *testing.T) {
		stored := []db.Message{pdMsg(0, "m", 100, 5)}
		parsed := []db.Message{pdMsg(0, "m", 110, 5)}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageTokens, diffs[0].Field)
		assert.Equal(
			t, "context=100 output=5 usage_bytes=0", diffs[0].Stored,
		)
		assert.Equal(
			t, "context=110 output=5 usage_bytes=0", diffs[0].Parsed,
		)
		assert.Equal(
			t, "1/1 messages differ; first at ordinal 0",
			diffs[0].Detail,
		)
	})

	t.Run("presence-flag-only flip is a token diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant",
			OutputTokens: 0, HasOutputTokens: true,
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant",
			OutputTokens: 0, HasOutputTokens: false,
		}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageTokens, diffs[0].Field)
		assert.Contains(t, diffs[0].Stored, "output=0")
		assert.Contains(t, diffs[0].Parsed, "output=absent")
	})

	t.Run("token_usage payload drift is a token diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant",
			TokenUsage: json.RawMessage(`{"input_tokens":2}`),
		}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageTokens, diffs[0].Field)
	})

	t.Run("equal-length body rewrite is a content diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant",
			Content: "aaa", ContentLength: 3,
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant",
			Content: "bbb", ContentLength: 3,
		}}
		diffs := compareMessageMetadata(stored, parsed, false, true, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageContent, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail,
			"body differs at equal length (3 bytes)")
	})

	t.Run("length mismatch compares the overlap only", func(t *testing.T) {
		stored := []db.Message{pdMsg(0, "m", 100, 5)}
		parsed := []db.Message{
			pdMsg(0, "m", 100, 5),
			pdMsg(1, "different-model", 999, 9),
		}
		assert.Empty(t, compareMessageMetadata(stored, parsed, false, false, false))
	})

	t.Run("alignment matches ordinal values not indices", func(t *testing.T) {
		stored := []db.Message{
			pdMsg(0, "m", 100, 5), pdMsg(2, "m", 200, 6),
		}
		parsed := []db.Message{
			pdMsg(2, "m", 200, 6), pdMsg(0, "m", 100, 5),
		}
		assert.Empty(t, compareMessageMetadata(stored, parsed, false, false, false))
	})

	t.Run("is_system flip is a metadata diff", func(t *testing.T) {
		stored := []db.Message{{Ordinal: 0, Role: "user", IsSystem: false}}
		parsed := []db.Message{{Ordinal: 0, Role: "user", IsSystem: true}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "is_system false -> true")
	})

	t.Run("has_thinking flip is a metadata diff", func(t *testing.T) {
		stored := []db.Message{{Ordinal: 0, Role: "assistant"}}
		parsed := []db.Message{{Ordinal: 0, Role: "assistant", HasThinking: true}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "has_thinking false -> true")
	})

	t.Run("has_tool_use flip is a metadata diff", func(t *testing.T) {
		stored := []db.Message{{Ordinal: 0, Role: "assistant"}}
		parsed := []db.Message{{Ordinal: 0, Role: "assistant", HasToolUse: true}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "has_tool_use false -> true")
	})

	t.Run("thinking_text drift is a metadata diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant", ThinkingText: "old reasoning",
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant", ThinkingText: "new reasoning",
		}}
		diffs := compareMessageMetadata(stored, parsed, true, false, false)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "thinking_text differs")
	})

	t.Run("tool_name drift is a tool_calls diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant", HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{ToolName: "Read", Category: "Read", ToolUseID: "tu1"},
			},
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant", HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{ToolName: "Bash", Category: "Read", ToolUseID: "tu1"},
			},
		}}
		diffs := compareMessageMetadata(stored, parsed, false, false, true)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldToolCalls, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, `tool_name "Read" -> "Bash"`)
		assert.Contains(t, diffs[0].Detail, "first at ordinal 0")
	})

	t.Run("tool_call sub-field drift attributes to tool_calls",
		func(t *testing.T) {
			base := db.ToolCall{
				ToolName: "Read", Category: "Read", ToolUseID: "tu1",
				InputJSON: `{"file":"a"}`, SkillName: "s1",
				SubagentSessionID: "agent-1",
			}
			subCases := []struct {
				name   string
				mutate func(*db.ToolCall)
				detail string
			}{
				{"category", func(tc *db.ToolCall) { tc.Category = "Bash" },
					`category "Read" -> "Bash"`},
				{"tool_use_id", func(tc *db.ToolCall) { tc.ToolUseID = "tu2" },
					"tool_use_id differs"},
				{"input_json", func(tc *db.ToolCall) { tc.InputJSON = `{"file":"b"}` },
					"input_json differs"},
				{"skill_name", func(tc *db.ToolCall) { tc.SkillName = "s2" },
					`skill_name "s1" -> "s2"`},
				{"subagent_session_id",
					func(tc *db.ToolCall) { tc.SubagentSessionID = "agent-2" },
					"subagent_session_id differs"},
			}
			for _, sc := range subCases {
				t.Run(sc.name, func(t *testing.T) {
					parsedTC := base
					sc.mutate(&parsedTC)
					stored := []db.Message{{
						Ordinal: 0, Role: "assistant",
						ToolCalls: []db.ToolCall{base},
					}}
					parsed := []db.Message{{
						Ordinal: 0, Role: "assistant",
						ToolCalls: []db.ToolCall{parsedTC},
					}}
					diffs := compareMessageMetadata(
						stored, parsed, false, false, true,
					)
					require.Len(t, diffs, 1)
					assert.Equal(t, FieldToolCalls, diffs[0].Field)
					assert.Contains(t, diffs[0].Detail, sc.detail)
				})
			}
		})

	t.Run("tool_call count drift is a tool_calls diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant",
			ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "tu1"}},
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant",
			ToolCalls: []db.ToolCall{
				{ToolName: "Read", ToolUseID: "tu1"},
				{ToolName: "Bash", ToolUseID: "tu2"},
			},
		}}
		diffs := compareMessageMetadata(stored, parsed, false, false, true)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldToolCalls, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "tool_call count 1 -> 2")
	})

	t.Run("result_content_length drift is a tool_calls diff", func(t *testing.T) {
		stored := []db.Message{{
			Ordinal: 0, Role: "assistant",
			ToolCalls: []db.ToolCall{
				{ToolName: "Read", ToolUseID: "tu1", ResultContentLength: 100},
			},
		}}
		parsed := []db.Message{{
			Ordinal: 0, Role: "assistant",
			ToolCalls: []db.ToolCall{
				{ToolName: "Read", ToolUseID: "tu1", ResultContentLength: 250},
			},
		}}
		diffs := compareMessageMetadata(stored, parsed, false, false, true)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldToolCalls, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "result_content_length 100 -> 250")
	})

	t.Run("tool fingerprint differs but overlap matches yields fallback",
		func(t *testing.T) {
			// No aligned-ordinal tool diff (e.g. an ordinal-set shift),
			// but the tool fingerprint asserted inequality: the session
			// must not report identical.
			stored := []db.Message{{Ordinal: 0, Role: "assistant"}}
			parsed := []db.Message{{Ordinal: 0, Role: "assistant"}}
			diffs := compareMessageMetadata(stored, parsed, false, false, true)
			require.Len(t, diffs, 1)
			assert.Equal(t, FieldToolCalls, diffs[0].Field)
			assert.Equal(t, "fingerprint", diffs[0].Stored)
			assert.Contains(t, diffs[0].Detail, "tool-call fingerprint differs")
		})
}

func pdEvent(
	source, model string, in, out int, occurredAt, dedup string,
	ordinal *int,
) db.UsageEvent {
	return db.UsageEvent{
		Source:         source,
		Model:          model,
		InputTokens:    in,
		OutputTokens:   out,
		OccurredAt:     occurredAt,
		DedupKey:       dedup,
		MessageOrdinal: ordinal,
	}
}

func TestCompareUsageEvents(t *testing.T) {
	t.Run("both empty produces no diffs", func(t *testing.T) {
		assert.Empty(t, compareUsageEvents(nil, nil))
	})

	t.Run("shuffled order produces no diffs", func(t *testing.T) {
		a := pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", new(0))
		b := pdEvent("api", "m2", 20, 4, "2026-01-01T00:01:00Z", "", new(1))
		c := pdEvent("api", "m1", 10, 2, "2026-01-01T00:02:00Z", "k1", nil)
		stored := []db.UsageEvent{a, b, c}
		parsed := []db.UsageEvent{c, a, b}
		assert.Empty(t, compareUsageEvents(stored, parsed))
	})

	t.Run("removed event reports count and totals", func(t *testing.T) {
		a := pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", nil)
		b := pdEvent("api", "m2", 20, 4, "2026-01-01T00:01:00Z", "", nil)
		diffs := compareUsageEvents(
			[]db.UsageEvent{a, b}, []db.UsageEvent{a},
		)
		require.Len(t, diffs, 2)
		assert.Equal(t, FieldUsageEventCount, diffs[0].Field)
		assert.Equal(t, "2", diffs[0].Stored)
		assert.Equal(t, "1", diffs[0].Parsed)
		assert.Equal(t, FieldUsageEventTotals, diffs[1].Field)
		assert.Equal(t, "input 30 -> 10; output 6 -> 2", diffs[1].Detail)
	})

	t.Run("total drift with equal count reports totals only", func(t *testing.T) {
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 10, 3, "2026-01-01T00:00:00Z", "", nil),
		}
		diffs := compareUsageEvents(stored, parsed)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Equal(t, "output 2 -> 3", diffs[0].Detail)
		assert.Contains(t, diffs[0].Stored, "output=2")
		assert.Contains(t, diffs[0].Parsed, "output=3")
	})

	t.Run("matching dedup keys override tuple drift", func(t *testing.T) {
		// Same dedup key with a different occurred_at: the key is
		// authoritative, totals match, so no diff.
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T09:09:09Z", "k1", nil),
		}
		assert.Empty(t, compareUsageEvents(stored, parsed))
	})

	t.Run("dedup key drift with equal totals reports composition", func(t *testing.T) {
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k2", nil),
		}
		diffs := compareUsageEvents(stored, parsed)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "event composition differs")
		assert.Contains(t, diffs[0].Detail, "dedup|k1")
	})

	t.Run("model drift under stable dedup key surfaces", func(t *testing.T) {
		// Same dedup key and equal token totals, but the event is
		// re-attributed to a different model: must not pass silently.
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m2", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		diffs := compareUsageEvents(stored, parsed)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "event composition differs")
	})

	t.Run("dedup keyed token redistribution surfaces", func(t *testing.T) {
		// Equal event count, equal aggregate totals, and stable
		// dedup keys, but the per-event token payload changed.
		// --fail-on-change must not pass over that data drift.
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
			pdEvent("api", "m1", 20, 4, "2026-01-01T00:01:00Z", "k2", nil),
		}
		stored[0].CacheCreationInputTokens = 1
		stored[1].CacheCreationInputTokens = 3
		stored[0].CacheReadInputTokens = 5
		stored[1].CacheReadInputTokens = 7
		stored[0].ReasoningTokens = 11
		stored[1].ReasoningTokens = 13
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 20, 4, "2026-01-01T00:00:00Z", "k1", nil),
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:01:00Z", "k2", nil),
		}
		parsed[0].CacheCreationInputTokens = 3
		parsed[1].CacheCreationInputTokens = 1
		parsed[0].CacheReadInputTokens = 7
		parsed[1].CacheReadInputTokens = 5
		parsed[0].ReasoningTokens = 13
		parsed[1].ReasoningTokens = 11

		diffs := compareUsageEvents(stored, parsed)

		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Equal(t, sumUsageTokenTotals(stored).render(), diffs[0].Stored)
		assert.Equal(t, sumUsageTokenTotals(parsed).render(), diffs[0].Parsed)
		assert.Contains(t, diffs[0].Detail, "event composition differs")
		assert.Contains(t, diffs[0].Detail, "dedup|k1")
	})

	t.Run("tuple fallback detects occurred_at drift", func(t *testing.T) {
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T09:09:09Z", "", nil),
		}
		diffs := compareUsageEvents(stored, parsed)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Contains(t, diffs[0].Detail, "event composition differs")
	})

	t.Run("dedup key drift in totals still reports totals", func(t *testing.T) {
		// Same dedup key but token drift: the multiset is equal by
		// key, yet the per-class sums must still be compared.
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 99, 2, "2026-01-01T00:00:00Z", "k1", nil),
		}
		diffs := compareUsageEvents(stored, parsed)
		require.Len(t, diffs, 1)
		assert.Equal(t, FieldUsageEventTotals, diffs[0].Field)
		assert.Equal(t, "input 10 -> 99", diffs[0].Detail)
	})

	t.Run("cost columns are ignored", func(t *testing.T) {
		cost := 0.42
		stored := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", nil),
		}
		stored[0].CostUSD = &cost
		stored[0].CostStatus = "final"
		stored[0].CostSource = "pricing"
		parsed := []db.UsageEvent{
			pdEvent("api", "m1", 10, 2, "2026-01-01T00:00:00Z", "", nil),
		}
		assert.Empty(t, compareUsageEvents(stored, parsed))
	})
}

// TestFingerprintTwinMatchesDB pins the in-memory fingerprint twin
// against db.MessageTokenFingerprint for messages written through the
// real bulk sync pipeline. Twin drift silently breaks the tier-1 fast
// path, so this parity is the design's top risk.
func TestFingerprintTwinMatchesDB(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:           "pd-twin-session",
			Project:      "proj",
			Machine:      "test-machine",
			Agent:        parser.AgentClaude,
			FirstMessage: "hello",
			StartedAt:    ts,
			EndedAt:      ts.Add(time.Minute),
			MessageCount: 3,
			File: parser.FileInfo{
				Path:  "/tmp/pd-twin.jsonl",
				Size:  100,
				Mtime: ts.UnixNano(),
			},
		},
		msgs: []parser.ParsedMessage{
			{
				Ordinal:       0,
				Role:          parser.RoleUser,
				Content:       "hello",
				ContentLength: 5,
				Timestamp:     ts,
				SourceType:    "user",
				SourceUUID:    "uuid-0",
			},
			{
				Ordinal:       1,
				Role:          parser.RoleAssistant,
				Content:       "hi with NUL \x00 and unicode ünï",
				ContentLength: 20,
				Timestamp:     ts.Add(time.Second),
				Model:         "claude-op\x00us-4",
				TokenUsage: json.RawMessage(
					`{"input_tokens":10,"output_tokens":2}`,
				),
				ContextTokens:    12,
				OutputTokens:     2,
				HasContextTokens: true,
				HasOutputTokens:  true,
				ClaudeMessageID:  "msg_01",
				ClaudeRequestID:  "req_01",
				SourceType:       "assistant",
				SourceUUID:       "uuid-1",
				SourceParentUUID: "uuid-0",
			},
			{
				Ordinal:           2,
				Role:              parser.RoleAssistant,
				Content:           "sidechain boundary",
				ContentLength:     18,
				Timestamp:         ts.Add(2 * time.Second),
				Model:             "claude-haiku",
				IsSidechain:       true,
				IsCompactBoundary: true,
			},
		},
	}

	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteBulk, false,
	)
	require.Equal(t, 1, written, "session must be written")
	require.Zero(t, failed)

	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)
	require.NotEmpty(t, msgs)

	storedFP, err := d.MessageTokenFingerprint(prepared.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedFP)

	assert.Equal(
		t, storedFP, messageTokenFingerprintTwin(msgs),
		"in-memory twin must match db.MessageTokenFingerprint exactly",
	)

	storedRoleTimeFP, err := d.MessageRoleTimeFingerprint(prepared.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedRoleTimeFP)

	assert.Equal(
		t, storedRoleTimeFP, messageRoleTimeFingerprintTwin(msgs),
		"in-memory twin must match db.MessageRoleTimeFingerprint exactly",
	)

	storedContentFP, err := d.MessageContentHashFingerprint(prepared.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedContentFP)

	assert.Equal(
		t, storedContentFP, messageContentHashFingerprintTwin(msgs),
		"in-memory twin must match db.MessageContentHashFingerprint exactly",
	)
}

// TestCompareStoredSessionRoundTrip proves that a session written
// through the real pipeline compares as identical against itself,
// covering the session row, message metadata, and usage events.
func TestCompareStoredSessionRoundTrip(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:                "pd-roundtrip",
			Project:           "proj",
			Machine:           "test-machine",
			Agent:             parser.AgentClaude,
			FirstMessage:      "round trip",
			SessionName:       "Round Trip",
			StartedAt:         ts,
			EndedAt:           ts.Add(time.Minute),
			MessageCount:      2,
			UserMessageCount:  1,
			TerminationStatus: parser.TerminationClean,
			File: parser.FileInfo{
				Path:  "/tmp/pd-roundtrip.jsonl",
				Size:  64,
				Mtime: ts.UnixNano(),
			},
		},
		msgs: []parser.ParsedMessage{
			{
				Ordinal:       0,
				Role:          parser.RoleUser,
				Content:       "round trip",
				ContentLength: 10,
				Timestamp:     ts,
			},
			{
				Ordinal:       1,
				Role:          parser.RoleAssistant,
				Content:       "ack",
				ContentLength: 3,
				Timestamp:     ts.Add(time.Second),
				Model:         "claude-sonnet",
				TokenUsage: json.RawMessage(
					`{"input_tokens":7,"output_tokens":3}`,
				),
				ContextTokens:    10,
				OutputTokens:     3,
				HasContextTokens: true,
				HasOutputTokens:  true,
			},
		},
		usageEvents: []parser.ParsedUsageEvent{
			{
				MessageOrdinal: new(1),
				Source:         "transcript",
				Model:          "claude-sonnet",
				InputTokens:    7,
				OutputTokens:   3,
				OccurredAt:     "2026-06-01T10:00:01Z",
				DedupKey:       "msg_rt:req_rt",
			},
		},
	}

	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteBulk, false,
	)
	require.Equal(t, 1, written)
	require.Zero(t, failed)

	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)
	events := toDBUsageEvents(prepared.ID, pw.usageEvents)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, events,
	)
	require.NoError(t, err)
	assert.Empty(t, diffs, "self-written session must be identical")
}

// TestCompareStoredSessionDetectsDrift mutates one stored column and
// expects the comparator to attribute it through the database path.
func TestCompareStoredSessionDetectsDrift(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:           "pd-drift",
			Project:      "proj",
			Machine:      "test-machine",
			Agent:        parser.AgentClaude,
			FirstMessage: "drift",
			StartedAt:    ts,
			MessageCount: 1,
			File: parser.FileInfo{
				Path:  "/tmp/pd-drift.jsonl",
				Size:  32,
				Mtime: ts.UnixNano(),
			},
		},
		msgs: []parser.ParsedMessage{
			{
				Ordinal:       0,
				Role:          parser.RoleAssistant,
				Content:       "drift",
				ContentLength: 5,
				Timestamp:     ts,
				Model:         "claude-sonnet",
				OutputTokens:  3,
			},
		},
	}
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteBulk, false,
	)
	require.Equal(t, 1, written)
	require.Zero(t, failed)

	// Simulate parser drift: the new parse reports a different model.
	pw.msgs[0].Model = "claude-haiku"
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldModels, diffs[0].Field)
	assert.Equal(t, "claude-sonnet", diffs[0].Stored)
	assert.Equal(t, "claude-haiku", diffs[0].Parsed)
	assert.Contains(t, diffs[0].Detail, "first at ordinal 0")
}

// pdWriteSingleMessageSession writes a one-message session through the
// real pipeline and returns the engine plus DB for re-parse comparison.
func pdWriteSingleMessageSession(
	t *testing.T, id string, msg parser.ParsedMessage,
) (*Engine, *db.DB, pendingWrite) {
	t.Helper()
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID: id, Project: "proj", Machine: "test-machine",
			Agent: parser.AgentClaude, FirstMessage: "x",
			StartedAt: ts, MessageCount: 1,
			File: parser.FileInfo{
				Path: "/tmp/" + id + ".jsonl", Size: 32, Mtime: ts.UnixNano(),
			},
		},
		msgs: []parser.ParsedMessage{msg},
	}
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteBulk, false,
	)
	require.Equal(t, 1, written)
	require.Zero(t, failed)
	return e, d, pw
}

// TestCompareStoredSessionDetectsContentDrift proves the content tier
// catches a message-body change the token fingerprint cannot see: only
// the content length moves, model and tokens are unchanged.
func TestCompareStoredSessionDetectsContentDrift(t *testing.T) {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	e, d, pw := pdWriteSingleMessageSession(t, "pd-content", parser.ParsedMessage{
		Ordinal: 0, Role: parser.RoleAssistant,
		Content: "short", ContentLength: 5, Timestamp: ts,
		Model: "claude-sonnet",
	})

	// New parse: same model and tokens, longer body.
	pw.msgs[0].Content = "a much longer reply body"
	pw.msgs[0].ContentLength = len(pw.msgs[0].Content)
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldMessageContent, diffs[0].Field)
	assert.Contains(t, diffs[0].Detail, "first at ordinal 0")
}

// TestCompareStoredSessionDetectsMetadataDrift proves a fingerprint
// mismatch confined to a non-model/token field (is_sidechain) is
// surfaced as message_metadata rather than silently reported identical.
func TestCompareStoredSessionDetectsMetadataDrift(t *testing.T) {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	e, d, pw := pdWriteSingleMessageSession(t, "pd-meta", parser.ParsedMessage{
		Ordinal: 0, Role: parser.RoleAssistant,
		Content: "body", ContentLength: 4, Timestamp: ts,
		Model: "claude-sonnet", IsSidechain: false,
	})

	// New parse flips only is_sidechain: same model, tokens, content.
	pw.msgs[0].IsSidechain = true
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
	assert.Contains(t, diffs[0].Detail, "is_sidechain")
}

// pdToolSession builds a Claude pendingWrite that exercises the
// flags and tool-call comparison paths: a thinking block, two tool calls
// (one with a paired result), and a system message.
func pdToolSession(id string) pendingWrite {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	return pendingWrite{
		sess: parser.ParsedSession{
			ID: id, Project: "proj", Machine: "test-machine",
			Agent: parser.AgentClaude, FirstMessage: "do a thing",
			StartedAt: ts, MessageCount: 3,
			File: parser.FileInfo{
				Path: "/tmp/" + id + ".jsonl", Size: 128, Mtime: ts.UnixNano(),
			},
		},
		msgs: []parser.ParsedMessage{
			{
				Ordinal: 0, Role: parser.RoleUser,
				Content: "do a thing", ContentLength: 10, Timestamp: ts,
			},
			{
				Ordinal: 1, Role: parser.RoleAssistant,
				Content: "working on it", ContentLength: 13,
				Timestamp:   ts.Add(time.Second),
				Model:       "claude-sonnet",
				HasThinking: true, ThinkingText: "let me reason about this",
				HasToolUse: true,
				ToolCalls: []parser.ParsedToolCall{
					{
						ToolUseID: "tu1", ToolName: "Read",
						Category: "Read", InputJSON: `{"file":"a.go"}`,
					},
					{
						ToolUseID: "tu2", ToolName: "Bash",
						Category: "Bash", InputJSON: `{"cmd":"ls"}`,
					},
				},
			},
			{
				Ordinal: 2, Role: parser.RoleUser,
				Content: "looks good", ContentLength: 10,
				Timestamp: ts.Add(2 * time.Second),
				IsSystem:  true,
				ToolResults: []parser.ParsedToolResult{
					{
						ToolUseID: "tu1", ContentLength: 18,
						ContentRaw: `"file contents x"`,
					},
				},
			},
		},
	}
}

// pdWriteToolSession writes pdToolSession through the real pipeline.
func pdWriteToolSession(
	t *testing.T, id string,
) (*Engine, *db.DB, pendingWrite) {
	t.Helper()
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	pw := pdToolSession(id)
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteBulk, false,
	)
	require.Equal(t, 1, written)
	require.Zero(t, failed)
	return e, d, pw
}

// TestToolCallAndFlagsFingerprintTwinsMatchDB pins the two new in-memory
// twins against their DB queries through the real write pipeline, the
// way TestFingerprintTwinMatchesDB does for the message fingerprints.
func TestToolCallAndFlagsFingerprintTwinsMatchDB(t *testing.T) {
	e, d, pw := pdWriteToolSession(t, "pd-tool-twin")
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	storedFlagsFP, err := d.MessageFlagsFingerprint(prepared.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedFlagsFP)
	assert.Equal(t, storedFlagsFP, messageFlagsFingerprintTwin(msgs),
		"flags twin must match db.MessageFlagsFingerprint exactly")

	storedToolFP, err := d.ToolCallParseDiffFingerprint(prepared.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedToolFP)
	assert.Equal(t, storedToolFP, toolCallParseDiffFingerprintTwin(msgs),
		"tool-call twin must match db.ToolCallParseDiffFingerprint exactly")
}

// TestCompareStoredSessionRoundTripToolCalls is the false-diff acid test
// for the new fields: a session with tool calls, a thinking block, and a
// system message must compare identical against itself.
func TestCompareStoredSessionRoundTripToolCalls(t *testing.T) {
	e, d, pw := pdWriteToolSession(t, "pd-tool-rt")
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	assert.Empty(t, diffs,
		"tool calls, thinking, and a system message must round-trip clean")
}

// TestCompareStoredSessionDetectsToolCallDrift proves a change confined
// to a tool_call column is caught: none of the message
// token/role/content/flags fingerprints move, so it surfaces only if the
// tool-call fingerprint triggers the tier-2 comparison.
func TestCompareStoredSessionDetectsToolCallDrift(t *testing.T) {
	e, d, pw := pdWriteToolSession(t, "pd-tool-drift")
	pw.msgs[1].ToolCalls[0].ToolName = "Grep"
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldToolCalls, diffs[0].Field)
	assert.Contains(t, diffs[0].Detail, `tool_name "Read" -> "Grep"`)
}

// TestCompareStoredSessionDetectsFlagDrift proves a change confined to a
// per-message flag (has_thinking) is caught only via the flags
// fingerprint triggering the tier-2 comparison.
func TestCompareStoredSessionDetectsFlagDrift(t *testing.T) {
	e, d, pw := pdWriteToolSession(t, "pd-flag-drift")
	pw.msgs[1].HasThinking = false
	prepared, msgs, ok := e.prepareSessionWrite(pw, nil)
	require.True(t, ok)

	stored := pdFetchStored(t, d, prepared.ID)
	diffs, err := e.compareStoredSession(
		context.Background(), stored, prepared, msgs, nil,
	)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
	assert.Contains(t, diffs[0].Detail, "has_thinking true -> false")
}

func pdFetchStored(t *testing.T, d *db.DB, id string) *db.Session {
	t.Helper()
	sessions, err := d.ListSessionsModifiedBetween(
		context.Background(), "", "", nil, nil,
	)
	require.NoError(t, err)
	for i := range sessions {
		if sessions[i].ID == id {
			return &sessions[i]
		}
	}
	require.Failf(t, "session not found", "id %s", id)
	return nil
}

func TestParseDiffClassifyPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		needsRetry    bool
		prepared      bool
		hasStored     bool
		storedTrashed bool
		pendingResync bool
		realDiffs     int
		wantClass     DiffClass
		wantReason    string
	}{
		{
			name:       "needs retry wins over everything",
			needsRetry: true, prepared: false, hasStored: true,
			storedTrashed: true, pendingResync: true, realDiffs: 3,
			wantClass:  DiffNeedsRetry,
			wantReason: "transient low-fidelity parse; differences expected",
		},
		{
			name:     "archive-preserve veto wins over missing stored row",
			prepared: false, hasStored: false,
			wantClass:  DiffExcluded,
			wantReason: "archive-preserved",
		},
		{
			name:     "no stored row is new on disk",
			prepared: true, hasStored: false,
			wantClass: DiffNewOnDisk,
		},
		{
			name:     "trashed stored row is skipped, not excluded",
			prepared: true, hasStored: true, storedTrashed: true,
			pendingResync: true, realDiffs: 2,
			wantClass:  DiffSkipped,
			wantReason: "trashed in archive",
		},
		{
			name:     "pending resync wins over changed",
			prepared: true, hasStored: true, pendingResync: true,
			realDiffs: 2,
			wantClass: DiffPendingResync,
		},
		{
			name:     "real diffs mean changed",
			prepared: true, hasStored: true, realDiffs: 1,
			wantClass: DiffChanged,
		},
		{
			name:     "no real diffs mean identical",
			prepared: true, hasStored: true, realDiffs: 0,
			wantClass: DiffIdentical,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, reason := classifyParseDiffSession(
				tt.needsRetry, tt.prepared, tt.hasStored,
				tt.storedTrashed, tt.pendingResync, tt.realDiffs,
			)
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestParseDiffReportHasFailures(t *testing.T) {
	tests := []struct {
		name   string
		totals ParseDiffTotals
		want   bool
	}{
		{name: "empty report has no failures"},
		{
			name:   "identical only",
			totals: ParseDiffTotals{Identical: 10, Examined: 10},
		},
		{
			name:   "changed sessions fail",
			totals: ParseDiffTotals{Changed: 1},
			want:   true,
		},
		{
			name:   "parse errors fail",
			totals: ParseDiffTotals{ParseErrors: 1},
			want:   true,
		},
		{
			name:   "both fail",
			totals: ParseDiffTotals{Changed: 2, ParseErrors: 3},
			want:   true,
		},
		{
			name: "pending resync, skipped, retry, new do not fail",
			totals: ParseDiffTotals{
				PendingResync: 4, Skipped: 9,
				NeedsRetry: 2, NewOnDisk: 3,
				ExcludedByParser: 1, InformationalOnly: 5,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ParseDiffReport{Totals: tt.totals}
			assert.Equal(t, tt.want, r.HasFailures())
		})
	}
}
