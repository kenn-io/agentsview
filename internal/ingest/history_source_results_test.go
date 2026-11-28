package ingest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
)

// A CodeBuddy branch restores a removed saved output from the sessions its
// transcript archived, ignoring its own stale copy and refusing when the
// other sessions disagree.
func TestReconcileProviderHistoryCodeBuddySourceToolResults(t *testing.T) {
	ts := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	tsStr := timeutil.Format(ts)
	placeholder := "Full output saved to: /x/tool-results/c1.txt"
	persistedPlaceholder := "<persisted-output>\nOutput too large (1KB). Full output saved to: /x/tool-results/c1.txt\n</persisted-output>"
	saved := func(sessionID, content string) (string, []db.Message) {
		return sessionID, []db.Message{{
			SessionID: sessionID, Ordinal: 0, Role: "assistant", Timestamp: tsStr,
			ToolCalls: []db.ToolCall{{
				ToolUseID: "c1", Category: "bash",
				ResultContent: content, ResultContentLength: len(content),
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID: "c1", Source: "tool_result", Timestamp: tsStr,
					Content: content, ContentLength: len(content),
				}},
			}},
		}}
	}
	parsed := []parser.ParsedMessage{{
		Ordinal: 0, Timestamp: ts,
		ToolResults: []parser.ParsedToolResult{{
			ToolUseID: "c1", Source: "tool_result",
			PersistedOutputUnresolved: true,
		}},
	}}
	type archive struct {
		id      string
		content string
	}
	tests := []struct {
		name     string
		own      string
		archives []archive
		want     string
	}{
		{
			name: "the conversation's copy fills the branch",
			archives: []archive{
				{"codebuddy:s1", "full output"},
				{"codebuddy:s1-u1", placeholder},
			},
			want: "full output",
		},
		{
			name: "agreeing sessions fill the branch",
			archives: []archive{
				{"codebuddy:s1", "full output"},
				{"codebuddy:s1-u2", "full output"},
			},
			want: "full output",
		},
		{
			name: "disagreeing sessions leave the placeholder",
			archives: []archive{
				{"codebuddy:s1", "full output"},
				{"codebuddy:s1-u2", "other output"},
			},
			want: placeholder,
		},
		{
			name: "the branch's own archive wins",
			own:  "own output",
			archives: []archive{
				{"codebuddy:s1", "full output"},
			},
			want: "own output",
		},
		{
			name: "a saved placeholder takes the full output others agree on",
			own:  persistedPlaceholder,
			archives: []archive{
				{"codebuddy:s1", "full output"},
				{"codebuddy:s1-u2", persistedPlaceholder},
			},
			want: "full output",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index := &ingest.SourceToolResults{}
			index.Want(parsed)
			for _, a := range tc.archives {
				index.Add(saved(a.id, a.content))
			}
			prior := &ingest.PriorSession{SourceToolResults: index}
			if tc.own != "" {
				_, prior.Messages = saved("codebuddy:s1-u1", tc.own)
			}
			_, current := saved("codebuddy:s1-u1", placeholder)
			candidate := ingest.Candidate{
				Session:  db.Session{ID: "codebuddy:s1-u1", Agent: string(parser.AgentCodeBuddy)},
				Messages: current,
				Parsed: parser.ParseResult{
					Session: parser.ParsedSession{
						ID: "s1-u1", Agent: parser.AgentCodeBuddy,
						File: parser.FileInfo{Path: "/synthetic/projects/demo/s1.jsonl"},
					},
					Messages: parsed,
				},
			}

			result, err := ingest.ReconcileProviderHistory(
				t.Context(), candidate, prior, ingest.ContentOptions{},
			)
			require.NoError(t, err)
			call := result.Candidate.Messages[0].ToolCalls[0]
			assert.Equal(t, tc.want, call.ResultEvents[0].Content)
			assert.Equal(t, tc.want, call.ResultContent)
		})
	}
}

// The index keeps only outputs a branch asked for, so sessions with no
// missing saved outputs add nothing to it.
func TestSourceToolResultsKeepsOnlyRequestedOutputs(t *testing.T) {
	index := &ingest.SourceToolResults{}
	assert.False(t, index.Wants())
	index.Want([]parser.ParsedMessage{{
		Timestamp: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC),
		ToolResults: []parser.ParsedToolResult{{
			ToolUseID: "c1", Source: "tool_result", PersistedOutputUnresolved: true,
		}},
	}})
	require.True(t, index.Wants())
	index.Add("codebuddy:s1", []db.Message{{
		ToolCalls: []db.ToolCall{{ResultEvents: []db.ToolResultEvent{{
			ToolUseID: "c2", Source: "tool_result",
			Timestamp: timeutil.Format(time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)),
			Content:   "unrelated output",
		}}}},
	}})
	assert.False(t, index.Found(), "an output no branch asked for is not kept")
}
