//go:build pgtest || chtest

package activity_test

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// usageRuleMessage is one message of a usage-rules fixture session. An
// assistant message carries usage for model, or usageRuleModel when empty.
type usageRuleMessage struct {
	role, model              string
	ts                       string
	input, output            int
	claudeMessage, claudeReq string
	sourceUUID               string
}

type usageRuleSession struct {
	id, relationship, parent string
	messages                 []usageRuleMessage
}

// usageRuleModel prices input at 0.5 and output at 2 microdollars a token,
// so a row with one input token costs half a microdollar before rounding.
const usageRuleModel = "rule-model"

// usageRuleSessions exercise each usage rule on its own sessions:
//
//   - snap-*: one Claude response held by a root, its fork, and its
//     subagent. The fork's 40-token snapshot is the fullest; the root's
//     transcript holds the response first, so the root is credited.
//   - tie-*: two sessions hold equally full snapshots at the same instant;
//     the lower session ID is credited.
//   - uuid-*: two sessions share a source UUID at the same instant; the
//     first in report order, the lower session ID, keeps the row.
//   - null-1: a usage row without a timestamp counts at its session's
//     start; null-2, without a start either, never counts.
//   - round-1: three rows of half a microdollar each cost one microdollar
//     each, three in total, where rounding the sum would give two.
//   - edge-1: rows at the range start and at the range end; only the first
//     is inside the half-open day.
//   - zero-1: a row without tokens, of a model no other row uses, costs
//     nothing and leaves the pricing block as SQLite's.
func usageRuleSessions() []usageRuleSession {
	const day = "2026-06-14T"
	return []usageRuleSession{
		{id: "snap-root", messages: []usageRuleMessage{
			{role: "user", ts: day + "10:00:00Z"},
			{role: "assistant", ts: day + "10:01:00Z", input: 1, output: 10, claudeMessage: "msg-a", claudeReq: "req-a"},
		}},
		{id: "snap-fork", relationship: "fork", parent: "snap-root", messages: []usageRuleMessage{
			{role: "user", ts: day + "10:00:30Z"},
			{role: "assistant", ts: day + "10:01:20Z", input: 1, output: 40, claudeMessage: "msg-a", claudeReq: "req-a"},
		}},
		{id: "snap-sub", relationship: "subagent", parent: "snap-root", messages: []usageRuleMessage{
			{role: "user", ts: day + "10:02:00Z"},
			{role: "assistant", ts: day + "10:02:30Z", input: 1, output: 25, claudeMessage: "msg-a", claudeReq: "req-a"},
		}},
		{id: "tie-a", messages: []usageRuleMessage{
			{role: "user", ts: day + "10:59:00Z"},
			{role: "assistant", ts: day + "11:00:00Z", input: 1, output: 7, claudeMessage: "msg-b", claudeReq: "req-b"},
		}},
		{id: "tie-b", messages: []usageRuleMessage{
			{role: "user", ts: day + "10:59:30Z"},
			{role: "assistant", ts: day + "11:00:00Z", input: 1, output: 7, claudeMessage: "msg-b", claudeReq: "req-b"},
		}},
		{id: "uuid-1", messages: []usageRuleMessage{
			{role: "user", ts: day + "11:59:00Z"},
			{role: "assistant", ts: day + "12:00:00Z", input: 1, output: 3, sourceUUID: "uuid-shared"},
		}},
		{id: "uuid-2", messages: []usageRuleMessage{
			{role: "user", ts: day + "11:59:30Z"},
			{role: "assistant", ts: day + "12:00:00Z", input: 1, output: 3, sourceUUID: "uuid-shared"},
		}},
		{id: "null-1", messages: []usageRuleMessage{
			{role: "user", ts: day + "14:00:00Z"},
			{role: "assistant", ts: "", input: 1, output: 100, claudeMessage: "msg-n", claudeReq: "req-n"},
			{role: "user", ts: day + "14:01:00Z"},
		}},
		{id: "null-2", messages: []usageRuleMessage{
			{role: "user", ts: ""},
			{role: "assistant", ts: "", input: 1, output: 50, claudeMessage: "msg-m", claudeReq: "req-m"},
		}},
		{id: "round-1", messages: []usageRuleMessage{
			{role: "user", ts: day + "12:59:00Z"},
			{role: "assistant", ts: day + "13:00:00Z", input: 1, claudeMessage: "msg-r1", claudeReq: "req-r1"},
			{role: "assistant", ts: day + "13:01:00Z", input: 1, claudeMessage: "msg-r2", claudeReq: "req-r2"},
			{role: "assistant", ts: day + "13:02:00Z", input: 1, claudeMessage: "msg-r3", claudeReq: "req-r3"},
		}},
		{id: "edge-1", messages: []usageRuleMessage{
			{role: "user", ts: "2026-06-13T23:59:00Z"},
			{role: "assistant", ts: day + "00:00:00Z", output: 5, claudeMessage: "msg-e1", claudeReq: "req-e1"},
			{role: "user", ts: "2026-06-14T23:59:30Z"},
			{role: "assistant", ts: "2026-06-15T00:00:00Z", output: 5, claudeMessage: "msg-e2", claudeReq: "req-e2"},
		}},
		{id: "zero-1", messages: []usageRuleMessage{
			{role: "user", ts: day + "15:00:00Z"},
			{role: "assistant", model: "zero-model", ts: day + "15:01:00Z", claudeMessage: "msg-z", claudeReq: "req-z"},
		}},
	}
}

func seedUsageRulesSQLite(t *testing.T) *db.DB {
	t.Helper()
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "usage-rules.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: usageRuleModel, InputPerMTok: money.MustParseDollars("0.5"), OutputPerMTok: money.MustParseDollars("2"),
	}}))
	var writes []db.SessionBatchWrite
	for _, fs := range usageRuleSessions() {
		relationship := fs.relationship
		if relationship == "" {
			relationship = "root"
		}
		first, last := fs.messages[0].ts, fs.messages[len(fs.messages)-1].ts
		label := "usage rules " + fs.id
		session := db.Session{
			ID: fs.id, Project: "usage-rules", Machine: "local", Agent: "claude",
			FirstMessage: &label, CreatedAt: "2026-06-14T00:00:00Z",
			MessageCount: len(fs.messages), UserMessageCount: 1, RelationshipType: relationship, DataVersion: 1,
		}
		if first != "" {
			session.StartedAt, session.EndedAt, session.LocalModifiedAt = &first, &last, &first
		}
		if fs.parent != "" {
			session.ParentSessionID = &fs.parent
		}
		messages := make([]db.Message, len(fs.messages))
		for i, m := range fs.messages {
			messages[i] = db.Message{
				SessionID: fs.id, Ordinal: i, Role: m.role, Content: m.role, ContentLength: len(m.role),
				Timestamp: m.ts, ClaudeMessageID: m.claudeMessage, ClaudeRequestID: m.claudeReq, SourceUUID: m.sourceUUID,
			}
			if m.role != "assistant" {
				continue
			}
			usage, err := json.Marshal(map[string]int{"input_tokens": m.input, "output_tokens": m.output})
			require.NoError(t, err)
			messages[i].Model, messages[i].TokenUsage = cmp.Or(m.model, usageRuleModel), usage
			messages[i].OutputTokens, messages[i].HasOutputTokens = m.output, true
		}
		writes = append(writes, db.SessionBatchWrite{
			Session: session, Messages: messages, DataVersion: 1, ReplaceMessages: true,
		})
	}
	_, err = local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	return local
}

type usageRuleTotals struct {
	outputTokens int
	cost         int64
}

// TestActivityReportUsageRulesAcrossBackends checks each backend's report
// against hand-computed usage, for a complete day and for the same day
// still in progress.
func TestActivityReportUsageRulesAcrossBackends(t *testing.T) {
	ctx := t.Context()
	local := seedUsageRulesSQLite(t)
	sessions := len(usageRuleSessions())
	type reporter interface {
		GetActivityReport(ctx context.Context, f db.AnalyticsFilter, q activity.Query) (activity.Report, error)
	}
	backends := map[string]reporter{"sqlite": local, "duckdb": pushParityDuckDB(t, ctx, local, sessions)}
	if os.Getenv("TEST_PG_URL") != "" {
		backends["postgres"] = pushParityPostgres(t, ctx, local, sessions)
	}
	if store := pushParityClickHouse(t, ctx, local, sessions); store != nil {
		backends["clickhouse"] = store
	}
	cases := []struct {
		name     string
		now      string
		total    usageRuleTotals
		sessions map[string]usageRuleTotals
	}{
		{
			// snap-root 1*0.5+40*2 = 80.5 -> 81; tie-a 14.5 -> 15;
			// uuid-1 6.5 -> 7; null-1 200.5 -> 201; round-1 3 * (0.5 -> 1);
			// edge-1 5*2.
			name: "complete day", now: "2030-01-01T00:00:00Z",
			total: usageRuleTotals{155, 317},
			sessions: map[string]usageRuleTotals{
				"snap-root": {40, 81}, "tie-a": {7, 15}, "uuid-1": {3, 7}, "null-1": {100, 201},
				"round-1": {0, 3}, "edge-1": {5, 10},
			},
		},
		{
			// Before 10:01:20 only the root's 10-token snapshot is in range,
			// so it is the fullest: 1*0.5+10*2 = 20.5 -> 21.
			name: "day in progress", now: "2026-06-14T10:01:20Z",
			total: usageRuleTotals{15, 31},
			sessions: map[string]usageRuleTotals{
				"snap-root": {10, 21}, "edge-1": {5, 10},
			},
		},
	}
	for _, tc := range cases {
		now, err := time.Parse(time.RFC3339, tc.now)
		require.NoError(t, err)
		q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-06-14", Timezone: "UTC"}, now)
		require.NoError(t, err)
		sqliteReport, err := local.GetActivityReport(ctx, db.AnalyticsFilter{Timezone: "UTC"}, q)
		require.NoError(t, err)
		for name, backend := range backends {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				report, err := backend.GetActivityReport(ctx, db.AnalyticsFilter{Timezone: "UTC"}, q)
				require.NoError(t, err)
				if name != "sqlite" {
					// SQLite is the reference pricing block.
					require.Equal(t, sqliteReport.Pricing, report.Pricing)
				}
				require.Equal(t, tc.total, usageRuleTotals{report.Totals.OutputTokens, report.Totals.Cost.Microdollars})
				got := map[string]usageRuleTotals{}
				for _, row := range report.BySession {
					if row.OutputTokens != 0 || row.Cost.Microdollars != 0 {
						got[row.SessionID] = usageRuleTotals{row.OutputTokens, row.Cost.Microdollars}
					}
				}
				require.Equal(t, tc.sessions, got)
			})
		}
	}
}
