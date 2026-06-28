package db

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
)

var (
	dailyUsageFixtureOnce sync.Once
	dailyUsageFixtureDir  string
	dailyUsageFixturePath string
)

func TestDailyUsageResultOmitsUnsetPricingMetadata(t *testing.T) {
	b, err := json.Marshal(DailyUsageResult{})
	require.NoError(t, err)

	assert.NotContains(t, string(b), `"pricing"`)
}

func TestDailyUsageResultEmitsEmptyProjectsMap(t *testing.T) {
	b, err := json.Marshal(DailyUsageResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Projects:      map[string]export.ProjectMapEntry{},
		Daily:         []DailyUsageEntry{},
	})
	require.NoError(t, err)

	assert.Contains(t, string(b), `"projects":{}`)
}

func TestUsageDailyEmptyProjectsMapExcludesUnrelatedObservations(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedProjectIdentityObservation(t, d, "unrelated-project")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-06-01", To: "2026-06-01", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Empty(t, result.Daily)
	assert.Empty(t, result.Projects)
}

func openDailyUsageFixtureDB(t *testing.T) *DB {
	t.Helper()

	dailyUsageFixtureOnce.Do(func() {
		dailyUsageFixtureDir, dailyUsageFixturePath =
			buildDailyUsageFixtureTemplate(t)
	})

	dst := filepath.Join(t.TempDir(), "test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		require.NoError(t,
			copyTemplateDBFile(
				dailyUsageFixturePath+suffix, dst+suffix, suffix == "",
			),
			"copy daily usage fixture %q", suffix)
	}
	d, err := OpenPreparedTestDB(dst)
	require.NoError(t, err, "open daily usage fixture")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

func buildDailyUsageFixtureTemplate(t *testing.T) (string, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "agentsview-daily-usage-*")
	require.NoError(t, err, "create daily usage fixture dir")
	path := filepath.Join(dir, "test.db")
	require.NoError(t, copyTestDBTemplate(path),
		"copy base db template for daily usage fixture")

	d, err := OpenPreparedTestDB(path)
	require.NoError(t, err, "open daily usage template")
	seedDailyUsageFixture(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, d.CheckpointWALTruncate(ctx),
		"checkpoint daily usage template")
	require.NoError(t, d.Close(), "close daily usage template")
	return dir, path
}

func seedDailyUsageFixture(t *testing.T, d *DB) {
	t.Helper()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", InputPerMTok: 2.0,
			OutputPerMTok: 10.0},
		{ModelPattern: "gpt-5", InputPerMTok: 2.5,
			OutputPerMTok: 10.0},
	}), "UpsertModelPricing")

	type combo struct {
		project string
		agent   string
	}
	combos := []combo{
		{"proj-a", "claude"},
		{"proj-a", "codex"},
		{"proj-b", "claude"},
		{"proj-b", "codex"},
	}
	for i, c := range combos {
		sid := "usage-fixture-" + strconv.Itoa(i)
		insertSession(t, d, sid, c.project, func(s *Session) {
			s.Agent = c.agent
			s.StartedAt = Ptr("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d,
			Message{
				SessionID:  sid,
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:30:00Z",
				Model:      "model-a",
				TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
			},
			Message{
				SessionID:  sid,
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:31:00Z",
				Model:      "gpt-5",
				TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
			},
		)
	}

	insertSession(t, d, "usage-fixture-no-price", "proj-unknown", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-07-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "usage-fixture-no-price",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-07-15T10:30:00Z",
		Model:      "unknown-model",
		TokenUsage: json.RawMessage(`{"input_tokens":500,"output_tokens":250}`),
	})
}

func TestGetDailyUsageEmpty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-01-01",
		To:   "2024-12-31",
	})
	requireNoError(t, err, "GetDailyUsage empty")

	require.NotNil(t, result.Daily, "Daily should be non-nil empty slice")
	assert.Len(t, result.Daily, 0, "got")
	assert.Equal(t, 0.0, result.Totals.TotalCost, "TotalCost")
}

func TestUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	query, args := usageRowQuery(UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "and u.ts >=")
	assert.NotContains(t, normalized, "and u.ts <=")
	assert.NotContains(t, normalized, " or ")
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "m.timestamp != ''")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "nullif(m.timestamp, '') is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized, "s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	assert.Contains(t, normalized, "s.started_at <= ?")
	require.Len(t, args, 8)
	assert.Equal(t, "2024-05-31T10:00:00Z", args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[1])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[2])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[3])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[4])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[5])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[6])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[7])
}

func TestTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	query, args := topSessionsUsageRowQuery(UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "m.timestamp != ''")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "nullif(m.timestamp, '') is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized,
		"nullif(m.timestamp, '') is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized,
		"ue.occurred_at is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	require.Len(t, args, 8)
	assert.Equal(t, "2024-05-31T10:00:00Z", args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[1])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[2])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[3])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[4])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[5])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[6])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[7])
}

func TestUsageEventsReplaceAndList(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "hermes:event", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})

	cost := 0.02
	ordinal := 3
	events := []UsageEvent{{
		SessionID:                "hermes:event",
		MessageOrdinal:           &ordinal,
		Source:                   "session",
		Model:                    "gpt-5.4",
		InputTokens:              100,
		OutputTokens:             50,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     11,
		ReasoningTokens:          13,
		CostUSD:                  &cost,
		CostStatus:               "estimated",
		CostSource:               "hermes",
		OccurredAt:               "2026-05-14T10:05:00Z",
		DedupKey:                 "session:hermes:event",
	}}
	err := d.ReplaceSessionUsageEvents("hermes:event", events)
	require.NoError(t, err, "ReplaceSessionUsageEvents")

	got, err := d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, got, 1, "len")
	require.Equal(t, 100, got[0].InputTokens,
		"InputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 50, got[0].OutputTokens,
		"OutputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 7, got[0].CacheCreationInputTokens,
		"CacheCreationInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 11, got[0].CacheReadInputTokens,
		"CacheReadInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 13, got[0].ReasoningTokens,
		"ReasoningTokens (token fields not round-tripped: %#v)", got[0])
	require.NotNil(t, got[0].MessageOrdinal, "MessageOrdinal want 3")
	require.Equal(t, 3, *got[0].MessageOrdinal, "MessageOrdinal")
	require.NotNil(t, got[0].CostUSD, "CostUSD want %v", cost)
	require.Equal(t, cost, *got[0].CostUSD, "CostUSD")
	require.Equal(t, "session:hermes:event", got[0].DedupKey, "DedupKey")
	fps, err := d.UsageEventFingerprints([]string{"hermes:event", "missing"})
	require.NoError(t, err, "UsageEventFingerprints")
	require.NotEmpty(t, fps["hermes:event"],
		"expected non-empty usage event fingerprint")
	require.Equal(t, "", fps["missing"], "missing fingerprint")

	err = d.ReplaceSessionUsageEvents("hermes:event", nil)
	require.NoError(t, err, "ReplaceSessionUsageEvents clear")
	got, err = d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents after clear")
	require.Len(t, got, 0, "usage events after clear =")
}

func TestUsageEventsReplaceRejectsDuplicateDedupKeysAndRollsBack(t *testing.T) {
	// A parser emitting two events with the same dedup key (e.g. Grok
	// retry/replay lines sharing a prompt_id, before the parser-side
	// last-wins dedupe) violates idx_usage_events_dedup and must fail
	// the whole replace, leaving the prior events untouched. Parsers
	// therefore dedupe in memory before handing events to the DB.
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "grok:dup", "proj", func(s *Session) {
		s.Agent = "grok"
	})

	prior := []UsageEvent{{
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 1,
		DedupKey:    "session:grok:dup:p-1:grok-4.5",
	}}
	require.NoError(t, d.ReplaceSessionUsageEvents("grok:dup", prior))

	dup := []UsageEvent{{
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 2,
		DedupKey:    "session:grok:dup:p-2:grok-4.5",
	}, {
		SessionID:   "grok:dup",
		Source:      "session",
		Model:       "grok-4.5",
		InputTokens: 3,
		DedupKey:    "session:grok:dup:p-2:grok-4.5",
	}}
	err := d.ReplaceSessionUsageEvents("grok:dup", dup)
	require.Error(t, err, "duplicate dedup keys must violate idx_usage_events_dedup")

	got, err := d.GetUsageEvents(ctx, "grok:dup")
	require.NoError(t, err, "GetUsageEvents after failed replace")
	require.Len(t, got, 1, "failed replace must roll back to the prior events")
	assert.Equal(t, 1, got[0].InputTokens, "prior event must survive the rollback")
}

func TestGetDailyUsageWithData(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}})
	requireNoError(t, err, "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T11:00:00Z")
	})

	tokenUsage := `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"cache_creation_input_tokens": 200,
		"cache_read_input_tokens": 300
	}`
	insertMessages(t, d, Message{
		SessionID:  "sess1",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(tokenUsage),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, "2024-06-15", day.Date, "Date")
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 200, day.CacheCreationTokens, "CacheCreationTokens")
	assert.Equal(t, 300, day.CacheReadTokens, "CacheReadTokens")

	// Cost = (1000*3.0 + 500*15.0 + 200*3.75 + 300*0.30) / 1_000_000
	//      = (3000 + 7500 + 750 + 90) / 1_000_000
	//      = 11340 / 1_000_000
	//      = 0.01134
	wantCost := 0.01134
	assert.InDelta(t, wantCost, day.TotalCost, 1e-9, "TotalCost")

	assert.Equal(t, []string{"claude-sonnet-4-20250514"},
		day.ModelsUsed, "ModelsUsed")

	// Totals should match single day
	assert.Equal(t, 1000, result.Totals.InputTokens, "Totals.InputTokens")
	assert.InDelta(t, wantCost, result.Totals.TotalCost, 1e-9,
		"Totals.TotalCost")
}

// TestGetDailyUsageForkModelPricing verifies that custom model
// patterns added via UpsertModelPricing (e.g. for internal/private
// models that are not in the upstream LiteLLM catalog) are picked
// up by GetDailyUsage and produce non-zero cost. This guards
// against regressions where a fork or downstream adds bespoke
// models but the daily usage board silently shows $0.00 because
// the model does not canonicalize to a known catalog key.
func TestGetDailyUsageForkModelPricing(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "internal-private-model",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.5,
	}})
	requireNoError(t, err, "UpsertModelPricing")

	insertSession(t, d, "fork-sess", "fork-proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-06-25T10:00:00Z")
		s.EndedAt = new("2026-06-25T11:00:00Z")
	})

	tokenUsage := `{
		"input_tokens": 1000000,
		"output_tokens": 500000,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens": 0
	}`
	insertMessages(t, d, Message{
		SessionID:  "fork-sess",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2026-06-25T10:30:00Z",
		Model:      "internal-private-model",
		TokenUsage: json.RawMessage(tokenUsage),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-06-20",
		To:   "2026-06-28",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1,
		"daily should have one entry; got 0 means fork model "+
			"pricing is being silently dropped")

	day := result.Daily[0]
	assert.Equal(t, "2026-06-25", day.Date, "Date")
	assert.Equal(t, 1000000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500000, day.OutputTokens, "OutputTokens")

	// Cost = (1_000_000 * 5.0 + 500_000 * 25.0) / 1_000_000
	//      = (5_000_000 + 12_500_000) / 1_000_000
	//      = 17.5
	assert.InDelta(t, 17.5, day.TotalCost, 1e-9, "TotalCost")
	assert.InDelta(t, 17.5, result.Totals.TotalCost, 1e-9,
		"Totals.TotalCost")
}

// TestGetDailyUsageUnknownModelHasZeroCostButCountsTokens asserts
// the failing case the fix guards against: a model that is not
// in the pricing catalog must NOT silently make days[] empty.
// The tokens still count toward Daily/Totals, but cost is $0
// for the unrecognised model. If this test ever fails with
// len(result.Daily)==0 the upstream time-window SQL has
// regressed and the bug from upstream issue #904 has crept
// back in.
func TestGetDailyUsageUnknownModelHasZeroCostButCountsTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Deliberately do NOT register pricing for "totally-unknown".
	insertSession(t, d, "unk-sess", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-06-25T10:00:00Z")
	})
	tokenUsage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d, Message{
		SessionID:  "unk-sess",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2026-06-25T10:30:00Z",
		Model:      "totally-unknown",
		TokenUsage: json.RawMessage(tokenUsage),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-06-25",
		To:   "2026-06-25",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1,
		"daily entry must exist even when model is unpriced")
	assert.Equal(t, 0.0, result.Daily[0].TotalCost, "cost is zero")
	assert.Equal(t, 1000, result.Daily[0].InputTokens,
		"input tokens still counted")
}

func TestUsageRowsHandleBlankMessageTimestampWithoutSessionStart(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "blank-ts", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 1
		s.StartedAt = nil
	})
	insertMessages(t, d, Message{
		SessionID:  "blank-ts",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":100,"output_tokens":50}`),
	})

	daily, err := d.GetDailyUsage(ctx, UsageFilter{})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, 100, daily.Totals.InputTokens)
	assert.Equal(t, 50, daily.Totals.OutputTokens)

	usage, err := d.GetSessionUsage(ctx, "blank-ts", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage)
	assert.Equal(t, []string{"claude-sonnet-4-20250514"}, usage.Models)
}

func TestUsagePreservesSessionSummaryUsageEventTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawInput := MaxPlausibleTokens + 250_000
	rawOutput := MaxPlausibleTokens + 500_000

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "gpt-5.4",
		InputPerMTok:  1.0,
		OutputPerMTok: 2.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "hermes:summary", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
		s.TotalOutputTokens = rawOutput
		s.PeakContextTokens = rawInput
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:summary",
		[]UsageEvent{{
			SessionID:    "hermes:summary",
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  rawInput,
			OutputTokens: rawOutput,
			OccurredAt:   "2026-05-14T10:05:00Z",
			DedupKey:     "session:hermes:summary",
		}},
	), "ReplaceSessionUsageEvents")

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14",
		To:   "2026-05-14",
	})
	requireNoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "daily entries")
	assert.Equal(t, rawInput, daily.Totals.InputTokens, "daily input")
	assert.Equal(t, rawOutput, daily.Totals.OutputTokens, "daily output")

	usage, err := d.GetSessionUsage(ctx, "hermes:summary", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "session usage")
	assert.Equal(t, rawOutput, usage.TotalOutputTokens, "session output total")
	assert.Equal(t, rawInput, usage.PeakContextTokens, "session peak context")
	require.True(t, usage.HasCost, "HasCost")
	wantCost := (float64(rawInput)*1.0 + float64(rawOutput)*2.0) / 1_000_000
	assert.InDelta(t, wantCost, usage.CostUSD, 1e-9, "session usage cost")
}

func TestGetDailyUsageFallsBackForEmptyMessageTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "empty-ts", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "empty-ts",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`,
		),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-15",
		To:   "2024-06-15",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1, "daily entries")
	assert.Equal(t, "2024-06-15", result.Daily[0].Date, "Date")
	assert.Equal(t, 1000, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, 500, result.Totals.OutputTokens, "OutputTokens")
}

func TestUsageQueriesUnionMessageAndUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "claude:msg", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-14T09:00:00Z")
		s.UserMessageCount = 2
	})
	insertMessages(t, d, Message{
		SessionID: "claude:msg",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T09:05:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":40}`,
		),
	})

	insertSession(t, d, "hermes:event", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event",
		[]UsageEvent{{
			SessionID:            "hermes:event",
			Source:               "session",
			Model:                "gpt-5.4",
			InputTokens:          300,
			OutputTokens:         70,
			CacheReadInputTokens: 20,
			DedupKey:             "shared-key",
		}},
	), "replace hermes usage event")
	insertSession(t, d, "hermes:event-2", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:10:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event-2",
		[]UsageEvent{{
			SessionID:    "hermes:event-2",
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  50,
			OutputTokens: 5,
			DedupKey:     "shared-key",
		}},
	), "replace second hermes usage event")

	filter := UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	}
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage")
	require.Equal(t, 450, daily.Totals.InputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 115, daily.Totals.OutputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 20, daily.Totals.CacheReadTokens,
		"daily totals: %#v", daily.Totals)
	require.Len(t, daily.Daily, 1, "daily entries =")
	require.Len(t, daily.Daily[0].AgentBreakdowns, 2,
		"agent breakdowns: %#v", daily.Daily[0].AgentBreakdowns)

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost")
	topByID := make(map[string]TopSessionEntry, len(top))
	for _, entry := range top {
		topByID[entry.SessionID] = entry
	}
	require.Equal(t, 140, topByID["claude:msg"].TotalTokens,
		"claude top tokens: %#v", topByID["claude:msg"])
	require.Equal(t, 390, topByID["hermes:event"].TotalTokens,
		"hermes top tokens: %#v", topByID["hermes:event"])
	require.Equal(t, 55, topByID["hermes:event-2"].TotalTokens,
		"second hermes top tokens: %#v", topByID["hermes:event-2"])

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts")
	require.Equal(t, 3, counts.Total, "counts: %#v", counts)
	require.Equal(t, 1, counts.ByAgent["claude"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByAgent["hermes"], "counts: %#v", counts)
	require.Equal(t, 1, counts.ByProject["proj-a"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByProject["proj-b"], "counts: %#v", counts)
}

func TestGetDailyUsageIncludesCursorUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor")
	require.Len(t, result.Daily, 1, "daily len =")

	day := result.Daily[0]
	assert.Equal(t, "2026-05-14", day.Date, "Date")
	assert.Equal(t, 1234, day.InputTokens, "InputTokens")
	assert.Equal(t, 567, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 0, day.CacheCreationTokens, "CacheCreationTokens")
	assert.Equal(t, 8901, day.CacheReadTokens, "CacheReadTokens")
	assert.InDelta(t, 0.1566, day.TotalCost, 1e-9, "TotalCost")
	require.Equal(t, []string{"claude-4.6-opus-high-thinking"}, day.ModelsUsed)
	require.Len(t, day.AgentBreakdowns, 1)
	assert.Equal(t, "cursor", day.AgentBreakdowns[0].Agent)
	assert.InDelta(t, 0.1566, day.AgentBreakdowns[0].Cost, 1e-9)
	assert.Empty(t, result.Projects, "cursor-only usage should not emit project identities")
	assert.NotContains(t, result.Projects, "")
	assert.Equal(t, 0, result.SessionCounts.Total, "cursor rows should not count as sessions")
}

func TestGetDailyUsageIncludesCursorUsageEventsWithSessionDefaults(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:             "2026-05-14",
		To:               "2026-05-14",
		Breakdowns:       true,
		ExcludeAutomated: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor with defaults")
	require.Len(t, result.Daily, 1, "daily len =")
	assert.Equal(t, 1234, result.Daily[0].InputTokens, "InputTokens")
	assert.Equal(t, 0, result.SessionCounts.Total, "cursor rows should not count as sessions")
}

func TestGetDailyUsageSkipsCursorUsageEventsForExcludeOneShot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:           "2026-05-14",
		To:             "2026-05-14",
		Breakdowns:     true,
		ExcludeOneShot: true,
	})
	require.NoError(t, err, "GetDailyUsage cursor exclude one-shot")
	assert.Empty(t, result.Daily, "daily entries should be empty")
	assert.Zero(t, result.Totals.InputTokens, "InputTokens")
	assert.Zero(t, result.SessionCounts.Total, "cursor rows should not count as sessions")
}

func TestGetDailyUsageSkipsCursorUsageEventsForTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-sonnet-4-20250514",
		InputPerMTok: 3.0, OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "clean-session", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.TerminationStatus = new("clean")
	})
	insertMessages(t, d, Message{
		SessionID: "clean-session",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T10:30:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":40}`,
		),
	})
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:      "2026-05-14T10:05:00Z",
		Model:           "claude-4.6-opus-high-thinking",
		Kind:            "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:     1234,
		OutputTokens:    567,
		CacheReadTokens: 8901,
		ChargedCents:    15.66,
		CursorTokenFee:  3.32,
		UserID:          "152683922",
		UserEmail:       "member@example.com",
	}}), "InsertCursorUsageEvents")

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:        "2026-05-14",
		To:          "2026-05-14",
		Termination: "clean",
	})
	require.NoError(t, err, "GetDailyUsage clean termination")
	require.Len(t, result.Daily, 1, "daily len =")
	assert.Equal(t, 100, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, 40, result.Totals.OutputTokens, "OutputTokens")
	assert.Equal(t, 1, result.SessionCounts.Total, "SessionCounts.Total")
}

func TestInsertCursorUsageEventsDedupesByFingerprint(t *testing.T) {
	d := testDB(t)

	event := CursorUsageEvent{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{event}))
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{event}))

	var count int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM cursor_usage_events",
	).Scan(&count))
	assert.Equal(t, 1, count, "duplicate fingerprint should be ignored")
}

// TestGetDailyUsage_CacheSavingsUsesPerModelRates pins down
// that totals.CacheSavings is computed from each row's actual
// per-model pricing, not a hard-coded proxy. A hard-coded
// Sonnet rate would misreport an Opus-heavy workload because
// Opus rates are roughly 5x Sonnet on both sides.
func TestGetDailyUsage_CacheSavingsUsesPerModelRates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}), "UpsertModelPricing")

	// Same 1M/1M mix of cache read + cache creation tokens
	// on both models so the per-model rate difference is the
	// only thing that can move the result.
	tokens := json.RawMessage(
		`{"input_tokens":0,"output_tokens":0,` +
			`"cache_creation_input_tokens":1000000,` +
			`"cache_read_input_tokens":1000000}`)

	insertSession(t, d, "s-opus", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-opus", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-opus-4-6", TokenUsage: tokens,
	})

	insertSession(t, d, "s-sonnet", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:05:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-sonnet", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:35:00Z",
		Model: "claude-sonnet-4-20250514", TokenUsage: tokens,
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01", To: "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	// Opus per-token delta: read earns (15 - 1.50) = 13.50,
	// creation earns (15 - 18.75) = -3.75.
	// Opus savings on 1M + 1M = 13.50 + (-3.75) = 9.75.
	// Sonnet per-token delta: read earns (3 - 0.30) = 2.70,
	// creation earns (3 - 3.75) = -0.75.
	// Sonnet savings on 1M + 1M = 2.70 + (-0.75) = 1.95.
	// Net total savings = 9.75 + 1.95 = 11.70.
	wantSavings := 11.70
	assert.InDelta(t, wantSavings, result.Totals.CacheSavings, 1e-9,
		"Totals.CacheSavings")

	// Falsification: if the code had used Sonnet rates for
	// both rows the total would be 2 * 1.95 = 3.90, which
	// differs from wantSavings by >$7. Assert we're nowhere
	// near that value so a regression to a single-rate path
	// trips the test.
	assert.Greater(t, math.Abs(result.Totals.CacheSavings-3.90), 0.1,
		"CacheSavings looks like single-rate path; expected per-model math")
}

func TestGetDailyUsageAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}})
	requireNoError(t, err, "UpsertModelPricing")

	// Claude session
	insertSession(t, d, "sess-claude", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-claude",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Codex session
	insertSession(t, d, "sess-codex", "proj1", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-codex",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":1000}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Agent: "claude",
	})
	requireNoError(t, err, "GetDailyUsage agent filter")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
}

func TestGetDailyUsageMultipleDaysAndModels(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "model-a",
			InputPerMTok:  2.0,
			OutputPerMTok: 10.0,
		},
		{
			ModelPattern:  "model-b",
			InputPerMTok:  4.0,
			OutputPerMTok: 20.0,
		},
	})
	requireNoError(t, err, "UpsertModelPricing")

	// Day 1: two models
	insertSession(t, d, "sess-d1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-10T08:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID:  "sess-d1",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-10T08:30:00Z",
			Model:      "model-a",
			TokenUsage: json.RawMessage(`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID:  "sess-d1",
			Ordinal:    1,
			Role:       "assistant",
			Timestamp:  "2024-06-10T09:00:00Z",
			Model:      "model-b",
			TokenUsage: json.RawMessage(`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// Day 2: one model
	insertSession(t, d, "sess-d2", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-11T08:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-d2",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-11T08:30:00Z",
		Model:      "model-a",
		TokenUsage: json.RawMessage(`{"input_tokens":300,"output_tokens":150}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage multi")

	require.Len(t, result.Daily, 2, "got")

	// Day 1: check totals
	d1 := result.Daily[0]
	assert.Equal(t, "2024-06-10", d1.Date, "day1 Date")
	assert.Equal(t, 300, d1.InputTokens, "day1 InputTokens")
	assert.Equal(t, 150, d1.OutputTokens, "day1 OutputTokens")
	assert.Len(t, d1.ModelsUsed, 2, "day1 ModelsUsed count")

	// Day 2
	d2 := result.Daily[1]
	assert.Equal(t, "2024-06-11", d2.Date, "day2 Date")
	assert.Equal(t, 300, d2.InputTokens, "day2 InputTokens")

	// Totals should sum both days
	wantTotalInput := 600
	assert.Equal(t, wantTotalInput, result.Totals.InputTokens, "Totals.InputTokens")
	wantTotalOutput := 300
	assert.Equal(t, wantTotalOutput, result.Totals.OutputTokens, "Totals.OutputTokens")

	// Cost check: day1 model-a = (100*2+50*10)/1e6 = 0.0007
	//             day1 model-b = (200*4+100*20)/1e6 = 0.0028
	//             day2 model-a = (300*2+150*10)/1e6 = 0.0021
	//             total = 0.0056
	wantTotalCost := 0.0056
	assert.InDelta(t, wantTotalCost, result.Totals.TotalCost, 1e-9,
		"Totals.TotalCost")
}

func TestGetDailyUsageNoPricing(t *testing.T) {
	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-07-01",
		To:   "2024-07-31",
	})
	requireNoError(t, err, "GetDailyUsage no pricing")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 500, day.InputTokens, "InputTokens")
	assert.Equal(t, 250, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 0.0, day.TotalCost, "TotalCost")
	assert.Equal(t, []string{"unknown-model"}, day.ModelsUsed,
		"ModelsUsed")
}

func TestGetDailyUsageCostsMessageReasoningTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "gpt-5.4",
		InputPerMTok:  1,
		OutputPerMTok: 2,
	}}))
	insertSession(t, d, "reasoning-message", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-05-14T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "reasoning-message",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T10:30:00Z",
		Model:     "gpt-5.4",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":0,"reasoning_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14", To: "2026-05-14", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Zero(t, result.Totals.OutputTokens)
	assert.InDelta(t, 0.002, result.Totals.TotalCost, 1e-12)
	assert.InDelta(t, 0.002, result.Daily[0].TotalCost, 1e-12)
}

// TestGetDailyUsageTruncatedTokenJSON documents what happens when
// a message lands in the DB with truncated token_usage. The hot
// aggregation counter is intentionally permissive and still extracts
// leading fields, so the valid data is preserved. This is why we don't
// require fully valid JSON on the hot aggregation path: the realistic
// corruption modes reachable from our parsers don't produce silent zeros.
func TestGetDailyUsageTruncatedTokenJSON(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{
			SessionID: "sess1", Ordinal: 0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     "claude-sonnet-4-20250514",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "sess1", Ordinal: 1,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:31:00Z",
			Model:     "claude-sonnet-4-20250514",
			// Truncated mid-key. The usage counter still finds
			// the two leading numeric fields and extracts them.
			TokenUsage: json.RawMessage(
				`{"input_tokens":9999,"output_tokens":4242,"ca`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage truncated")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	// 1000 (valid row) + 9999 (truncated but still parseable)
	assert.Equal(t, 10999, day.InputTokens,
		"InputTokens want 10999 "+
			"(counter should extract leading fields from truncated JSON)")
	assert.Equal(t, 4742, day.OutputTokens, "OutputTokens")
}

func TestParseUsageTokenCounters(t *testing.T) {
	in, out, cacheCreate, cacheRead := parseUsageTokenCounters(
		`{"input_tokens":100,"output_tokens":50,` +
			`"cache_creation_input_tokens":20,` +
			`"cache_read_input_tokens":300,` +
			`"reasoning_tokens":75}`,
	)
	assert.Equal(t, 100, in)
	assert.Equal(t, 50, out)
	assert.Equal(t, 20, cacheCreate)
	assert.Equal(t, 300, cacheRead)

	in, out, cacheCreate, cacheRead, reasoning := parseUsageTokenCountersWithReasoning(
		`{"input_tokens":100,"output_tokens":50,` +
			`"cache_creation_input_tokens":20,` +
			`"cache_read_input_tokens":300,` +
			`"reasoning_tokens":75}`,
	)
	assert.Equal(t, 100, in)
	assert.Equal(t, 50, out)
	assert.Equal(t, 20, cacheCreate)
	assert.Equal(t, 300, cacheRead)
	assert.Equal(t, 75, reasoning)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"input_tokens":9999,"output_tokens":4242,"ca`,
	)
	assert.Equal(t, 9999, in)
	assert.Equal(t, 4242, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"input_tokens":"-5","cache_read_input_tokens":"100",` +
			`"output_tokens":"42"}`,
	)
	assert.Equal(t, -5, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Equal(t, 100, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"metadata":{"input_tokens":999},` +
			`"note":"\"output_tokens\":777",` +
			`"output_tokens":42}`,
	)
	assert.Zero(t, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)

	in, out, cacheCreate, cacheRead = parseUsageTokenCounters(
		`{"metadata":{"url":"https:\/\/x"},"output_tokens":42}`,
	)
	assert.Zero(t, in)
	assert.Equal(t, 42, out)
	assert.Zero(t, cacheCreate)
	assert.Zero(t, cacheRead)
}

func TestUsageAggregationClampsMessageTokenJSON(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const maxTokens = 2_000_000

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         1.0,
		OutputPerMTok:        2.0,
		CacheCreationPerMTok: 3.0,
		CacheReadPerMTok:     4.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.TotalOutputTokens = maxTokens
		s.PeakContextTokens = maxTokens
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "sess1", Ordinal: 0,
		Role:      "assistant",
		Timestamp: "2024-06-15T10:30:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(
			`{"input_tokens":9999999999,` +
				`"output_tokens":9999999999,` +
				`"cache_creation_input_tokens":9999999999,` +
				`"cache_read_input_tokens":9999999999}`),
		ContextTokens:    maxTokens,
		OutputTokens:     maxTokens,
		HasContextTokens: true,
		HasOutputTokens:  true,
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	assert.Equal(t, maxTokens, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, maxTokens, result.Totals.OutputTokens, "OutputTokens")
	assert.Equal(t, maxTokens, result.Totals.CacheCreationTokens,
		"CacheCreationTokens")
	assert.Equal(t, maxTokens, result.Totals.CacheReadTokens,
		"CacheReadTokens")
	assert.InDelta(t, 20.0, result.Totals.TotalCost, 1e-9,
		"TotalCost")

	usage, err := d.GetSessionUsage(ctx, "sess1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "session usage")
	require.True(t, usage.HasCost, "HasCost")
	assert.InDelta(t, 20.0, usage.CostUSD, 1e-9, "CostUSD")
}

func TestGetDailyUsage_DedupesByClaudeMessageAndRequestID(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheCreationPerMTok: 18.75,
		CacheReadPerMTok:     1.50,
	}}), "seed pricing")

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := d.getWriter().Exec(q, args...)
		require.NoError(t, err, "exec %q", q)
	}
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	          VALUES (?, ?, 'local', 'claude', ?, ?)`,
		"s-main", "proj", "2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at, parent_session_id, relationship_type)
	          VALUES (?, ?, 'local', 'claude', ?, ?, 's-main', 'fork')`,
		"s-fork", "proj", "2026-04-10T10:01:00Z", "2026-04-10T10:06:00Z")

	shared := `{"input_tokens":100,"output_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":50000}`
	unique := `{"input_tokens":20,"output_tokens":80,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000}`

	for _, row := range []struct {
		sid, ts, usage, mid, rid string
		ord                      int
	}{
		{"s-main", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:03:00Z", unique, "msg_uniq", "req_uniq", 1},
	} {
		mustExec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 model, token_usage,
			 claude_message_id, claude_request_id,
			 has_output_tokens, has_context_tokens)
			VALUES (?, ?, 'assistant', '', ?, 'claude-opus-4-6', ?, ?, ?, 1, 1)`,
			row.sid, row.ord, row.ts, row.usage, row.mid, row.rid)
	}

	result, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1, "daily entries =")
	day := result.Daily[0]
	assert.Equal(t, 120, day.InputTokens, "input")
	assert.Equal(t, 580, day.OutputTokens, "output")
	assert.Equal(t, 1200, day.CacheCreationTokens, "cache_cr")
	assert.Equal(t, 55000, day.CacheReadTokens, "cache_rd")
}

func TestGetDailyUsage_DedupKeyVariants(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheCreationPerMTok: 18.75,
		CacheReadPerMTok:     1.50,
	}}), "seed pricing")

	insertSession(t, d, "source-main", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-10T10:00:00Z")
		s.EndedAt = new("2026-04-10T10:05:00Z")
	})
	insertSession(t, d, "source-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-10T10:01:00Z")
		s.EndedAt = new("2026-04-10T10:06:00Z")
		s.ParentSessionID = new("source-main")
		s.RelationshipType = "fork"
	})
	insertSession(t, d, "missing-keys", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-04-11T10:00:00Z")
		s.EndedAt = new("2026-04-11T10:05:00Z")
	})

	shared := json.RawMessage(`{"input_tokens":100,"output_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":50000}`)
	unique := json.RawMessage(`{"input_tokens":20,"output_tokens":80,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000}`)
	missingKeysUsage := json.RawMessage(`{"input_tokens":0,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`)

	insertMessages(t, d,
		Message{
			SessionID: "source-main", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-10T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: shared, HasOutputTokens: true,
			ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
		},
		Message{
			SessionID: "source-fork", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-10T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: shared, HasOutputTokens: true,
			ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
		},
		Message{
			SessionID: "source-fork", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-04-10T10:03:00Z",
			Model: "claude-opus-4-6", TokenUsage: unique, HasOutputTokens: true,
			ClaudeMessageID: "msg_uniq", SourceUUID: "source_uniq",
		},
		Message{
			SessionID: "missing-keys", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-04-11T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: missingKeysUsage,
			HasOutputTokens: true,
		},
		Message{
			SessionID: "missing-keys", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-04-11T10:02:00Z",
			Model: "claude-opus-4-6", TokenUsage: missingKeysUsage,
			HasOutputTokens: true,
		},
	)

	t.Run("dedupes by source uuid when claude pair incomplete", func(t *testing.T) {
		result, err := d.GetDailyUsage(context.Background(), UsageFilter{
			From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
		})
		require.NoError(t, err, "GetDailyUsage")
		require.Len(t, result.Daily, 1, "daily entries =")
		day := result.Daily[0]
		assert.Equal(t, 120, day.InputTokens, "input")
		assert.Equal(t, 580, day.OutputTokens, "output")
		assert.Equal(t, 1200, day.CacheCreationTokens, "cache_cr")
		assert.Equal(t, 55000, day.CacheReadTokens, "cache_rd")
	})

	t.Run("missing dedup keys counted every time", func(t *testing.T) {
		result, err := d.GetDailyUsage(context.Background(), UsageFilter{
			From: "2026-04-11", To: "2026-04-11", Timezone: "UTC",
		})
		require.NoError(t, err, "GetDailyUsage")
		require.Len(t, result.Daily, 1,
			"output want 20 (both no-key rows counted): %v", result.Daily)
		assert.Equal(t, 20, result.Daily[0].OutputTokens,
			"output want 20 (both no-key rows counted): %v", result.Daily)
	})
}

func TestGetDailyUsageLongLivedSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-6",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "upsert pricing")

	// Session started on Apr 1 but has messages on Apr 10.
	requireNoError(t, d.UpsertSession(Session{
		ID: "long-lived", Project: "proj", Machine: "local",
		Agent:     "claude",
		StartedAt: new("2026-04-01T10:00:00Z"),
	}), "upsert session")

	insertMessages(t, d,
		Message{
			SessionID: "long-lived", Ordinal: 0,
			Role: "assistant", Content: "early",
			ContentLength: 5,
			Timestamp:     "2026-04-01T10:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":100,"output_tokens":50}`),
			ContextTokens:    100,
			OutputTokens:     50,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
		Message{
			SessionID: "long-lived", Ordinal: 1,
			Role: "assistant", Content: "late",
			ContentLength: 4,
			Timestamp:     "2026-04-10T14:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":2000,"output_tokens":500}`),
			ContextTokens:    2000,
			OutputTokens:     500,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
	)

	// Query Apr 10 only — should include the late message even
	// though the session started on Apr 1.
	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:     "2026-04-10",
		To:       "2026-04-10",
		Timezone: "UTC",
	})
	requireNoError(t, err, "GetDailyUsage long-lived")

	require.Len(t, result.Daily, 1, "expected 1 day")
	assert.Equal(t, 2000, result.Daily[0].InputTokens, "InputTokens")
}

func TestGetDailyUsageProjectFilter(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:    "2024-06-01",
		To:      "2024-06-30",
		Project: "proj-a",
	})
	requireNoError(t, err, "GetDailyUsage project filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 4000, day.InputTokens, "InputTokens")
	assert.Equal(t, 4000, result.Totals.InputTokens, "Totals.InputTokens")
}

func TestGetDailyUsageModelFilter(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5",
	})
	requireNoError(t, err, "GetDailyUsage model filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 4000, day.InputTokens, "InputTokens")
	assert.Equal(t, []string{"gpt-5"}, day.ModelsUsed, "ModelsUsed")
}

func TestGetDailyUsageProjectBreakdowns(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage project breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.ProjectBreakdowns, 2, "ProjectBreakdowns len")

	projMap := make(map[string]ProjectBreakdown)
	var projCostSum float64
	for _, pb := range day.ProjectBreakdowns {
		projMap[pb.Project] = pb
		projCostSum += pb.Cost
	}
	for _, name := range []string{"proj-a", "proj-b"} {
		pb, ok := projMap[name]
		if !assert.Truef(t, ok,
			"missing ProjectBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 4000, pb.InputTokens,
			"%s InputTokens", name)
	}
	assert.InDelta(t, day.TotalCost, projCostSum, 1e-9,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
}

func TestDailyUsageEntryBreakdownSlicesMarshalAsEmptyArrays(t *testing.T) {
	data, err := json.Marshal(DailyUsageEntry{
		Date:       "2026-07-03",
		ModelsUsed: []string{},
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, []any{}, got["modelBreakdowns"])
	assert.Equal(t, []any{}, got["projectBreakdowns"])
	assert.Equal(t, []any{}, got["agentBreakdowns"])
	assert.Equal(t, []any{}, got["machineBreakdowns"])
}

func TestGetDailyUsageMachineBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "model-a",
		InputPerMTok:  1,
		OutputPerMTok: 5,
	}}))

	type machineUsage struct {
		name         string
		inputTokens  int
		outputTokens int
	}
	fixtures := []machineUsage{
		{name: "host-a", inputTokens: 2000, outputTokens: 200},
		{name: "host-b", inputTokens: 1000, outputTokens: 100},
	}
	for i, fixture := range fixtures {
		sessionID := fmt.Sprintf("machine-breakdown-%d", i)
		insertSession(t, d, sessionID, "project-a", func(s *Session) {
			s.Machine = fixture.name
			s.Agent = "claude"
			s.StartedAt = Ptr("2026-07-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2026-07-15T10:30:00Z",
			Model:     "model-a",
			TokenUsage: json.RawMessage(fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":%d}`,
				fixture.inputTokens, fixture.outputTokens,
			)),
		})
	}

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-07-15",
		To:         "2026-07-15",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	day := result.Daily[0]
	require.Len(t, day.MachineBreakdowns, 2)
	assert.Equal(t, "host-a", day.MachineBreakdowns[0].MachineName)
	assert.Equal(t, 2000, day.MachineBreakdowns[0].InputTokens)
	assert.Equal(t, 200, day.MachineBreakdowns[0].OutputTokens)
	assert.InDelta(t, 0.003, day.MachineBreakdowns[0].Cost, 1e-9)
	assert.Equal(t, "host-b", day.MachineBreakdowns[1].MachineName)
	assert.Equal(t, 1000, day.MachineBreakdowns[1].InputTokens)
	assert.Equal(t, 100, day.MachineBreakdowns[1].OutputTokens)
	assert.InDelta(t, 0.0015, day.MachineBreakdowns[1].Cost, 1e-9)
	assert.InDelta(t, day.TotalCost,
		day.MachineBreakdowns[0].Cost+day.MachineBreakdowns[1].Cost, 1e-9)

	fastResult, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-15", To: "2026-07-15", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, fastResult.Daily, 1)
	assert.Empty(t, fastResult.Daily[0].MachineBreakdowns)
}

func TestGetDailyUsageAgentBreakdowns(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage agent breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.AgentBreakdowns, 2, "AgentBreakdowns len")

	agentMap := make(map[string]AgentBreakdown)
	var agentCostSum float64
	for _, ab := range day.AgentBreakdowns {
		agentMap[ab.Agent] = ab
		agentCostSum += ab.Cost
	}
	for _, name := range []string{"claude", "codex"} {
		ab, ok := agentMap[name]
		if !assert.Truef(t, ok,
			"missing AgentBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 4000, ab.InputTokens,
			"%s InputTokens", name)
	}
	assert.InDelta(t, day.TotalCost, agentCostSum, 1e-9,
		"sum(AgentBreakdowns.Cost) want TotalCost")
}

func TestGetDailyUsageBreakdownInvariant(t *testing.T) {

	d := openDailyUsageFixtureDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage breakdown invariant")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]

	var modelCostSum float64
	for _, mb := range day.ModelBreakdowns {
		modelCostSum += mb.Cost
	}
	var projectCostSum float64
	for _, pb := range day.ProjectBreakdowns {
		projectCostSum += pb.Cost
	}
	var agentCostSum float64
	for _, ab := range day.AgentBreakdowns {
		agentCostSum += ab.Cost
	}

	assert.InDelta(t, day.TotalCost, modelCostSum, 1e-9,
		"sum(ModelBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, day.TotalCost, projectCostSum, 1e-9,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, day.TotalCost, agentCostSum, 1e-9,
		"sum(AgentBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, projectCostSum, modelCostSum, 1e-9,
		"model cost sum != project cost sum")
	assert.InDelta(t, agentCostSum, modelCostSum, 1e-9,
		"model cost sum != agent cost sum")
}

// BenchmarkGetDailyUsage measures the hot-path scan over a realistic
// synthetic dataset. The baseline number (captured against the commit
// that introduces this benchmark) is the non-regression budget for all
// subsequent changes to GetDailyUsage: new code must land within +10%.
//
// See docs/specs/2026-04-12-token-usage-ui-design.md for the full
// non-destructive benchmark procedure.
func TestGetTopSessionsByCost(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	// Expensive session
	insertSession(t, d, "sBig", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("Big Session")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sBig", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":5000,"output_tokens":2000,` +
				`"cache_creation_input_tokens":1000,` +
				`"cache_read_input_tokens":3000}`),
	})

	// Cheap session
	insertSession(t, d, "sSmall", "proj-b", func(s *Session) {
		s.Agent = "codex"
		s.SessionName = new("Small Session")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sSmall", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50,` +
				`"cache_creation_input_tokens":10,` +
				`"cache_read_input_tokens":20}`),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	// Ordered cost desc — sBig first
	assert.Equal(t, "sBig", top[0].SessionID, "top[0].SessionID")
	assert.Equal(t, "Big Session", top[0].DisplayName, "top[0].DisplayName")
	assert.Equal(t, "proj-a", top[0].Project, "top[0].Project")
	assert.Equal(t, "claude", top[0].Agent, "top[0].Agent")
	// TotalTokens = 5000 + 2000 + 1000 + 3000 = 11000
	assert.Equal(t, 11000, top[0].TotalTokens, "top[0].TotalTokens")
	assert.Greater(t, top[0].Cost, 0.0, "top[0].Cost want > 0")

	assert.Equal(t, "sSmall", top[1].SessionID, "top[1].SessionID")
	assert.Greater(t, top[0].Cost, top[1].Cost,
		"top[0].Cost should be > top[1].Cost")
}

func TestGetTopSessionsByCost_DisplayNameFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	tokenJSON := `{"input_tokens":100,"output_tokens":50,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`

	// Session with session_name set — should use session_name via COALESCE.
	insertSession(t, d, "s-dn", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("My Custom Name")
		s.FirstMessage = new("some first message")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-dn", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name — should fall back to first_message.
	insertSession(t, d, "s-fm", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.FirstMessage = new("fix the login bug")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-fm", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name and no first_message — should
	// fall back to project.
	insertSession(t, d, "s-proj", "my-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-proj", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name, no first_message, and empty
	// project — should fall back to session ID.
	insertSession(t, d, "s-id", "", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T13:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-id", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T13:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost fallback")

	require.Len(t, top, 4, "len")

	// Build a map for easy lookup (order is by cost, all equal
	// here so secondary sort is by session ID).
	byID := make(map[string]TopSessionEntry)
	for _, e := range top {
		byID[e.SessionID] = e
	}

	assert.Equal(t, "My Custom Name", byID["s-dn"].DisplayName,
		"s-dn DisplayName")
	assert.Equal(t, "fix the login bug", byID["s-fm"].DisplayName,
		"s-fm DisplayName")
	assert.Equal(t, "my-project", byID["s-proj"].DisplayName,
		"s-proj DisplayName")
	assert.Equal(t, "s-id", byID["s-id"].DisplayName,
		"s-id DisplayName")
}

// TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID
// mirrors TestGetDailyUsage_DedupesByClaudeMessageAndRequestID
// for the top-sessions query: a parent session and a forked
// session that both replay the same Claude message should only
// count that message once in the per-session totals. The
// earliest-timestamp session wins the credit.
func TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	// Parent session starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Forked session starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500,` +
			`"cache_creation_input_tokens":200,` +
			`"cache_read_input_tokens":3000}`)
	unique := json.RawMessage(
		`{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0}`)

	// The shared message exists on both sessions with the same
	// Claude IDs; the parent's timestamp is earlier so it should
	// win the dedup.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Plus a unique fork-only message so the fork still appears.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 1,
		Role: "assistant", Timestamp: "2024-06-15T10:04:00Z",
		Model: "claude-sonnet", TokenUsage: unique,
		ClaudeMessageID: "msg_uniq", ClaudeRequestID: "req_uniq",
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	require.True(t, ok, "s-parent missing from top sessions")
	// Parent owns shared: 1000+500+200+3000 = 4700 tokens.
	assert.Equal(t, 4700, parent.TotalTokens, "parent.TotalTokens")

	fork, ok := byID["s-fork"]
	require.True(t, ok, "s-fork missing from top sessions")
	// Fork should only own the unique message: 10+20 = 30
	// tokens. If the dedup were missing, the shared row would
	// be counted again and this would jump to 4730.
	assert.Equal(t, 30, fork.TotalTokens,
		"fork.TotalTokens want 30 "+
			"(shared message should be deduped)")

	// Total across both entries must equal the undeduped
	// message sum: parent 4700 + fork 30 = 4730.
	total := parent.TotalTokens + fork.TotalTokens
	assert.Equal(t, 4730, total, "sum of per-session totals")
}

func TestGetTopSessionsByCost_DedupesBySourceUUIDWhenClaudePairIncomplete(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500,` +
			`"cache_creation_input_tokens":200,` +
			`"cache_read_input_tokens":3000}`)
	unique := json.RawMessage(
		`{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0}`)

	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 1,
		Role: "assistant", Timestamp: "2024-06-15T10:04:00Z",
		Model: "claude-sonnet", TokenUsage: unique,
		ClaudeMessageID: "msg_uniq", SourceUUID: "source_uniq",
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")
	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	require.True(t, ok, "s-parent missing from top sessions")
	assert.Equal(t, 4700, parent.TotalTokens, "parent.TotalTokens")

	fork, ok := byID["s-fork"]
	require.True(t, ok, "s-fork missing from top sessions")
	assert.Equal(t, 30, fork.TotalTokens, "fork.TotalTokens want 30")
}

func TestGetTopSessionsByCostLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	for i := range 5 {
		sid := "sess-" + strconv.Itoa(i)
		insertSession(t, d, sid, "proj", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sid, Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		})
	}

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 3)
	requireNoError(t, err, "GetTopSessionsByCost limit")

	require.Len(t, top, 3, "len")
}

func TestGetUsageSessionCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// s1: proj-a / claude — TWO messages across TWO days
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID: "s1", Ordinal: 1,
			Role: "assistant", Timestamp: "2024-06-16T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// s2: proj-a / codex
	insertSession(t, d, "s2", "proj-a", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s2", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	// s3: proj-b / claude
	insertSession(t, d, "s3", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s3", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, 2, counts.ByProject["proj-a"], "ByProject[proj-a]")
	assert.Equal(t, 1, counts.ByProject["proj-b"], "ByProject[proj-b]")
	assert.Equal(t, 2, counts.ByAgent["claude"], "ByAgent[claude]")
	assert.Equal(t, 1, counts.ByAgent["codex"], "ByAgent[codex]")

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, counts.Total, daily.SessionCounts.Total)
	assert.Equal(t, counts.ByAgent, daily.SessionCounts.ByAgent)
	projectCounts := make(map[string]int, len(daily.Projects))
	for key, project := range daily.Projects {
		projectCounts[project.DisplayLabel] = daily.SessionCounts.ByProject[key]
		assert.NotContains(t, key, project.DisplayLabel)
	}
	assert.Equal(t, counts.ByProject, projectCounts)

	dailyNoCounts, err := d.GetDailyUsage(ctx, UsageFilter{
		From:              "2024-06-01",
		To:                "2024-06-30",
		SkipSessionCounts: true,
	})
	requireNoError(t, err, "GetDailyUsage skip counts")
	assert.Equal(t, daily.Daily, dailyNoCounts.Daily)
	assert.Equal(t, daily.Totals, dailyNoCounts.Totals)
	assert.Zero(t, dailyNoCounts.SessionCounts.Total)
	assert.Nil(t, dailyNoCounts.SessionCounts.ByProject)
	assert.Nil(t, dailyNoCounts.SessionCounts.ByAgent)
}

func TestGetUsageMatchingSessionCount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-empty", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-empty", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:00:00Z",
		Model: "gpt-5.3-codex",
	})

	insertSession(t, d, "claude-usage", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T11:00:00Z")
		s.EndedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "claude-usage", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:00:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	tests := []struct {
		name   string
		filter UsageFilter
		want   int
	}{
		{
			name: "counts copilot sessions without usage rows",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot",
			},
			want: 1,
		},
		{
			name: "respects model filters from session messages",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot", Model: "gpt-5.3-codex",
			},
			want: 1,
		},
		{
			name: "excludes sessions when model is excluded",
			filter: UsageFilter{
				From: "2024-06-15", To: "2024-06-15",
				Timezone: "UTC", Agent: "copilot", ExcludeModel: "gpt-5.3-codex",
			},
			want: 0,
		},
		{
			name: "respects date range",
			filter: UsageFilter{
				From: "2024-06-16", To: "2024-06-16",
				Timezone: "UTC", Agent: "copilot",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.GetUsageMatchingSessionCount(ctx, tt.filter)
			requireNoError(t, err, "GetUsageMatchingSessionCount")
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestGetUsageMatchingSessionCount_UsesMessageTimestampNotSessionActivity
// guards against regressing to session-activity bounding: a Copilot
// session whose started_at/ended_at fall outside the requested window but
// whose message is timestamped inside it must still be counted, because
// GetDailyUsage and GetUsageSessionCounts already bound on message/event
// timestamps, not session activity.
func TestGetUsageMatchingSessionCount_UsesMessageTimestampNotSessionActivity(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-late-message", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-08T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-late-message", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-10T12:00:00Z",
		Model:      "gpt-5.3-codex",
		TokenUsage: nil,
	})

	insertSession(t, d, "copilot-out-of-range", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-08T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-out-of-range", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-08T10:00:00Z",
		Model:      "gpt-5.3-codex",
		TokenUsage: nil,
	})

	filter := UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	}

	got, err := d.GetUsageMatchingSessionCount(ctx, filter)
	requireNoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, got)
}

// TestGetUsageMatchingSessionCount_ModelFilterAppliesToBoundedRow guards
// against the model/exclude-model predicate being applied session-wide
// instead of to the in-range message/event row: a session with an
// out-of-range message on the filtered model but an in-range message on a
// different model must not match a Model filter for the out-of-range
// model, and must not be excluded by an ExcludeModel filter for the
// out-of-range model either.
func TestGetUsageMatchingSessionCount_ModelFilterAppliesToBoundedRow(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-mixed-model", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-08T10:00:00Z")
		s.EndedAt = new("2026-02-10T12:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "copilot-mixed-model", Ordinal: 0,
			Role: "assistant", Timestamp: "2026-02-08T10:00:00Z",
			Model:      "gpt-5.3-codex",
			TokenUsage: nil,
		},
		Message{
			SessionID: "copilot-mixed-model", Ordinal: 1,
			Role: "assistant", Timestamp: "2026-02-10T12:00:00Z",
			Model:      "claude-sonnet",
			TokenUsage: nil,
		},
	)

	inRangeFilter := UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	}

	got, err := d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: inRangeFilter.From, To: inRangeFilter.To,
		Timezone: inRangeFilter.Timezone, Agent: inRangeFilter.Agent,
		Model: "gpt-5.3-codex",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount with Model")
	assert.Equal(t, 0, got,
		"out-of-range message's model must not match the bounded window")

	got, err = d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: inRangeFilter.From, To: inRangeFilter.To,
		Timezone: inRangeFilter.Timezone, Agent: inRangeFilter.Agent,
		ExcludeModel: "gpt-5.3-codex",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount with ExcludeModel")
	assert.Equal(t, 1, got,
		"in-range message's model is not excluded, so the session must still count")
}

// TestGetUsageMatchingSessionCount_CountsAssistantMessageWithNoModel
// guards against gating matching-session eligibility on m.model != ”:
// some Copilot assistant messages parse before a model name is known, so
// an assistant message with an empty Model must still count toward the
// matching-session total when no Model/ExcludeModel filter narrows it.
func TestGetUsageMatchingSessionCount_CountsAssistantMessageWithNoModel(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-no-model", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-02-10T10:00:00Z")
		s.EndedAt = new("2026-02-10T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-no-model", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-02-10T10:00:00Z",
		Model:      "",
		TokenUsage: nil,
	})

	got, err := d.GetUsageMatchingSessionCount(ctx, UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	})
	requireNoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, got,
		"assistant message with no model must still count without a model filter")
}

// TestGetUsageMatchingSessionCount_UnboundedMatchesBoundedSemantics guards
// against the unbounded (no From/To) branch drifting from the bounded
// branch: both must require an assistant, non-synthetic message (or a
// usage_events row with a model), both must admit empty-model assistant
// messages, and Model/ExcludeModel must narrow the same rows.
func TestGetUsageMatchingSessionCount_UnboundedMatchesBoundedSemantics(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "copilot-user-only", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T10:00:00Z")
		s.EndedAt = new("2026-03-01T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-user-only", Ordinal: 0,
		Role: "user", Timestamp: "2026-03-01T10:00:00Z",
	})

	insertSession(t, d, "copilot-synthetic-only", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T11:00:00Z")
		s.EndedAt = new("2026-03-01T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-synthetic-only", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-03-01T11:00:00Z",
		Model: "<synthetic>",
	})

	insertSession(t, d, "copilot-no-model-msg", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T12:00:00Z")
		s.EndedAt = new("2026-03-01T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "copilot-no-model-msg", Ordinal: 0,
		Role: "assistant", Timestamp: "2026-03-01T12:00:00Z",
		Model: "",
	})

	insertSession(t, d, "copilot-no-messages", "proj-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-03-01T13:00:00Z")
		s.EndedAt = new("2026-03-01T13:00:00Z")
	})

	tests := []struct {
		name   string
		filter UsageFilter
		want   int
	}{
		{
			name:   "unbounded requires assistant or event activity",
			filter: UsageFilter{Timezone: "UTC", Agent: "copilot"},
			// Only copilot-no-model-msg has a qualifying assistant
			// message; user-only, synthetic-only, and message-less
			// sessions must not count.
			want: 1,
		},
		{
			name: "unbounded exclude-model keeps empty-model assistant messages",
			filter: UsageFilter{
				Timezone: "UTC", Agent: "copilot",
				ExcludeModel: "gpt-5.3-codex",
			},
			want: 1,
		},
		{
			name: "unbounded model filter narrows to matching rows",
			filter: UsageFilter{
				Timezone: "UTC", Agent: "copilot", Model: "gpt-5.3-codex",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.GetUsageMatchingSessionCount(ctx, tt.filter)
			requireNoError(t, err, "GetUsageMatchingSessionCount")
			assert.Equal(t, tt.want, got)

			bounded := tt.filter
			bounded.From = "2026-03-01"
			bounded.To = "2026-03-01"
			boundedGot, err := d.GetUsageMatchingSessionCount(ctx, bounded)
			requireNoError(t, err, "GetUsageMatchingSessionCount bounded")
			assert.Equal(t, got, boundedGot,
				"bounded and unbounded requests must match the same sessions")
		})
	}
}

func TestNewUsageSessionCounts(t *testing.T) {
	counts := NewUsageSessionCounts(map[string]UsageSessionInfo{
		"s1": {Project: "proj-a", Agent: "claude"},
		"s2": {Project: "proj-a", Agent: "codex"},
		"s3": {Project: "proj-b", Agent: "claude"},
	})

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, map[string]int{
		"proj-a": 2,
		"proj-b": 1,
	}, counts.ByProject, "ByProject")
	assert.Equal(t, map[string]int{
		"claude": 2,
		"codex":  1,
	}, counts.ByAgent, "ByAgent")
}

// TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID
// mirrors the dedup regression coverage on the other two usage
// queries. A fork session whose only qualifying messages are
// replays of its parent's (same claude_message_id +
// claude_request_id) contributes zero cost after dedup in
// GetDailyUsage, so it must also NOT be counted in
// GetUsageSessionCounts — otherwise the summary cards disagree
// with the charts.
func TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	// Parent starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Fork starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(
		`{"input_tokens":100,"output_tokens":50}`)

	// Parent has one unique message.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Fork's ONLY qualifying message is a replay of the parent
	// row — same claude IDs. After dedup the fork contributes
	// nothing and must not be counted.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 1, counts.Total,
		"Total want 1 (fork should dedup out)")
	assert.Equal(t, 1, counts.ByProject["proj"], "ByProject[proj]")
	assert.Equal(t, 1, counts.ByAgent["claude"], "ByAgent[claude]")
}

func TestGetUsageSessionCounts_DedupesBySourceUUIDWhenClaudePairIncomplete(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(`{"input_tokens":100,"output_tokens":50}`)

	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", SourceUUID: "source_dup",
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 1, counts.Total, "Total want 1 (fork should dedup out)")
	assert.Equal(t, 1, counts.ByProject["proj"], "ByProject[proj]")
	assert.Equal(t, 1, counts.ByAgent["claude"], "ByAgent[claude]")
}

// TestUsageQueryEligibilityParity seeds messages that fail each
// disqualification predicate and asserts all three usage queries
// ignore them. Guardrail against drift between usage queries.
func TestUsageQueryEligibilityParity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	// Good session — should be visible to all queries.
	insertSession(t, d, "good", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "good", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Bad: empty token_usage
	insertSession(t, d, "bad-empty", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-empty", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(""),
	})

	// Bad: synthetic model
	insertSession(t, d, "bad-synth", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-synth", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "<synthetic>",
		TokenUsage: json.RawMessage(
			`{"input_tokens":999,"output_tokens":999}`),
	})

	// Bad: soft-deleted session
	insertSession(t, d, "bad-deleted", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-deleted", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":999,"output_tokens":999}`),
	})
	requireNoError(t,
		d.SoftDeleteSession("bad-deleted"),
		"SoftDeleteSession")

	filter := UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	}

	// GetDailyUsage
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage parity")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "GetDailyUsage InputTokens")

	// GetUsageSessionCounts
	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts parity")
	assert.Equal(t, 1, counts.Total, "GetUsageSessionCounts Total")

	// GetTopSessionsByCost
	top, err := d.GetTopSessionsByCost(ctx, filter, 20)
	requireNoError(t, err, "GetTopSessionsByCost parity")
	require.Len(t, top, 1, "GetTopSessionsByCost len")
	assert.Equal(t, "good", top[0].SessionID,
		"GetTopSessionsByCost[0].SessionID")
}

// TestExcludeProjectFilter verifies that ExcludeProject removes
// matching projects from all three usage queries.
func TestExcludeProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sA", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sB", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sC", "proj-c", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "sA", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "sB", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "sC", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
	)

	base := UsageFilter{From: "2024-06-01", To: "2024-06-30"}

	// Exclude one project.
	f1 := base
	f1.ExcludeProject = "proj-b"
	daily, err := d.GetDailyUsage(ctx, f1)
	requireNoError(t, err, "GetDailyUsage exclude one")
	assert.Equal(t, 2000, daily.Totals.InputTokens, "exclude proj-b: InputTokens")

	// Exclude two projects (comma-separated).
	f2 := base
	f2.ExcludeProject = "proj-a,proj-c"
	daily, err = d.GetDailyUsage(ctx, f2)
	requireNoError(t, err, "GetDailyUsage exclude two")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude a+c: InputTokens")

	// GetTopSessionsByCost with exclude.
	top, err := d.GetTopSessionsByCost(ctx, f1, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude")
	require.Len(t, top, 2, "exclude proj-b: top len =")
	for _, ts := range top {
		assert.NotEqual(t, "proj-b", ts.Project,
			"excluded proj-b still in top sessions")
	}

	// GetUsageSessionCounts with exclude.
	counts, err := d.GetUsageSessionCounts(ctx, f1)
	requireNoError(t, err, "GetUsageSessionCounts exclude")
	assert.Equal(t, 2, counts.Total, "exclude proj-b: Total")
	assert.Equal(t, 0, counts.ByProject["proj-b"], "excluded proj-b count")
}

func TestUsageSessionFilters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	tokenUsage := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-filter-keep", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-machine", "proj", func(s *Session) {
		s.Machine = "host-b"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-prompts", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-one-shot", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 1
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-automated", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 1 WHERE id = ?",
		"usage-filter-automated",
	)
	require.NoError(t, err, "patch automated fixture")

	for _, sid := range []string{
		"usage-filter-keep",
		"usage-filter-machine",
		"usage-filter-prompts",
		"usage-filter-one-shot",
		"usage-filter-automated",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		Machine:          "host-a",
		MinUserMessages:  2,
		ExcludeOneShot:   true,
		ExcludeAutomated: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage session filters")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost session filters")
	require.Len(t, top, 1,
		"top sessions want only usage-filter-keep: %+v", top)
	require.Equal(t, "usage-filter-keep", top[0].SessionID,
		"top sessions want only usage-filter-keep: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts session filters")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

func TestUsageTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	clean := "clean"
	unclean := "tool_call_pending"
	tokenUsage := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`,
	)
	insertSession(t, d, "usage-filter-clean", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &clean
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-unclean", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &unclean
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	for _, sid := range []string{
		"usage-filter-clean",
		"usage-filter-unclean",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		Termination: "clean",
	}
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage termination filter")
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost termination filter")
	if len(top) != 1 || top[0].SessionID != "usage-filter-clean" {
		t.Fatalf("top sessions = %+v, want only usage-filter-clean", top)
	}

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts termination filter")
	if counts.Total != 1 {
		t.Errorf("counts.Total = %d, want 1", counts.Total)
	}
}

// TestUsageActivityFallbackEmptyEndedAt guards the SQLite usage activity-time
// fallback. A session whose ended_at was never persisted is stored as the empty
// string, so COALESCE(s.ended_at, ...) returned ” and strftime('%s', ”)
// yielded NULL, silently dropping it from active_since and the active/stale/
// unclean termination filters. NULLIF must let the fallback reach started_at.
func TestUsageActivityFallbackEmptyEndedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	flagged := "tool_call_pending"
	// started_at set; ended_at persisted as the empty string, the legacy
	// state every other read query guards with NULLIF(ended_at, '').
	insertSession(t, d, "untimed", "proj", func(s *Session) {
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.TerminationStatus = &flagged
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("")
	})
	insertMessages(t, d, Message{
		SessionID:  "untimed",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	// active_since before started_at must keep the session via the fallback.
	activeCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		ActiveSince: "2024-06-01T00:00:00Z",
	})
	require.NoError(t, err, "GetUsageSessionCounts active_since")
	assert.Equal(t, 1, activeCounts.Total,
		"active_since must match empty-ended_at session via started_at")

	// The unclean filter evaluates the activity epoch expression; the flagged
	// session with an old started_at must be matched.
	uncleanCounts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From:        "2024-06-01",
		To:          "2024-06-30",
		Termination: "unclean",
	})
	require.NoError(t, err, "GetUsageSessionCounts unclean")
	assert.Equal(t, 1, uncleanCounts.Total,
		"unclean must match flagged empty-ended_at session via started_at")
}

func TestUsageExcludeOneShotUsesUserMessageCount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	tokenUsage := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-one-user-message", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-two-user-messages", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 3
		s.UserMessageCount = 2
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	for _, sid := range []string{
		"usage-one-user-message",
		"usage-two-user-messages",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:           "2024-06-01",
		To:             "2024-06-30",
		ExcludeOneShot: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage exclude one-shot")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude one-shot")
	require.Len(t, top, 1,
		"top sessions want only usage-two-user-messages: %+v", top)
	require.Equal(t, "usage-two-user-messages", top[0].SessionID,
		"top sessions want only usage-two-user-messages: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts exclude one-shot")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

// TestExcludeAgentFilter verifies ExcludeAgent on GetDailyUsage.
func TestExcludeAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "s2", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeAgent: "codex",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude agent")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude codex: InputTokens")
}

// TestExcludeModelFilter verifies ExcludeModel on GetDailyUsage.
func TestExcludeModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "sonnet", InputPerMTok: 3.0,
			OutputPerMTok: 15.0},
		{ModelPattern: "opus", InputPerMTok: 15.0,
			OutputPerMTok: 75.0},
	}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`)},
		Message{SessionID: "s1", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-15T11:30:00Z", Model: "opus",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeModel: "opus",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude model")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude opus: InputTokens")
	require.Len(t, daily.Daily, 1, "daily len =")
	for _, mb := range daily.Daily[0].ModelBreakdowns {
		assert.NotEqual(t, "opus", mb.ModelName,
			"excluded model opus still in breakdowns")
	}
}

func BenchmarkGetDailyUsage(b *testing.B) {
	d := testDB(b)
	ctx := context.Background()
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	if err := d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet-4-20250514",
			InputPerMTok: 3.0, OutputPerMTok: 15.0,
			CacheCreationPerMTok: 3.75, CacheReadPerMTok: 0.30},
		{ModelPattern: "claude-opus-4-20250514",
			InputPerMTok: 15.0, OutputPerMTok: 75.0,
			CacheCreationPerMTok: 18.75, CacheReadPerMTok: 1.50},
		{ModelPattern: "gpt-5",
			InputPerMTok: 2.5, OutputPerMTok: 10.0,
			CacheCreationPerMTok: 2.5, CacheReadPerMTok: 0.25},
		{ModelPattern: "gemini-2.5-pro",
			InputPerMTok: 1.25, OutputPerMTok: 5.0,
			CacheCreationPerMTok: 1.25, CacheReadPerMTok: 0.125},
	}); err != nil {
		b.Fatalf("UpsertModelPricing: %v", err)
	}

	projects := []string{
		"agentsview", "quokka", "arrow-rs", "side-quests",
		"infrastructure", "blog", "experiments", "docs",
		"dotfiles", "playground",
	}
	agents := []string{"claude", "codex", "openhands"}
	models := []string{
		"claude-sonnet-4-20250514",
		"claude-opus-4-20250514",
		"gpt-5",
		"gemini-2.5-pro",
	}

	// 500 sessions × 200 messages each = 100k rows.
	const sessionCount = 500
	const msgsPerSession = 200

	tokenUsage := `{"input_tokens":1200,"output_tokens":480,` +
		`"cache_creation_input_tokens":300,` +
		`"cache_read_input_tokens":2400}`

	// Pre-parse the anchor timestamp once; the seed loop offsets from it.
	startTime, err := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	if err != nil {
		b.Fatalf("parsing start time: %v", err)
	}

	for i := range sessionCount {
		id := "bench-sess-" + strconv.Itoa(i)
		project := projects[i%len(projects)]
		agent := agents[i%len(agents)]
		// Spread sessions across a 60-day window.
		dayOffset := i % 60
		s := Session{
			ID:           id,
			Project:      project,
			Machine:      defaultMachine,
			Agent:        agent,
			MessageCount: msgsPerSession,
			StartedAt:    new(startTime.Format(time.RFC3339)),
		}
		if err := d.UpsertSession(s); err != nil {
			b.Fatalf("UpsertSession: %v", err)
		}
		msgs := make([]Message, msgsPerSession)
		for j := range msgsPerSession {
			msgs[j] = Message{
				SessionID:  id,
				Ordinal:    j,
				Role:       "assistant",
				Timestamp:  startTime.AddDate(0, 0, dayOffset).Format(time.RFC3339),
				Model:      models[(i+j)%len(models)],
				TokenUsage: json.RawMessage(tokenUsage),
			}
		}
		if err := d.InsertMessages(msgs); err != nil {
			b.Fatalf("InsertMessages: %v", err)
		}
	}

	log.SetOutput(origLog)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := d.GetDailyUsage(ctx, UsageFilter{
			From: "2024-06-01",
			To:   "2024-08-01",
		})
		if err != nil {
			b.Fatalf("GetDailyUsage: %v", err)
		}
	}
}

func TestGetDailyUsage_PricingPrecedence(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern: "db-only-model",
			InputPerMTok: 1.0, OutputPerMTok: 4.0,
		},
		{
			ModelPattern: "custom-overrides-model",
			InputPerMTok: 1.0, OutputPerMTok: 4.0,
		},
		{
			ModelPattern: "db-model",
			InputPerMTok: 3.0, OutputPerMTok: 10.0,
		},
	}), "UpsertModelPricing")
	d.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-overrides-model": {Input: 2.0, Output: 8.0},
		"my-custom-model":        {Input: 1.5, Output: 6.0},
		"other-model":            {Input: 99.0, Output: 99.0},
	})

	tests := []struct {
		name     string
		model    string
		input    int // input tokens
		output   int // output tokens
		wantCost float64
	}{
		{
			name:     "db pricing only",
			model:    "db-only-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 1.4, // 1M*$1/M + 100k*$4/M
		},
		{
			name:     "custom overrides db for same model",
			model:    "custom-overrides-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 2.8, // 1M*$2/M + 100k*$8/M
		},
		{
			name:     "custom for unknown model, no db entry",
			model:    "my-custom-model",
			input:    500_000,
			output:   50_000,
			wantCost: 1.05, // 500k*$1.5/M + 50k*$6/M
		},
		{
			name:     "no pricing at all yields zero cost",
			model:    "unknown-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 0.0,
		},
		{
			name:     "custom only affects targeted model",
			model:    "db-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 4.0, // 1M*$3/M + 100k*$10/M -- db rates, not custom
		},
	}

	for i, tt := range tests {
		sessionID := "pricing-" + strconv.Itoa(i)
		insertSession(t, d, sessionID, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     tt.model,
			TokenUsage: json.RawMessage(
				`{"input_tokens":` + strconv.Itoa(tt.input) +
					`,"output_tokens":` + strconv.Itoa(tt.output) + `}`,
			),
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := d.GetDailyUsage(ctx, UsageFilter{
				From:  "2024-06-01",
				To:    "2024-06-30",
				Model: tt.model,
			})
			requireNoError(t, err, "GetDailyUsage")

			assert.Equal(t, tt.input, result.Totals.InputTokens,
				"InputTokens")
			assert.Equal(t, tt.output, result.Totals.OutputTokens,
				"OutputTokens")
			assert.InDelta(t, tt.wantCost, result.Totals.TotalCost, 0.01,
				"TotalCost")
		})
	}
}

func seedOpusPricing(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-opus-4-6",
		InputPerMTok: 5.0, OutputPerMTok: 25.0,
		CacheCreationPerMTok: 6.25, CacheReadPerMTok: 0.5,
	}}), "UpsertModelPricing")
}

func TestGetSessionUsage_PricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	insertSession(t, d, "claude:s1", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.PeakContextTokens = 1200
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s1", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	require.True(t, u.HasCost, "HasCost = false, want true")
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD")
	assert.Equal(t, 500, u.TotalOutputTokens,
		"TotalOutputTokens want 500")
	assert.Equal(t, 1200, u.PeakContextTokens,
		"PeakContextTokens want 1200")
	assert.Equal(t, []string{"claude-opus-4-6"}, u.Models, "Models")
	assert.Empty(t, u.UnpricedModels, "UnpricedModels")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	entry := u.Breakdown[0]
	assert.Equal(t, 1, entry.Ordinal, "Breakdown ordinal")
	require.NotNil(t, entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, 0, *entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, "message", entry.Source, "Source")
	assert.Equal(t, "Prompt 1", entry.Label, "Label")
	assert.Equal(t, "2026-05-20T10:30:00Z", entry.Timestamp, "Timestamp")
	assert.Equal(t, "claude-opus-4-6", entry.Model, "Model")
	assert.Equal(t, 1000, entry.InputTokens, "InputTokens")
	assert.Equal(t, 500, entry.OutputTokens, "OutputTokens")
	assert.InDelta(t, 0.0175, entry.CostUSD, 1e-9, "entry CostUSD")
	assert.True(t, entry.HasCost, "entry HasCost")
}

func TestGetSessionUsage_UnpricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s2", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s2", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "local-llama-99",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s2", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (unpriced)")
	assert.Equal(t, 0.0, u.CostUSD, "CostUSD")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_MixedPricedUnpriced(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s3", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s3", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s3", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "local-llama-99",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:s3", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (mixed)")
	assert.Equal(t, 0.0, u.CostUSD, "CostUSD")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_ExplicitCostOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "hermes:s4", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	cost := 0.02
	require.NoError(t, d.ReplaceSessionUsageEvents("hermes:s4", []UsageEvent{{
		SessionID: "hermes:s4", Source: "session", Model: "gpt-5.4",
		InputTokens: 100, OutputTokens: 50,
		CostUSD: &cost, CostStatus: "estimated", CostSource: "hermes",
		OccurredAt: "2026-05-20T10:05:00Z", DedupKey: "session:hermes:s4",
	}}), "ReplaceSessionUsageEvents")

	u, err := d.GetSessionUsage(ctx, "hermes:s4", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.True(t, u.HasCost, "HasCost = false, want true (explicit cost)")
	assert.InDelta(t, 0.02, u.CostUSD, 1e-9, "CostUSD")
	assert.Equal(t, []string{"gpt-5.4"}, u.Models, "Models")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	entry := u.Breakdown[0]
	assert.Nil(t, entry.MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, "session", entry.Source, "Source")
	assert.Equal(t, "session", entry.Label, "Label")
	assert.Equal(t, 100, entry.InputTokens, "InputTokens")
	assert.Equal(t, 50, entry.OutputTokens, "OutputTokens")
	assert.True(t, entry.HasCost, "entry HasCost")
	assert.InDelta(t, 0.02, entry.CostUSD, 1e-9, "entry CostUSD")
}

func TestGetSessionUsage_BreakdownOrderingAndBuckets(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s-breakdown", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 650
		s.PeakContextTokens = 1500
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s-breakdown", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":100,"output_tokens":50,` +
					`"cache_creation_input_tokens":10,` +
					`"cache_read_input_tokens":20}`),
		},
		Message{
			SessionID: "claude:s-breakdown", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":200,"output_tokens":60,` +
					`"cache_creation_input_tokens":0,` +
					`"cache_read_input_tokens":40}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:s-breakdown", true)
	requireNoError(t, err, "GetSessionUsage")
	require.Len(t, u.Breakdown, 2, "Breakdown")
	assert.Equal(t, 650, u.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1500, u.PeakContextTokens, "PeakContextTokens")

	first := u.Breakdown[0]
	require.NotNil(t, first.MessageOrdinal, "first MessageOrdinal")
	assert.Equal(t, 0, *first.MessageOrdinal, "first MessageOrdinal")
	assert.Equal(t, "Prompt 1", first.Label, "first Label")
	assert.Equal(t, 200, first.InputTokens, "first InputTokens")
	assert.Equal(t, 60, first.OutputTokens, "first OutputTokens")
	assert.Equal(t, 0, first.CacheCreationInputTokens, "first CacheCreationInputTokens")
	assert.Equal(t, 40, first.CacheReadInputTokens, "first CacheReadInputTokens")

	second := u.Breakdown[1]
	require.NotNil(t, second.MessageOrdinal, "second MessageOrdinal")
	assert.Equal(t, 1, *second.MessageOrdinal, "second MessageOrdinal")
	assert.Equal(t, "Prompt 2", second.Label, "second Label")
	assert.Equal(t, 100, second.InputTokens, "second InputTokens")
	assert.Equal(t, 50, second.OutputTokens, "second OutputTokens")
	assert.Equal(t, 10, second.CacheCreationInputTokens, "second CacheCreationInputTokens")
	assert.Equal(t, 20, second.CacheReadInputTokens, "second CacheReadInputTokens")

	breakdownCost := first.CostUSD + second.CostUSD
	assert.InDelta(t, breakdownCost, u.CostUSD, 1e-9,
		"breakdown cost should sum to total cost")
}

func TestGetSessionUsage_DedupesDuplicateClaudeRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s6", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	// Two rows sharing the same claude message+request id (a
	// fork/replay) must be counted once, not doubled.
	insertMessages(t, d,
		Message{
			SessionID: "claude:s6", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s6", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)
	u, err := d.GetSessionUsage(ctx, "claude:s6", true)
	requireNoError(t, err, "GetSessionUsage")
	// One row priced at 1000*5/1e6 + 500*25/1e6 = 0.0175; deduped, not 0.035.
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD want 0.0175 (deduped)")
	assert.True(t, u.HasCost, "HasCost = false, want true")
	require.Len(t, u.Breakdown, 1, "Breakdown")
	require.NotNil(t, u.Breakdown[0].MessageOrdinal, "MessageOrdinal")
	assert.Equal(t, 0, *u.Breakdown[0].MessageOrdinal, "MessageOrdinal")
}

func TestGetSessionUsage_DedupesBySourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s7", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s7", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", SourceUUID: "source-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s7", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", SourceUUID: "source-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)
	u, err := d.GetSessionUsage(ctx, "claude:s7", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD want 0.0175 (deduped)")
	assert.True(t, u.HasCost, "HasCost = false, want true")
}

func TestGetSessionUsage_NoTokenRowsKeepsMetadata(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s5", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 700
		s.PeakContextTokens = 3000
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})

	u, err := d.GetSessionUsage(ctx, "claude:s5", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.Equal(t, 700, u.TotalOutputTokens,
		"TotalOutputTokens want 700")
	assert.Equal(t, 3000, u.PeakContextTokens,
		"PeakContextTokens want 3000")
	assert.True(t, u.HasTokenData, "HasTokenData = false, want true")
	assert.False(t, u.HasCost, "HasCost = true, want false (no cost rows)")
	assert.NotNil(t, u.Models, "Models = nil, want non-nil empty slice")
	assert.Empty(t, u.Breakdown, "Breakdown")
}

func TestGetSessionUsage_NotFound(t *testing.T) {
	d := testDB(t)
	u, err := d.GetSessionUsage(context.Background(), "nope:x", true)
	requireNoError(t, err, "GetSessionUsage")
	assert.Nil(t, u, "usage")
}

func TestGetSessionUsage_AICreditsCapability(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("ai-credit-agent"),
		DisplayName: "AI Credit Agent",
		Usage: parser.UsageCapabilities{
			AICreditsDenominated: true,
		},
	})

	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	insertSession(t, d, "ai-credit-agent:s1", "proj", func(s *Session) {
		s.Agent = "ai-credit-agent"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "ai-credit-agent:s1",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-20T10:30:00Z",
		Model:     "claude-opus-4-6",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "ai-credit-agent:s1", true)
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.True(t, u.HasCost, "HasCost = false, want true")
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD")
	assert.InDelta(t, 1.75, u.AICredits, 1e-9, "AICredits")
}

func TestCopilotReportedCostSuppressesSessionEstimates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	for _, id := range []string{"copilot:reported", "copilot:fallback"} {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "copilot"
			s.StartedAt = new("2026-05-20T10:00:00Z")
		})
	}
	reportedCost := 0.0275
	require.NoError(t, d.ReplaceSessionUsageEvents("copilot:reported", []UsageEvent{
		{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			OccurredAt: "2026-05-20T10:10:00Z", DedupKey: "segment-1",
		},
		{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			CostUSD: &reportedCost, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-05-21T10:20:00Z", DedupKey: "segment-2",
		},
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents("copilot:fallback", []UsageEvent{{
		Source: "shutdown", Model: "claude-opus-4-6",
		InputTokens: 1000, OutputTokens: 500,
		OccurredAt: "2026-05-20T10:30:00Z", DedupKey: "fallback",
	}}))

	session, err := d.GetSessionUsage(ctx, "copilot:reported", true)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasCost)
	assert.InDelta(t, reportedCost, session.CostUSD, 1e-12)
	assert.InDelta(t, reportedCost/0.01, session.AICredits, 1e-9)
	require.Len(t, session.Breakdown, 2)
	assert.InDelta(t, 0.01375, session.Breakdown[0].CostUSD, 1e-12)
	assert.InDelta(t, 0.01375, session.Breakdown[1].CostUSD, 1e-12)
	assert.Equal(t, session.CostUSD,
		session.Breakdown[0].CostUSD+session.Breakdown[1].CostUSD)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-21", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 2)
	assert.InDelta(t, 0.03125, daily.Daily[0].TotalCost, 1e-12)
	assert.InDelta(t, 0.01375, daily.Daily[1].TotalCost, 1e-12)
	for _, day := range daily.Daily {
		require.Len(t, day.ModelBreakdowns, 1)
		assert.Equal(t, day.TotalCost, day.ModelBreakdowns[0].Cost)
	}
	assert.InDelta(t, 0.045, daily.Totals.TotalCost, 1e-12)
	assert.InDelta(t, 4.5, daily.Totals.CopilotAICredits, 1e-9,
		"credits derive from the authoritative reported cost")
	require.NotNil(t, daily.Pricing)
	assert.Equal(t, export.CostSourceMixed, daily.Pricing.CostSource,
		"authoritative reported cost must surface in pricing provenance")
	assert.Equal(t, export.CostSourceComputed,
		daily.Pricing.Models["claude-opus-4-6"].CostSource)

	earlyDay, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.035, earlyDay.Totals.TotalCost, 1e-12)

	modelFiltered, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-20", To: "2026-05-21", Timezone: "UTC",
		Model: "claude-opus-4-6",
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.0525, modelFiltered.Totals.TotalCost, 1e-12)
	assert.InDelta(t, 5.25, modelFiltered.Totals.CopilotAICredits, 1e-9,
		"model-filtered credits track the estimated totals")
	require.NotNil(t, modelFiltered.Pricing)
	assert.Equal(t, export.CostSourceComputed, modelFiltered.Pricing.CostSource,
		"model-filtered totals stay estimated, so provenance stays computed")
}

func TestCopilotReportedZeroCostSuppressesEstimate(t *testing.T) {
	d := testDB(t)
	seedOpusPricing(t, d)
	insertSession(t, d, "copilot:reported-zero", "proj", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-05-21T10:00:00Z")
	})
	zeroCost := 0.0
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"copilot:reported-zero",
		[]UsageEvent{{
			Source: "shutdown", Model: "claude-opus-4-6",
			InputTokens: 1000, OutputTokens: 500,
			CostUSD: &zeroCost, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-05-21T10:10:00Z", DedupKey: "final",
		}},
	))

	session, err := d.GetSessionUsage(
		context.Background(), "copilot:reported-zero", false)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasCost)
	assert.Zero(t, session.CostUSD)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-05-21", To: "2026-05-21", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Zero(t, daily.Totals.TotalCost)
	require.Len(t, daily.Daily, 1)
	require.Len(t, daily.Daily[0].ModelBreakdowns, 1)
	assert.Zero(t, daily.Daily[0].ModelBreakdowns[0].Cost)
}

// TestGetDailyUsage_CopilotAICredits verifies AI credits are computed from
// agents with the parser AI-credit capability: costUSD / 0.01.
func TestGetDailyUsage_CopilotAICredits(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("ai-credit-agent"),
		DisplayName: "AI Credit Agent",
		Usage: parser.UsageCapabilities{
			AICreditsDenominated: true,
		},
	})

	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "gpt-4",
			InputPerMTok:         15.0,
			OutputPerMTok:        60.0,
			CacheCreationPerMTok: 15.0,
			CacheReadPerMTok:     6.0,
		},
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}))

	tests := []struct {
		name        string
		sessionID   string
		agent       string
		model       string
		inputRate   float64
		outputRate  float64
		wantCredits bool
	}{
		{
			name:        "copilot credits computed",
			sessionID:   "copilot:aicredits",
			agent:       "copilot",
			model:       "gpt-4",
			inputRate:   15.0,
			outputRate:  60.0,
			wantCredits: true,
		},
		{
			name:        "non copilot capability credits computed",
			sessionID:   "ai-credit-agent:aicredits",
			agent:       "ai-credit-agent",
			model:       "gpt-4",
			inputRate:   15.0,
			outputRate:  60.0,
			wantCredits: true,
		},
		{
			name:       "non copilot has no credits",
			sessionID:  "claude:nocredits",
			agent:      "claude-code",
			model:      "claude-opus-4-6",
			inputRate:  3.0,
			outputRate: 15.0,
		},
	}

	for _, tt := range tests {
		insertSession(t, d, tt.sessionID, "proj", func(s *Session) {
			s.Agent = tt.agent
			s.StartedAt = new("2024-06-15T10:00:00Z")
			s.EndedAt = new("2024-06-15T11:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: tt.sessionID,
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     tt.model,
			TokenUsage: json.RawMessage(`{
				"input_tokens": 1000,
				"output_tokens": 500
			}`),
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := d.GetDailyUsage(ctx, UsageFilter{
				From:  "2024-06-01",
				To:    "2024-06-30",
				Agent: tt.agent,
			})
			requireNoError(t, err, "GetDailyUsage")

			wantCost := (1000*tt.inputRate + 500*tt.outputRate) / 1_000_000
			wantCredits := 0.0
			if tt.wantCredits {
				wantCredits = wantCost / 0.01
			}
			assert.InDelta(t, wantCost, result.Totals.TotalCost, 1e-9,
				"TotalCost")
			assert.InDelta(t, wantCredits, result.Totals.CopilotAICredits,
				1e-6, "CopilotAICredits")
		})
	}
}

func TestAICreditsFromCost(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		cost  float64
		want  float64
	}{
		{"copilot converts at a cent per credit", "copilot", 0.42, 42},
		{"zero cost yields zero credits", "copilot", 0, 0},
		{"non-credit agent yields zero", "claude", 3.5, 0},
		{"unknown agent yields zero", "unknown-agent", 3.5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want,
				AICreditsFromCost(tc.agent, tc.cost), 1e-9)
		})
	}
}
