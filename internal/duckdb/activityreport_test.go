//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// duckDayQuery resolves a single-day "day" Query for date/tz against a
// fixed far-future now, so the candidate range is the full local day and
// the report is never partial regardless of the wall clock.
func duckDayQuery(t *testing.T, date, tz string) activity.Query {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "day", Date: date, Timezone: tz}, now)
	require.NoError(t, err)
	return q
}

// activityReportStore seeds the given writes into a fresh local SQLite DB,
// pushes them into DuckDB, and returns a read-only DuckDB store, mirroring
// newSyncedStore's sync path.
func activityReportStore(
	t *testing.T, writes []db.SessionBatchWrite, pricing []db.ModelPricing,
) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	if len(pricing) > 0 {
		require.NoError(t, local.UpsertModelPricing(pricing))
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB())
}

func TestActivityReportSourceProbeTracksIdentityRevision(t *testing.T) {
	ctx := context.Background()
	conn := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)

	before, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NoError(t, recordMetadataKey(
		ctx, conn, identityRevisionMetadataKey, "7",
	))
	after, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)

	assert.NotEqual(t, before, after,
		"identity-only mirror updates must change the Activity probe")
}

func TestActivityReportCandidateSourcePreservesRangeEdgePairs(t *testing.T) {
	const sessionID = "range-edge"
	timestamps := []string{
		"2026-06-13T23:54:59Z",
		"2026-06-13T23:59:30Z",
		"2026-06-14T00:00:30Z",
		"2026-06-14T23:59:00Z",
		"2026-06-15T00:20:00Z",
	}
	session := syncSession(sessionID, "edges", "edge", timestamps[0], len(timestamps))
	session.EndedAt = &timestamps[len(timestamps)-1]
	messages := make([]db.Message, 0, len(timestamps))
	for ordinal, timestamp := range timestamps {
		messages = append(messages, syncMessage(
			sessionID, ordinal, "user", "edge", timestamp,
		))
	}
	store := activityReportStore(t, []db.SessionBatchWrite{{
		Session: session, Messages: messages,
		DataVersion: 1, ReplaceMessages: true,
	}}, nil)
	q := duckDayQuery(t, "2026-06-14", "UTC")

	var starts []int
	err := store.ActivityReportCandidateSource(
		[]string{sessionID}, q,
	)(context.Background(), func(candidate activity.IntervalCandidate) error {
		starts = append(starts, candidate.StartOrdinal)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, starts,
		"left pruning and right successor lookup must preserve adjacency")
}

func TestDuckGetActivityReportBasicConcurrency(t *testing.T) {
	ctx := context.Background()
	// Two overlapping sessions on 2026-06-14 (UTC), each two timestamped
	// messages, mirroring the SQLite and PostgreSQL parity fixtures.
	aSession := syncSession("a", "proj1", "alpha first", "2026-06-14T10:00:00.000Z", 2)
	aSession.Agent = "claude"
	bSession := syncSession("b", "proj2", "beta first", "2026-06-14T10:01:00.000Z", 2)
	bSession.Agent = "codex"
	writes := []db.SessionBatchWrite{
		{
			Session: aSession,
			Messages: []db.Message{
				syncMessage("a", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
				syncMessage("a", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: bSession,
			Messages: []db.Message{
				syncMessage("b", 0, "user", "u", "2026-06-14T10:01:00.000Z"),
				syncMessage("b", 1, "assistant", "x", "2026-06-14T10:03:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 2, r.Peak.Agents)
	assert.Equal(t, 2, r.Totals.Sessions)
	assert.Equal(t, 2, r.SessionsTotal)
	assert.GreaterOrEqual(t, len(r.ByAgent), 2)
}

// TestDuckGetActivityReportIncludesSubagentUsage mirrors the SQLite
// TestGetActivityReport_IncludesSubagentUsage: subagent and fork sessions
// are candidates so their usage lands in the totals (matching daily
// usage, which never filters by relationship_type). The fork's replayed
// usage row dedups away, so it adds a session row but no cost.
func TestDuckGetActivityReportIncludesSubagentUsage(t *testing.T) {
	ctx := context.Background()
	root := syncSession("root", "proj1", "root first", "2026-06-14T10:00:00.000Z", 1)
	rootMsg := syncMessage("root", 0, "assistant", "x", "2026-06-14T10:00:00.000Z")
	rootMsg.Model = "root-model"
	rootMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	rootMsg.OutputTokens = 500
	rootMsg.ClaudeMessageID = "m-root"
	rootMsg.ClaudeRequestID = "r-root"

	parent := "root"
	sub := syncSession("agent-sub", "proj1", "sub first", "2026-06-14T10:02:00.000Z", 1)
	sub.RelationshipType = "subagent"
	sub.ParentSessionID = &parent
	subMsg := syncMessage("agent-sub", 0, "assistant", "y", "2026-06-14T10:03:00.000Z")
	subMsg.Model = "sub-model"
	subMsg.TokenUsage = json.RawMessage(`{"input_tokens":2000,"output_tokens":700}`)
	subMsg.OutputTokens = 700
	subMsg.ClaudeMessageID = "m-sub"
	subMsg.ClaudeRequestID = "r-sub"

	fork := syncSession("fork", "proj1", "fork first", "2026-06-14T10:05:00.000Z", 1)
	fork.RelationshipType = "fork"
	fork.ParentSessionID = &parent
	// The fork replays the root's message: same Claude ids, so the dedup
	// must drop its usage row while the session itself still appears.
	forkMsg := syncMessage("fork", 0, "assistant", "x", "2026-06-14T10:05:00.000Z")
	forkMsg.Model = "root-model"
	forkMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	forkMsg.OutputTokens = 500
	forkMsg.ClaudeMessageID = "m-root"
	forkMsg.ClaudeRequestID = "r-root"

	writes := []db.SessionBatchWrite{
		{Session: root, Messages: []db.Message{rootMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: sub, Messages: []db.Message{subMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: fork, Messages: []db.Message{forkMsg},
			DataVersion: 1, ReplaceMessages: true},
	}
	pricing := []db.ModelPricing{
		{ModelPattern: "root-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
		{ModelPattern: "sub-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "root")
	assert.Contains(t, ids, "agent-sub",
		"subagent session must be a candidate")
	assert.Contains(t, ids, "fork", "fork session must be a candidate")
	assert.Equal(t, 1200, r.Totals.OutputTokens,
		"totals include subagent usage; the fork's replayed row dedups away")
	// Cost = root (1000*3+500*15)/1e6 + subagent (2000*3+700*15)/1e6; the
	// fork's duplicate row contributes nothing.
	assert.Equal(t, money.MustParseDollars("0.027"), r.Totals.Cost)
}

func TestDuckGetActivityReportUsageCostAndTokens(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("s1", "proj1", "first", "2026-06-14T10:30:00.000Z", 1)
	sess.Agent = "claude"
	// Override the default token usage to a known input/output split so the
	// cost is deterministic.
	msg := syncMessage("s1", 0, "assistant", "x", "2026-06-14T10:30:00.000Z")
	msg.Model = "claude-sonnet-4-20250514"
	msg.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`)
	msg.OutputTokens = 500
	writes := []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{msg},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.Equal(t, money.MustParseDollars("0.0105"), r.Totals.Cost)
}

func TestDuckGetActivityReportPrefersCompleteClaudeSnapshot(t *testing.T) {
	ctx := context.Background()
	sess := syncSession(
		"streamed", "proj1", "first", "2026-06-14T10:30:00.000Z", 2)
	sess.Agent = "claude"
	partial := syncMessage(
		"streamed", 0, "assistant", "partial", "2026-06-14T10:30:00.000Z")
	partial.Model = "claude-sonnet-4-20250514"
	partial.ClaudeMessageID = "msg-stream"
	partial.ClaudeRequestID = "req-stream"
	partial.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":5}`)
	partial.OutputTokens = 5
	complete := syncMessage(
		"streamed", 1, "assistant", "complete", "2026-06-14T10:31:00.000Z")
	complete.Model = "claude-sonnet-4-20250514"
	complete.ClaudeMessageID = "msg-stream"
	complete.ClaudeRequestID = "req-stream"
	complete.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":631}`)
	complete.OutputTokens = 631
	store := activityReportStore(t, []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{partial, complete},
		DataVersion:     1,
		ReplaceMessages: true,
	}}, []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}})

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 631, r.Totals.OutputTokens)
}

func TestDuckGetActivityReportFiltersAfterCrossSessionSnapshotSelection(
	t *testing.T,
) {
	ctx := context.Background()
	parent := syncSession(
		"activity-parent", "parent-project", "parent",
		"2026-06-14T10:00:00.000Z", 1)
	parent.Agent = "claude"
	child := syncSession(
		"activity-child", "child-project", "child",
		"2026-06-14T10:01:00.000Z", 1)
	child.Agent = "claude"
	partial := syncMessage(
		"activity-parent", 0, "assistant", "partial",
		"2026-06-14T10:00:00.000Z")
	partial.Model = "partial-model"
	partial.TokenUsage = json.RawMessage(`{"input_tokens":10,"output_tokens":5}`)
	partial.OutputTokens = 5
	partial.ClaudeMessageID = "activity-message"
	partial.ClaudeRequestID = "activity-request"
	complete := syncMessage(
		"activity-child", 0, "assistant", "complete",
		"2026-06-14T10:01:00.000Z")
	complete.Model = "complete-model"
	complete.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":631}`)
	complete.OutputTokens = 631
	complete.ClaudeMessageID = "activity-message"
	complete.ClaudeRequestID = "activity-request"
	store := activityReportStore(t, []db.SessionBatchWrite{
		{Session: parent, Messages: []db.Message{partial},
			DataVersion: 1, ReplaceMessages: true},
		{Session: child, Messages: []db.Message{complete},
			DataVersion: 1, ReplaceMessages: true},
	}, nil)

	parentReport, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Project: "parent-project", Timezone: "UTC",
	}, duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, parentReport.Totals.Sessions)
	assert.Equal(t, 631, parentReport.Totals.OutputTokens,
		"the parent filter must retain the complete child snapshot")

	childReport, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Project: "child-project", Timezone: "UTC",
	}, duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, childReport.Totals.Sessions)
	assert.Zero(t, childReport.Totals.OutputTokens,
		"the child source must not claim usage attributed to the parent")
}

func TestDuckGetActivityReportSelectsPeersForLargeSnapshotKeySet(t *testing.T) {
	const pairCount = duckMaxSQLVars + 1
	ctx := context.Background()
	candidate := syncSession(
		"large-candidate", "included-project", "candidate",
		"2026-06-14T10:00:00.000Z", pairCount)
	peer := syncSession(
		"large-peer", "excluded-project", "peer",
		"2026-06-14T10:01:00.000Z", pairCount)
	candidateMessages := make([]db.Message, pairCount)
	peerMessages := make([]db.Message, pairCount)
	for i := range pairCount {
		messageID := fmt.Sprintf("large-message-%04d", i)
		requestID := fmt.Sprintf("large-request-%04d", i)
		candidateMessages[i] = syncMessage(
			candidate.ID, i, "assistant", "partial",
			"2026-06-14T10:00:00.000Z")
		candidateMessages[i].ClaudeMessageID = messageID
		candidateMessages[i].ClaudeRequestID = requestID
		candidateMessages[i].TokenUsage = json.RawMessage(
			`{"input_tokens":1,"output_tokens":1}`)
		candidateMessages[i].OutputTokens = 1
		peerMessages[i] = syncMessage(
			peer.ID, i, "assistant", "complete",
			"2026-06-14T10:01:00.000Z")
		peerMessages[i].ClaudeMessageID = messageID
		peerMessages[i].ClaudeRequestID = requestID
		peerMessages[i].TokenUsage = json.RawMessage(
			`{"input_tokens":1,"output_tokens":10}`)
		peerMessages[i].OutputTokens = 10
	}
	store := activityReportStore(t, []db.SessionBatchWrite{
		{Session: candidate, Messages: candidateMessages,
			DataVersion: 1, ReplaceMessages: true},
		{Session: peer, Messages: peerMessages,
			DataVersion: 1, ReplaceMessages: true},
	}, nil)

	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Project: "included-project", Timezone: "UTC",
	}, duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, pairCount*10, report.Totals.OutputTokens,
		"every complete peer snapshot must remain attributed to the candidate")
}

func TestDuckGetActivityReportDeduplicatesAfterProjectFilter(t *testing.T) {
	ctx := context.Background()
	excluded := syncSession(
		"excluded-earlier", "excluded-project", "excluded",
		"2026-06-14T10:00:00Z", 1)
	excluded.Agent = "claude"
	included := syncSession(
		"included-later", "included-project", "included",
		"2026-06-14T10:01:00Z", 1)
	included.Agent = "claude"
	excludedMsg := syncMessage(
		"excluded-earlier", 0, "assistant", "excluded",
		"2026-06-14T10:00:00Z")
	excludedMsg.Model = "model-x"
	excludedMsg.TokenUsage = json.RawMessage(
		`{"input_tokens":10,"output_tokens":5}`)
	excludedMsg.OutputTokens = 5
	excludedMsg.SourceUUID = "shared-source"
	includedMsg := syncMessage(
		"included-later", 0, "assistant", "included",
		"2026-06-14T10:01:00Z")
	includedMsg.Model = "model-x"
	includedMsg.TokenUsage = json.RawMessage(
		`{"input_tokens":20,"output_tokens":631}`)
	includedMsg.OutputTokens = 631
	includedMsg.SourceUUID = "shared-source"
	store := activityReportStore(t, []db.SessionBatchWrite{
		{Session: excluded, Messages: []db.Message{excludedMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: included, Messages: []db.Message{includedMsg},
			DataVersion: 1, ReplaceMessages: true},
	}, nil)

	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Project: "included-project", Timezone: "UTC",
	}, duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 631, report.Totals.OutputTokens,
		"an excluded duplicate must not suppress included usage")
}

func TestDuckActivityReportRowStatusCanonicalizesKimiAliasByTimestamp(t *testing.T) {
	tests := []struct {
		name         string
		timestamp    string
		canonical    string
		expectedCost money.Money
	}{
		{
			name:         "before cutoff",
			timestamp:    "2026-07-18T23:59:59Z",
			canonical:    pricingpkg.KimiK26Canonical,
			expectedCost: money.MustParseDollars("1"),
		},
		{
			name:         "at cutoff",
			timestamp:    "2026-07-19T00:00:00Z",
			canonical:    pricingpkg.KimiK3Canonical,
			expectedCost: money.MustParseDollars("2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := export.NewPricingResolver([]export.EffectivePricingRow{
				{
					ModelPattern: pricingpkg.KimiK26Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("1"),
					},
				},
				{
					ModelPattern: pricingpkg.KimiK3Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("2"),
					},
				},
			})

			_, cost, priced, contributes, err := duckActivityReportRowStatus(
				duckActivityReportUsageRow{
					model:    "daimon-kimi-code",
					ts:       tt.timestamp,
					inputTok: 1_000_000,
				},
				resolver,
			)

			require.NoError(t, err)
			assert.True(t, priced)
			assert.True(t, contributes)
			assert.Equal(t, tt.expectedCost, cost)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)
			require.Contains(t, block.Models, "daimon-kimi-code")
			resolutions := block.Models["daimon-kimi-code"].Resolutions
			require.Len(t, resolutions, 1)
			assert.Equal(t, tt.canonical, resolutions[0].PricedModel)
			assert.NotContains(t, block.Models, tt.canonical)
		})
	}
}

func TestDuckActivityReportRowStatusPrefersExactCustomKimiAlias(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "daimon-kimi-code",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	_, cost, priced, contributes, err := duckActivityReportRowStatus(
		duckActivityReportUsageRow{
			model:    "daimon-kimi-code",
			ts:       "2026-07-19T00:00:00Z",
			inputTok: 1_000_000,
		},
		resolver,
	)

	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "daimon-kimi-code")
	resolutions := block.Models["daimon-kimi-code"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "daimon-kimi-code", resolutions[0].PricedModel)
}

func TestDuckGetActivityReportPricingBandApplicationCountedOnce(t *testing.T) {
	ctx := context.Background()
	sess := syncSession(
		"pricing-band", "proj1", "banded", "2026-06-14T10:30:00.000Z", 1)
	msg := syncMessage(
		sess.ID, 0, "assistant", "request", "2026-06-14T10:30:00.000Z")
	msg.Model = "banded-model"
	msg.TokenUsage = json.RawMessage(`{"input_tokens":300000}`)
	store := activityReportStore(t, []db.SessionBatchWrite{{
		Session: sess, Messages: []db.Message{msg},
		DataVersion: 1, ReplaceMessages: true,
	}}, []db.ModelPricing{{
		ModelPattern: "banded-model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []db.PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}},
	}})

	report, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 600_000}, report.Totals.Cost)
	require.NotNil(t, report.Pricing)
	provenance := report.Pricing.Models["banded-model"]
	require.Len(t, provenance.Resolutions, 1)
	assert.Equal(t, export.PricingApplication{
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, provenance.Resolutions[0].Application)
}

func TestDuckGetActivityReportPricesGooseRequestAsRequestScoped(t *testing.T) {
	ctx := context.Background()
	sess := syncSession(
		"pricing-band", "proj1", "banded", "2026-06-14T10:30:00.000Z", 1)
	msg := syncMessage(
		sess.ID, 0, "user", "request", "2026-06-14T10:30:00.000Z")
	store := activityReportStore(t, []db.SessionBatchWrite{{
		Session: sess, Messages: []db.Message{msg}, UsageEvents: []db.UsageEvent{{
			Source: "goose-request", Model: "banded-model", InputTokens: 300_000,
			OccurredAt: "2026-06-14T10:30:30.000Z", DedupKey: "goose-request",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}}, []db.ModelPricing{{
		ModelPattern: "banded-model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []db.PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}},
	}})

	report, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 600_000}, report.Totals.Cost)
	require.NotNil(t, report.Pricing)
	provenance := report.Pricing.Models["banded-model"]
	require.Len(t, provenance.Resolutions, 1)
	assert.Equal(t, export.PricingApplication{
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, provenance.Resolutions[0].Application)
}

func TestDuckGetActivityReportCopilotReportedCostReplacesSessionEstimates(t *testing.T) {
	ctx := context.Background()
	reportedCost := money.MustParseDollars("0.03")
	sess := syncSession(
		"copilot:activity-authoritative", "proj1", "copilot activity",
		"2026-06-14T10:00:00.000Z", 1,
	)
	sess.Agent = "copilot"
	store := activityReportStore(t, []db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{
			{
				Source: "shutdown", Model: "copilot-model-a",
				InputTokens: 1_000_000,
				OccurredAt:  "2026-06-14T10:05:00.000Z", DedupKey: "first",
			},
			{
				Source: "shutdown", Model: "copilot-model-b",
				InputTokens: 1_000_000,
				Cost:        &reportedCost, CostStatus: "exact",
				CostSource: db.CopilotReportedCostSource,
				OccurredAt: "2026-06-14T10:10:00.000Z", DedupKey: "final",
			},
		},
		DataVersion: 1, ReplaceMessages: true,
	}}, []db.ModelPricing{
		{ModelPattern: "copilot-model-a", InputPerMTok: money.MustParseDollars("10")},
		{ModelPattern: "copilot-model-b", InputPerMTok: money.MustParseDollars("20")},
	})

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, reportedCost, r.Totals.Cost)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, reportedCost, r.BySession[0].Cost)
	modelCosts := make(map[string]money.Money, len(r.ByModel))
	for _, model := range r.ByModel {
		modelCosts[model.Key] = model.Cost
	}
	assert.Equal(t, money.MustParseDollars("0.01"), modelCosts["copilot-model-a"])
	assert.Equal(t, money.MustParseDollars("0.02"), modelCosts["copilot-model-b"])
	assert.Equal(t, r.Totals.Cost,
		money.MustAdd(modelCosts["copilot-model-a"], modelCosts["copilot-model-b"]))
}

func TestDuckGetActivityReportPricingModelsOnlyIncludeDedupSurvivors(t *testing.T) {
	ctx := context.Background()
	earlier := syncSession("earlier", "proj1", "first", "2026-06-14T10:30:00.000Z", 1)
	earlier.Agent = "claude"
	earlierMsg := syncMessage("earlier", 0, "assistant", "x", "2026-06-14T10:30:00.000Z")
	earlierMsg.Model = "partial-model"
	earlierMsg.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`)
	earlierMsg.OutputTokens = 500
	earlierMsg.ClaudeMessageID = "m-dup"
	earlierMsg.ClaudeRequestID = "r-dup"

	later := syncSession("later", "proj1", "first", "2026-06-14T10:31:00.000Z", 1)
	later.Agent = "claude"
	laterMsg := syncMessage("later", 0, "assistant", "x", "2026-06-14T10:31:00.000Z")
	laterMsg.Model = "complete-model"
	laterMsg.TokenUsage = json.RawMessage(
		`{"input_tokens":2000,"output_tokens":900}`)
	laterMsg.OutputTokens = 900
	laterMsg.ClaudeMessageID = "m-dup"
	laterMsg.ClaudeRequestID = "r-dup"

	writes := []db.SessionBatchWrite{
		{
			Session:         earlier,
			Messages:        []db.Message{earlierMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         later,
			Messages:        []db.Message{laterMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	pricing := []db.ModelPricing{
		{ModelPattern: "partial-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
		{ModelPattern: "complete-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 900, r.Totals.OutputTokens)
	require.NotNil(t, r.Pricing)
	assert.Contains(t, r.Pricing.Models, "complete-model")
	assert.NotContains(t, r.Pricing.Models, "partial-model")
}

func TestDuckGetActivityReportPreservesSessionSummaryUsageEventTokens(t *testing.T) {
	ctx := context.Background()
	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	sessionID := "summary-activity"
	sess := syncSession(sessionID, "proj1", "first", "2026-06-14T10:30:00.000Z", 1)
	sess.Agent = "hermes"
	sess.TotalOutputTokens = rawOutput
	sess.PeakContextTokens = rawInput
	sess.HasTotalOutputTokens = true
	sess.HasPeakContextTokens = true
	msg := syncMessage(sessionID, 0, "user", "first", "2026-06-14T10:30:00.000Z")
	msg.Model = ""
	msg.TokenUsage = nil
	writes := []db.SessionBatchWrite{{
		Session:  sess,
		Messages: []db.Message{msg},
		UsageEvents: []db.UsageEvent{{
			Source:       "session",
			Model:        "summary-model",
			InputTokens:  rawInput,
			OutputTokens: rawOutput,
			OccurredAt:   "2026-06-14T10:30:00.000Z",
			DedupKey:     "summary",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "summary-model",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("2"),
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, rawOutput, r.Totals.OutputTokens)
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(t, err)
	assert.Equal(t, wantCost, r.Totals.Cost)
}

// TestDuckGetActivityReportExcludesIneligibleUsage confirms the DuckDB
// usage union (the one backend that inlines its own usage CTE rather
// than sharing dailyUsageRowsSQLWithWhere) applies the same eligibility
// filters as GetDailyUsage: a synthetic-model message carrying real
// token_usage must not inflate the day totals. Mirrors the PostgreSQL
// TestPGGetActivityReportExcludesIneligibleUsage.
func TestDuckGetActivityReportExcludesIneligibleUsage(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("s1", "proj1", "first", "2026-06-14T10:30:00.000Z", 2)
	sess.Agent = "claude"
	end := "2026-06-14T10:31:00.000Z"
	sess.EndedAt = &end

	eligible := syncMessage("s1", 0, "assistant", "x", "2026-06-14T10:30:00.000Z")
	eligible.Model = "claude-sonnet-4-20250514"
	eligible.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`)
	eligible.OutputTokens = 500
	// Ineligible: a synthetic-model message carrying real token_usage. The
	// usage CTE drops m.model == '<synthetic>', so these tokens must NOT leak
	// into the day totals even though the blob is non-empty.
	synthetic := syncMessage("s1", 1, "assistant", "y", "2026-06-14T10:31:00.000Z")
	synthetic.Model = "<synthetic>"
	synthetic.TokenUsage = json.RawMessage(
		`{"input_tokens":9000,"output_tokens":7000}`)
	synthetic.OutputTokens = 7000

	writes := []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{eligible, synthetic},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens, "synthetic message excluded")
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.Equal(t, money.MustParseDollars("0.0105"), r.Totals.Cost)
}

// TestDuckGetActivityReportPriorDayWithinPadExcluded confirms the candidate
// window uses the EXACT local day, not the +/-14h padded bounds: a
// session that began and ended on the prior day but lands inside the pad
// must NOT appear as an untimed session in the target day's report.
func TestDuckGetActivityReportPriorDayWithinPadExcluded(t *testing.T) {
	ctx := context.Background()
	today := syncSession("today", "proj1", "today first", "2026-06-14T10:00:00.000Z", 2)
	today.Agent = "claude"
	prior := syncSession("prior", "proj2", "prior first", "2026-06-13T12:00:00.000Z", 1)
	prior.Agent = "codex"
	priorEnd := "2026-06-13T12:05:00.000Z"
	prior.EndedAt = &priorEnd
	writes := []db.SessionBatchWrite{
		{
			Session: today,
			Messages: []db.Message{
				syncMessage("today", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
				syncMessage("today", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: prior,
			Messages: []db.Message{
				syncMessage("prior", 0, "user", "u", "2026-06-13T12:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "today")
	assert.NotContains(t, ids, "prior", "prior-day session must not leak in")
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 0, r.Totals.UntimedSessions)
}

// TestDuckGetActivityReportOpenSessionWithInRangeMessageIncluded confirms a
// still-open session (no ended_at) that started before the range but has a
// message inside it is not dropped. The effective-end fallback uses the
// session's latest message timestamp, not started_at, matching SQLite and
// PostgreSQL. Mirrors the SQLite
// TestGetActivityReport_OpenSessionWithInRangeMessageIncluded.
func TestDuckGetActivityReportOpenSessionWithInRangeMessageIncluded(t *testing.T) {
	ctx := context.Background()
	// Started the day before and never closed (ended_at nil), active in-range.
	open := syncSession("open", "proj1", "open first", "2026-06-13T23:00:00.000Z", 2)
	open.Agent = "claude"
	open.EndedAt = nil
	writes := []db.SessionBatchWrite{{
		Session: open,
		Messages: []db.Message{
			syncMessage("open", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
			syncMessage("open", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "open",
		"open session active in-range must not be dropped by the started_at fallback")
	assert.Equal(t, 1, r.Totals.Sessions)
}

// TestDuckGetActivityReportUsageDedupSubSecondOrder confirms DuckDB orders the
// usage stream by the parsed instant so first-seen-wins fallback dedup keeps
// the chronologically earlier row when two rows share a source UUID in the same
// second -- one whole-second ("...00Z"), one fractional ("...00.123Z"). DuckDB
// already sorts on the parsed time (not the formatted text), so this locks in
// that cross-backend behavior, matching the SQLite
// TestGetActivityReport_UsageDedupSubSecondOrder.
func TestDuckGetActivityReportUsageDedupSubSecondOrder(t *testing.T) {
	ctx := context.Background()
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}

	// A resumed/forked pair shares one source UUID fallback dedup identity
	// across two sessions: the earlier whole-second instant carries 500 output
	// tokens, the later fractional instant 9000. Dedup must keep the 500 row.
	earlier := syncSession("earlier", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	earlierMsg := syncMessage("earlier", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	earlierMsg.Model = "claude-sonnet-4-20250514"
	earlierMsg.ClaudeMessageID = "dup-m"
	earlierMsg.SourceUUID = "dup-source"
	earlierMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	earlierMsg.OutputTokens = 500

	later := syncSession("later", "proj2", "first", "2026-06-14T10:30:00.123Z", 1)
	laterMsg := syncMessage("later", 0, "assistant", "x", "2026-06-14T10:30:00.123Z")
	laterMsg.Model = "claude-sonnet-4-20250514"
	laterMsg.ClaudeMessageID = "dup-m"
	laterMsg.SourceUUID = "dup-source"
	laterMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":9000}`)
	laterMsg.OutputTokens = 9000

	writes := []db.SessionBatchWrite{
		{Session: earlier, Messages: []db.Message{earlierMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: later, Messages: []db.Message{laterMsg},
			DataVersion: 1, ReplaceMessages: true},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"first-seen dedup keeps the chronologically earlier whole-second row")
}

func TestDuckGetActivityReportUsageDedupFallsBackToSourceUUID(t *testing.T) {
	ctx := context.Background()
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}

	earlier := syncSession("earlier", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	earlier.Agent = "claude"
	earlierMsg := syncMessage("earlier", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	earlierMsg.Model = "claude-sonnet-4-20250514"
	earlierMsg.ClaudeMessageID = "dup-m"
	earlierMsg.SourceUUID = "src-dup"
	earlierMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	earlierMsg.OutputTokens = 500

	later := syncSession("later", "proj2", "first", "2026-06-14T10:30:01Z", 1)
	later.Agent = "claude"
	laterMsg := syncMessage("later", 0, "assistant", "x", "2026-06-14T10:30:01Z")
	laterMsg.Model = "claude-sonnet-4-20250514"
	laterMsg.ClaudeMessageID = "dup-m"
	laterMsg.SourceUUID = "src-dup"
	laterMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":900}`)
	laterMsg.OutputTokens = 900

	writes := []db.SessionBatchWrite{
		{Session: earlier, Messages: []db.Message{earlierMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: later, Messages: []db.Message{laterMsg},
			DataVersion: 1, ReplaceMessages: true},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"incomplete Claude pairs fall back to source_uuid dedup in activity reports")
}

// TestDuckGetActivityReportZeroCostKeepsPrimaryModel confirms a usage-only
// (untimed) session whose known-model usage carries zero cost still reports
// that model as primary through the DuckDB path, guarding the shared zero-cost
// fallback end-to-end. Mirrors the aggregator unit test
// TestAggregate_UsageOnlySessionZeroCostKeepsPrimaryModel.
func TestDuckGetActivityReportZeroCostKeepsPrimaryModel(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("u", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	msg := syncMessage("u", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	// Known model, unpriced and zero tokens -> a usage row with zero cost.
	msg.Model = "model-x"
	msg.TokenUsage = json.RawMessage(`{"input_tokens":0,"output_tokens":0}`)
	msg.OutputTokens = 0
	writes := []db.SessionBatchWrite{{
		Session: sess, Messages: []db.Message{msg},
		DataVersion: 1, ReplaceMessages: true,
	}}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, "model-x", r.BySession[0].PrimaryModel,
		"zero-cost usage must still report its known model as primary")
}

// TestDuckGetActivityReportAutomationFilterAndSessionSplit confirms the shared
// AnalyticsFilter automation class is honored through the DuckDB analytics
// WHERE builder and that the Totals session-count split survives the sync into
// DuckDB. Mirrors the SQLite
// TestGetActivityReport_AutomationFilterAndSessionSplit.
func TestDuckGetActivityReportAutomationFilterAndSessionSplit(t *testing.T) {
	ctx := context.Background()

	// The sync path (WriteSessionBatchAtomic) classifies is_automated from the
	// transcript: a single-turn session whose first user message matches a
	// known automated (roborev) prompt prefix. Setting the struct flag alone
	// would be overridden by updateSessionAutomationFromMessagesTx, so the
	// automated sessions carry an automated first user message and a single
	// user turn, exactly as a real roborev review session does.
	auto1 := syncSession("auto1", "proj1", "You are a code reviewer.", "2026-06-14T10:00:00.000Z", 2)
	auto1.Agent = "claude"
	auto2 := syncSession("auto2", "proj1", "You are a code reviewer.", "2026-06-14T11:00:00.000Z", 2)
	auto2.Agent = "claude"
	human := syncSession("human", "proj2", "human first", "2026-06-14T12:00:00.000Z", 2)
	human.Agent = "codex"

	writes := []db.SessionBatchWrite{
		{
			Session: auto1,
			Messages: []db.Message{
				syncMessage("auto1", 0, "user", "You are a code reviewer.", "2026-06-14T10:00:00.000Z"),
				syncMessage("auto1", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: auto2,
			Messages: []db.Message{
				syncMessage("auto2", 0, "user", "You are a code reviewer.", "2026-06-14T11:00:00.000Z"),
				syncMessage("auto2", 1, "assistant", "x", "2026-06-14T11:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: human,
			Messages: []db.Message{
				syncMessage("human", 0, "user", "u", "2026-06-14T12:00:00.000Z"),
				syncMessage("human", 1, "assistant", "x", "2026-06-14T12:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

	tests := []struct {
		name            string
		filter          db.AnalyticsFilter
		wantAutomated   int
		wantInteractive int
		wantIDs         []string
	}{
		{
			name:            "all keeps both classes",
			filter:          db.AnalyticsFilter{Timezone: "UTC"},
			wantAutomated:   2,
			wantInteractive: 1,
			wantIDs:         []string{"auto1", "auto2", "human"},
		},
		{
			name:            "exclude automated keeps interactive only",
			filter:          db.AnalyticsFilter{Timezone: "UTC", ExcludeAutomated: true},
			wantAutomated:   0,
			wantInteractive: 1,
			wantIDs:         []string{"human"},
		},
		{
			name:            "exclude interactive keeps automated only",
			filter:          db.AnalyticsFilter{Timezone: "UTC", ExcludeInteractive: true},
			wantAutomated:   2,
			wantInteractive: 0,
			wantIDs:         []string{"auto1", "auto2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := store.GetActivityReport(ctx, tc.filter,
				duckDayQuery(t, "2026-06-14", "UTC"))
			require.NoError(t, err)
			assert.Equal(t, len(tc.wantIDs), r.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, r.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, r.Totals.InteractiveSessions)
			ids := make(map[string]struct{}, len(r.BySession))
			for _, s := range r.BySession {
				ids[s.SessionID] = struct{}{}
			}
			require.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}
