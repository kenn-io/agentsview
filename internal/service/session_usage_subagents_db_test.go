package service_test

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

// subagentFixture is a parent Claude session with two subagent transcripts.
// One of the parent's rows is repeated inside a subagent transcript under the
// same claude_message_id/claude_request_id, which is what Claude Code does
// when a sidechain record echoes a parent turn: it must be counted once.
//
// Rates are $2/Mtok input and $10/Mtok output, so:
//
//	parent  m-1: 1000 in +  500 out = $0.002 + $0.005 = $0.007
//	agent-a m-2: 2000 in + 1000 out = $0.004 + $0.010 = $0.014
//	agent-a m-1: later repeat of the parent's row          (survives)
//	agent-b m-3:  500 in +  100 out = $0.001 + $0.001 = $0.002
//
// Combined: $0.023. Naively summing per-session results would give $0.030.
//
// The per-session total_output_tokens aggregates below are what the Claude
// parser derives from each transcript in isolation, so agent-a's includes
// the echoed row: 500 + 1500 + 100 = 2100 naively, against 1600 once the
// echo is deduplicated. The combined view must report 1600, matching the
// cost in the same document and the day aggregate.
const (
	subagentParentID   = "claude:parent-1"
	subagentChildAID   = "agent-a1"
	subagentChildBID   = "agent-b1"
	subagentUsageDay   = "2026-05-20"
	subagentParentCost = "0.007"
	subagentChildACost = "0.014"
	subagentChildBCost = "0.002"
	subagentTotalCost  = "0.023"
	// Deduplicated output tokens: 500 (later agent-a copy attributed to the
	// parent) + 1000 (agent-a's own row) + 100 (agent-b).
	subagentTotalOutput = 1600
	// What summing the stored per-transcript aggregates would give.
	subagentNaiveOutput = 2100
)

func seedSubagentUsageFixture(t *testing.T, d *db.DB) {
	t.Helper()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parent := subagentParentID
	seedSubagentSession(t, d, subagentParentID, nil, "", 500)
	// 1500 = agent-a's own 1000 plus the 500 it echoed from the parent,
	// exactly as the parser derives it from that transcript alone.
	seedSubagentSession(t, d, subagentChildAID, &parent, "subagent", 1500)
	seedSubagentSession(t, d, subagentChildBID, &parent, "subagent", 100)

	dbtest.SeedMessages(t,
		d,
		usageMessage(subagentParentID, 0, "10:00:00", "m-1", 1000, 500),
		usageMessage(subagentChildAID, 0, "10:01:00", "m-2", 2000, 1000),
		// Same message identity and output count as the parent's row, later in
		// time, so it supplies the surviving usage while attribution remains
		// with the parent.
		usageMessage(subagentChildAID, 1, "10:02:00", "m-1", 1000, 500),
		usageMessage(subagentChildBID, 0, "10:03:00", "m-3", 500, 100),
	)
}

func seedSubagentSession(
	t *testing.T, d *db.DB, id string, parentID *string,
	relationship string, outputTokens int,
) {
	t.Helper()
	started := subagentUsageDay + "T09:00:00Z"
	dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.ParentSessionID = parentID
		s.RelationshipType = relationship
		s.TotalOutputTokens = outputTokens
		s.HasTotalOutputTokens = true
	})
}

func usageMessage(
	sessionID string, ordinal int, clock, messageID string, in, out int,
) db.Message {
	msg := dbtest.AsstMsg(sessionID, ordinal, "work")
	msg.Timestamp = subagentUsageDay + "T" + clock + "Z"
	msg.Model = "test-opus"
	msg.ClaudeMessageID = messageID
	msg.ClaudeRequestID = "req-" + messageID
	msg.TokenUsage = jsontext.Value(fmt.Sprintf(
		`{"input_tokens":%d,"output_tokens":%d}`, in, out))
	return msg
}

func TestSessionUsageWithRequiredSubagentsRequiresPerSessionContextCoverage(
	t *testing.T,
) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	parent := subagentParentID
	started := subagentUsageDay + "T09:00:00Z"
	dbtest.SeedSession(t, d, subagentParentID, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.TotalOutputTokens = 10
		s.HasTotalOutputTokens = true
		s.PeakContextTokens = 2000
		s.HasPeakContextTokens = true
	})
	dbtest.SeedSession(t, d, subagentChildAID, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
		s.TotalOutputTokens = 10
		s.HasTotalOutputTokens = true
		s.PeakContextTokens = 1000
		s.HasPeakContextTokens = true
	})
	dbtest.SeedMessages(t, d,
		usageMessage(subagentParentID, 0, "10:00:00", "m-parent", 2000, 10),
		usageMessage(subagentChildAID, 0, "10:01:00", "m-child", 0, 10),
	)

	got, _, err := service.SessionUsageWithRequiredSubagents(
		ctx, d, subagentParentID, []string{subagentChildAID}, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	_, complete, err := service.SessionUsageTokenTotals(ctx, got)
	require.NoError(t, err)
	assert.False(t, complete,
		"the parent's context must not cover the child's missing context categories")
	assert.False(t, got.HasCost,
		"computed cost must not omit the child's uncovered context categories")
	assert.Zero(t, got.Cost)
	assert.Nil(t, got.CostUSD)
}

func TestSessionUsageWithSubagentsOverSQLiteCombinesAndDedupes(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	seedSubagentUsageFixture(t, d)

	own, err := d.GetSessionUsage(ctx, subagentParentID, true)
	require.NoError(t, err)
	require.NotNil(t, own)
	require.Equal(t, money.MustParseDollars(subagentParentCost), own.Cost,
		"the own-session path must keep reporting only the parent's rows")
	require.Equal(t, 500, own.TotalOutputTokens)
	require.Zero(t, own.SubagentCount)

	got, err := service.SessionUsageWithSubagents(
		ctx, d, subagentParentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, subagentParentID, got.SessionID)
	assert.Equal(t, 2, got.SubagentCount)
	assert.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars(subagentTotalCost), got.Cost,
		"the shared row must be counted once, not once per transcript")
	assert.Equal(t, 3, got.BreakdownCount)

	// The echoed row must be deduplicated out of every figure in the
	// document, not just cost. Summing the stored per-transcript
	// aggregates (500 + 1500 + 100) would report it twice.
	assert.Equal(t, subagentTotalOutput, got.TotalOutputTokens,
		"output tokens must be deduplicated like cost is")
	assert.NotEqual(t, subagentNaiveOutput, got.TotalOutputTokens,
		"the stored per-session aggregates must not be summed naively")
	assert.True(t, got.HasTokenData)

	require.Len(t, got.Breakdown, 3)
	assert.Equal(t, []string{subagentChildAID, subagentChildAID, subagentChildBID},
		[]string{
			got.Breakdown[0].SubagentSessionID,
			got.Breakdown[1].SubagentSessionID,
			got.Breakdown[2].SubagentSessionID,
		}, "rows are ordered by timestamp and tagged with their session")
	assert.Equal(t, []int{1, 2, 3}, []int{
		got.Breakdown[0].Ordinal,
		got.Breakdown[1].Ordinal,
		got.Breakdown[2].Ordinal,
	})
	assert.Equal(t, "message", got.Breakdown[1].Source)
	assert.Equal(t, money.MustParseDollars(subagentChildACost),
		got.Breakdown[0].Cost)
	assert.Equal(t, money.MustParseDollars(subagentParentCost),
		got.Breakdown[1].Cost)
	assert.Equal(t, money.MustParseDollars(subagentChildBCost),
		got.Breakdown[2].Cost)
	assert.Equal(t, 2000, got.Breakdown[0].InputTokens)
	assert.Equal(t, 1000, got.Breakdown[0].OutputTokens)
}

func TestSessionUsageWithSubagentsDoesNotRestoreDedupedOnlyChildTokens(
	t *testing.T,
) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:dedup-only-parent"
	childID := "agent-dedup-only"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 500)
	seedSubagentSession(t, d, childID, &parent, "subagent", 500)
	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-shared", 1000, 500),
		usageMessage(childID, 0, "10:01:00", "m-shared", 1000, 500),
	)

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 1, got.SubagentCount)
	assert.Equal(t, 1, got.BreakdownCount)
	assert.Equal(t, 500, got.TotalOutputTokens,
		"a child whose only usage row was deduplicated must contribute zero")
	assert.True(t, got.HasCost,
		"a contributing deduplicated row still covers the child's token data")
}

func TestSessionUsageWithSubagentsPreservesOutputOnlyTokensAcrossSnapshots(
	t *testing.T,
) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:cross-snapshot-parent"
	childID := "agent-cross-snapshot-child"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 105)
	seedSubagentSession(t, d, childID, &parent, "subagent", 631)
	outputOnly := dbtest.AsstMsg(parentID, 1, "output-only")
	outputOnly.Timestamp = subagentUsageDay + "T10:02:00Z"
	outputOnly.Model = "test-opus"
	outputOnly.OutputTokens = 100
	outputOnly.HasOutputTokens = true
	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-stream", 1000, 5),
		usageMessage(childID, 0, "10:01:00", "m-stream", 1000, 631),
		outputOnly,
	)

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 731, got.TotalOutputTokens,
		"the complete snapshot and parent's output-only tokens both count")
	assert.False(t, got.HasCost,
		"the output-only residual has no usage row with complete cost coverage")
	assert.Zero(t, got.Cost)
	require.Len(t, got.Breakdown, 1)
	assert.Equal(t, 631, got.Breakdown[0].OutputTokens)
	assert.Equal(t, childID, got.Breakdown[0].SubagentSessionID,
		"the breakdown identifies the transcript that supplied the snapshot")
}

func TestSessionUsageRollupRecognizesChildSourcedAttributedSnapshot(
	t *testing.T,
) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:attributed-snapshot-parent"
	childID := "agent-attributed-snapshot-child"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 5)
	seedSubagentSession(t, d, childID, &parent, "subagent", 631)
	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-stream", 1000, 5),
		usageMessage(childID, 0, "10:01:00", "m-stream", 1000, 631),
	)

	got, err := service.GetSessionUsageRollup(ctx, d, parentID, false)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.True(t, got.HasCost,
		"a surviving row sourced from the child makes the rollup billable")
	assert.Equal(t, money.MustParseDollars("0.00831"), got.Cost)
}

func TestSessionUsageWithSubagentsPreservesOutputWithoutUsageRows(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:partial-token-parent"
	childID := "agent-partial-token-child"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 500)
	seedSubagentSession(t, d, childID, &parent, "subagent", 200)

	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-priced", 1000, 500),
	)

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 1, got.BreakdownCount)
	assert.Equal(t, 700, got.TotalOutputTokens,
		"a rowless subagent's stored output remains in the combined total")
	assert.False(t, got.HasCost,
		"priced rows do not cover the rowless subagent's stored output")
}

func TestSessionUsageWithSubagentsMarksRowlessContextIncomplete(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:context-token-parent"
	childID := "agent-context-token-child"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 500)
	started := subagentUsageDay + "T09:00:00Z"
	dbtest.SeedSession(t, d, childID, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
		s.PeakContextTokens = 1_000
		s.HasPeakContextTokens = true
	})
	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-priced", 1000, 500),
	)

	got, _, err := service.SessionUsageWithRequiredSubagents(
		ctx, d, parentID, []string{childID}, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 1, got.BreakdownCount)
	assert.Equal(t, 500, got.TotalOutputTokens)
	assert.Equal(t, 1_000, got.PeakContextTokens,
		"the rowless subagent's context high-water mark remains visible")
	assert.True(t, got.HasTokenData)
	assert.False(t, got.HasCost,
		"priced parent rows do not cover a rowless subagent's context tokens")
	totals, complete, err := service.SessionUsageTokenTotals(ctx, got)
	require.NoError(t, err)
	assert.False(t, complete,
		"parent rows do not prove the required subagent's input and cache usage")
	assert.Equal(t, 1_000, totals.InputTokens)
}

func TestSessionUsageWithSubagentsAllowsExplicitZeroValuedSubagent(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:zero-valued-parent"
	childID := "agent-zero-valued-child"
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 500)
	started := subagentUsageDay + "T09:00:00Z"
	dbtest.SeedSession(t, d, childID, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	dbtest.SeedMessages(t, d,
		usageMessage(parentID, 0, "10:00:00", "m-priced", 1000, 500),
	)

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 1, got.SubagentCount)
	assert.True(t, got.HasTokenData,
		"explicit zero-valued token metadata remains present")
	assert.True(t, got.HasCost,
		"zero-valued child metadata does not make parent cost incomplete")
	assert.Equal(t, money.MustParseDollars("0.007"), got.Cost)
}

func TestSessionUsageWithSubagentsIgnoresZeroRowForContextCoverage(t *testing.T) {
	tests := []struct {
		name      string
		messageID string
	}{
		{name: "surviving", messageID: "m-empty"},
		{name: "discarded duplicate", messageID: "m-priced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			ctx := context.Background()
			require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
				ModelPattern:  "test-opus",
				InputPerMTok:  money.MustParseDollars("2.0"),
				OutputPerMTok: money.MustParseDollars("10.0"),
			}}), "UpsertModelPricing")

			parentID := "claude:zero-row-parent"
			childID := "agent-zero-row-child"
			parent := parentID
			seedSubagentSession(t, d, parentID, nil, "", 500)
			started := subagentUsageDay + "T09:00:00Z"
			dbtest.SeedSession(t, d, childID, "proj", func(s *db.Session) {
				s.Agent = "claude"
				s.StartedAt = &started
				s.EndedAt = &started
				s.ParentSessionID = &parent
				s.RelationshipType = "subagent"
				s.PeakContextTokens = 2_000
				s.HasPeakContextTokens = true
			})
			dbtest.SeedMessages(t, d,
				usageMessage(
					parentID, 0, "10:00:00", "m-priced", 1000, 500),
				usageMessage(childID, 0, "10:01:00", tt.messageID, 0, 0),
			)

			got, err := service.SessionUsageWithSubagents(
				ctx, d, parentID, true)
			require.NoError(t, err)
			require.NotNil(t, got)

			assert.Equal(t, 1, got.BreakdownCount,
				"the zero-token row must not contribute a breakdown entry")
			assert.Equal(t, 2_000, got.PeakContextTokens)
			assert.False(t, got.HasCost,
				"a zero-token row does not cover stored context tokens")
		})
	}
}

func TestSessionUsageWithSubagentsDeductsStreamingSnapshotsOnce(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	parentID := "claude:streaming-parent"
	childID := "agent-streaming-child"
	parent := parentID
	// The stored total includes the partial 5-token snapshot, the complete
	// 500-token snapshot, and the 200 output-only tokens.
	seedSubagentSession(t, d, parentID, nil, "", 705)
	seedSubagentSession(t, d, childID, &parent, "subagent", 100)

	partial := usageMessage(
		parentID, 0, "10:00:00", "m-stream", 1000, 5)
	complete := usageMessage(
		parentID, 1, "10:01:00", "m-stream", 1000, 500)
	outputOnly := dbtest.AsstMsg(parentID, 2, "output-only")
	outputOnly.Timestamp = subagentUsageDay + "T10:02:00Z"
	outputOnly.Model = "test-opus"
	outputOnly.OutputTokens = 200
	outputOnly.HasOutputTokens = true
	dbtest.SeedMessages(t, d,
		partial,
		complete,
		outputOnly,
		usageMessage(childID, 0, "10:03:00", "m-child", 500, 100),
	)

	own, err := d.GetSessionUsage(ctx, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Equal(t, 700, own.TotalOutputTokens,
		"the own-session query removes the partial snapshot once")

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 800, got.TotalOutputTokens,
		"combined output keeps the complete snapshot, output-only tokens, "+
			"and child output without deducting the partial snapshot twice")
}

// TestSessionUsageWithSubagentsIsQueriedFromTheChildToo pins that a subagent
// queried directly still reports its own rows: the rollup is a parent-side
// view, not a relabeling of the archive.
func TestSessionUsageWithSubagentsIsQueriedFromTheChildToo(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	seedSubagentUsageFixture(t, d)

	got, err := service.SessionUsageWithSubagents(ctx, d, subagentChildBID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Zero(t, got.SubagentCount)
	assert.Equal(t, money.MustParseDollars(subagentChildBCost), got.Cost)
}

// TestSubagentRollupLeavesDayAggregatesUnchanged is the invariant that keeps
// the rollup presentation-only: GetDailyUsage already counts subagent
// sessions as first-class spend, so it must report the same deduplicated
// total as the combined session view and must not move when that view is
// computed.
func TestSubagentRollupLeavesDayAggregatesUnchanged(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()
	seedSubagentUsageFixture(t, d)

	filter := db.UsageFilter{
		From:     subagentUsageDay,
		To:       subagentUsageDay,
		Timezone: "UTC",
	}
	before, err := d.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars(subagentTotalCost),
		before.Totals.TotalCost,
		"the day total already includes subagent spend, deduplicated")
	assert.Equal(t, 3500, before.Totals.InputTokens)
	assert.Equal(t, subagentTotalOutput, before.Totals.OutputTokens)
	assert.Equal(t, 3, before.SessionCounts.Total,
		"subagent sessions stay first-class rows in the day aggregate")

	combined, err := service.SessionUsageWithSubagents(
		ctx, d, subagentParentID, true)
	require.NoError(t, err)
	require.NotNil(t, combined)

	after, err := d.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"combining usage for presentation must not persist or duplicate rows")
	assert.Equal(t, before.Totals.TotalCost, combined.Cost,
		"the parent's combined cost is the same money the day total counts")
	assert.Equal(t, before.Totals.OutputTokens, combined.TotalOutputTokens,
		"the two views must agree on output tokens, not just on cost")
}

// TestSessionUsageWithSubagentsReportsTokenDataForEmptyParent covers the exit
// code contract: a parent whose only usage lives in its subagents must look
// like it has data in the ordinary archive view.
func TestSessionUsageWithSubagentsReportsTokenDataForEmptyParent(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	started := subagentUsageDay + "T09:00:00Z"
	dbtest.SeedSession(t, d, "claude:empty-parent", "proj",
		func(s *db.Session) {
			s.Agent = "claude"
			s.StartedAt = &started
			s.EndedAt = &started
		})
	dbtest.SeedSession(t, d, "agent-only", "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.ParentSessionID = &[]string{"claude:empty-parent"}[0]
		s.RelationshipType = "subagent"
		s.TotalOutputTokens = 500
		s.HasTotalOutputTokens = true
	})
	dbtest.SeedMessages(t, d,
		usageMessage("agent-only", 0, "10:00:00", "m-9", 1000, 500))

	own, err := d.GetSessionUsage(ctx, "claude:empty-parent", true)
	require.NoError(t, err)
	require.NotNil(t, own)
	require.False(t, own.HasTokenData)
	require.False(t, own.HasCost)

	got, err := service.SessionUsageWithSubagents(
		ctx, d, "claude:empty-parent", true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.HasTokenData)
	assert.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars(subagentParentCost), got.Cost)
	assert.Equal(t, 500, got.TotalOutputTokens)
	assert.Equal(t, 1, got.SubagentCount)
}
