//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageCSVFilters(t *testing.T) {
	for _, source := range []string{"messages", "snapshots", "cursor"} {
		t.Run(source, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertModelPricing([]ModelPricing{
				{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("1")},
				{ModelPattern: "model-b", InputPerMTok: money.MustParseDollars("1")},
			}))
			if source == "cursor" {
				require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{
					{OccurredAt: "2026-08-10T10:00:00Z", Model: "model-a", InputTokens: 100000, Charged: money.MustParseDollars("0.10"), DedupKey: "a"},
					{OccurredAt: "2026-08-10T10:00:00Z", Model: "model-b", InputTokens: 200000, Charged: money.MustParseDollars("0.20"), DedupKey: "b"},
				}))
			} else {
				ids := []string{"session-a"}
				if source == "snapshots" {
					ids = append(ids, "session-b")
				}
				for _, id := range ids {
					insertSession(t, d, id, "project-a", func(s *Session) {
						s.Agent, s.Machine = "claude", "host-a"
						s.StartedAt = new("2026-08-10T10:00:00Z")
					})
					messages := []Message{
						{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T10:00:00Z", Model: "model-a", TokenUsage: []byte(`{"input_tokens":100000}`)},
						{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T10:00:00Z", Model: "model-b", TokenUsage: []byte(`{"input_tokens":200000}`)},
					}
					if source == "snapshots" {
						messages[0].ClaudeMessageID, messages[0].ClaudeRequestID = "message-a", "request-a"
						messages[1].ClaudeMessageID, messages[1].ClaudeRequestID = "message-b", "request-b"
					}
					insertMessages(t, d, messages...)
				}
			}
			type filterCase struct {
				name   string
				filter UsageFilter
				tokens int
				cost   string
			}
			cases := []filterCase{
				{"models", UsageFilter{Model: " model-a, model-b, "}, 300000, "0.30"},
				{"exclude-models", UsageFilter{ExcludeModel: "missing, model-b, "}, 100000, "0.10"},
				{"blank-models", UsageFilter{Model: " , ", ExcludeModel: " , "}, 300000, "0.30"},
				{"blank-session-filters", UsageFilter{Agent: " , ", ExcludeAgent: " , ", Machine: " , ", Project: " , ", ExcludeProject: " , "}, 300000, "0.30"},
			}
			if source != "cursor" {
				cases = append(cases, []filterCase{
					{"session-filters", UsageFilter{Agent: "missing, claude", Machine: "missing, host-a", Project: "missing, project-a"}, 300000, "0.30"},
					{"exclude-agents", UsageFilter{ExcludeAgent: "missing, claude"}, 0, "0"},
					{"exclude-projects", UsageFilter{ExcludeProject: "missing, project-a"}, 0, "0"},
					{"exact-labels", UsageFilter{ProjectLabels: []string{" project-a"}}, 0, "0"},
				}...)
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					c.filter.From, c.filter.To, c.filter.Timezone = "2026-08-10", "2026-08-10", "UTC"
					for _, cached := range []bool{false, true} {
						var result DailyUsageResult
						if cached {
							result = getDailyUsageRollupForTest(t, d, c.filter)
						} else {
							result = getDailyUsageLegacyForRollupTest(t, d, c.filter)
						}
						assert.Equal(t, c.tokens, result.Totals.InputTokens)
						assert.Equal(t, money.MustParseDollars(c.cost), result.Totals.TotalCost)
					}
				})
			}
		})
	}
}
