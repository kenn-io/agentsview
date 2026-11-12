//go:build fts5

package db

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

func checkUsageCacheFormatRecords(files fs.FS, version int, digest string) error {
	entries, err := fs.Glob(files, "*-*.txt")
	if err != nil {
		return err
	}
	highest := version
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry, "-")
		n, err := strconv.Atoi(prefix)
		if err == nil && n > highest {
			highest = n
		}
	}
	if highest > version {
		return fmt.Errorf("usage cache format %d has a newer record at format %d; bump to format %d", version, highest, highest+1)
	}
	current, err := fs.Glob(files, fmt.Sprintf("%d-*.txt", version))
	if err != nil {
		return err
	}
	if len(current) == 0 {
		return fmt.Errorf("record digest %s for usage cache format %d", digest, version)
	}
	if len(current) > 1 || current[0] != fmt.Sprintf("%d-%s.txt", version, digest) {
		return fmt.Errorf("usage cache format %d output changed or has multiple records (digest %s); bump to format %d and record the new digest", version, digest, version+1)
	}
	return nil
}

func TestUsageCacheFormat(t *testing.T) {
	setUsageLocalLocation(t, time.UTC)
	database := testDB(t)
	require.NoError(t, database.SetArchiveIdentityForTest(t.Context(), "usage-format", strings.Repeat("0", 63)+"1"))
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "format-model", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}, CacheCreationPerMTok: money.Money{Microdollars: 1_250_000}, CacheCreation1hPerMTok: money.Money{Microdollars: 3_000_000}, CacheReadPerMTok: money.Money{Microdollars: 100_000}, Bands: []PricingBand{{AboveInputTokens: 100, InputPerMTok: money.Money{Microdollars: 2_000_000}, OutputPerMTok: money.Money{Microdollars: 3_000_000}, CacheCreationPerMTok: money.Money{Microdollars: 2_500_000}, CacheCreation1hPerMTok: money.Money{Microdollars: 5_000_000}, CacheReadPerMTok: money.Money{Microdollars: 200_000}}}}}))
	_, err := database.getWriter().Exec(t.Context(), `UPDATE model_pricing SET updated_at='2026-08-01T00:00:00Z' WHERE model_pattern='format-model'`)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(t.Context(), `UPDATE model_pricing_bands SET updated_at='2026-08-01T00:00:00Z' WHERE model_pattern='format-model'`)
	require.NoError(t, err)
	database.SetEffectivePricing(map[string]export.ModelRates{
		"format-posit":             {InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}, Source: export.PricingRowSourceFetched},
		"format-openai":            {InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}, Source: export.PricingRowSourceFetched},
		"format-openai@priority":   {InputPerMTok: money.Money{Microdollars: 2_000_000}, OutputPerMTok: money.Money{Microdollars: 4_000_000}, Source: export.PricingRowSourceFetched},
		pricing.GPT56LunaCanonical: {InputPerMTok: money.Money{Microdollars: 1_500_000}, OutputPerMTok: money.Money{Microdollars: 3_000_000}, Source: export.PricingRowSourceFetched},
	})
	started := "2026-08-10T08:00:00Z"
	for _, session := range []struct{ id, project, agent string }{{"computed", "project-a", "claude"}, {"duplicate", "project-b", "claude"}, {"reported", "project-c", "copilot"}, {"activity", "activity", "copilot"}, {"posit", "project-posit", "posit-assistant"}, {"priority", "project-priority", "codex"}, {"openai", "project-openai", "codex"}, {"alias", "project-alias", "codex"}, {"recorded", "project-recorded", "opencode"}, {"recorded-priced", "project-recorded-priced", "opencode"}} {
		insertSession(t, database, session.id, session.project, func(s *Session) { s.Agent = session.agent; s.StartedAt = &started })
	}
	require.NoError(t, database.InsertMessages(t.Context(), []Message{
		{SessionID: "computed", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_1h_input_tokens":40},"cache_read_input_tokens":30,"server_tool_use":{"web_search_requests":2}}`), ClaudeMessageID: "first", ClaudeRequestID: "request"},
		{SessionID: "computed", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T10:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":50,"output_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "duplicate", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T10:01:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":50,"output_tokens":15}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "computed", Ordinal: 2, Role: "assistant", Timestamp: "2026-08-10T10:02:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_1h_input_tokens":20}}`), ClaudeMessageID: "no-request"},
		{SessionID: "computed", Ordinal: 3, Role: "assistant", Timestamp: "2026-08-10T10:03:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":10,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_1h_input_tokens":30}}`), ClaudeMessageID: "no-request"},
		{SessionID: "alias", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: pricing.GPTReserveModelName, TokenUsage: []byte(`{"input_tokens":1000000,"output_tokens":500000}`)},
		{SessionID: "recorded-priced", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":500,"recorded_cost_microdollars":9000}`)},
		{SessionID: "recorded", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "unpriced-harness-model", TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":500,"recorded_cost_microdollars":7000}`)},
		{SessionID: "activity", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T11:00:00Z"},
		{SessionID: "posit", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-posit", ProviderID: pricing.PositAssistantProviderID, TokenUsage: []byte(`{"input_tokens":1000000,"output_tokens":500000}`)},
		{SessionID: "priority", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-openai", ProviderID: pricing.OpenAIPriorityProviderID, TokenUsage: []byte(`{"input_tokens":1000000,"output_tokens":500000}`)},
		{SessionID: "openai", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-openai", ProviderID: "openai", TokenUsage: []byte(`{"input_tokens":1000000,"output_tokens":500000}`)},
	}))
	cost := money.Money{Microdollars: 123456}
	require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "reported", []UsageEvent{{Source: "shutdown", Model: "format-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: CopilotReportedCostSource, OccurredAt: "2026-08-10T09:30:00Z", DedupKey: "reported"}}))
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "format-model", InputTokens: 11, Charged: money.Money{Microdollars: 100000}, DedupKey: "cursor"}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true}
	warmJournaledUsage(t, database, filter)
	var reads []bool
	t.Cleanup(func() { database.usageCache.usageWindowObserver.afterRead = nil })
	type topEntry struct {
		SessionID string
		Cost      money.Money
	}
	type result struct {
		Daily    []DailyUsageEntry
		Totals   UsageTotals
		Top      []topEntry
		Counts   UsageSessionCounts
		Activity int
	}
	read := func() result {
		database.usageCache.usageWindowObserver.afterRead = func(journaled bool) { reads = append(reads, journaled) }
		daily, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		top, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		counts, err := database.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		database.usageCache.usageWindowObserver.afterRead = nil
		activity, err := database.GetUsageMatchingSessionCount(t.Context(), UsageFilter{From: filter.From, To: filter.To, Timezone: "UTC", Project: "activity"})
		require.NoError(t, err)
		var sessions []topEntry
		for _, entry := range top {
			sessions = append(sessions, topEntry{entry.SessionID, entry.Cost})
		}
		return result{daily.Daily, daily.Totals, sessions, counts, activity}
	}
	before := read()
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "computed", Ordinal: 4, Role: "assistant", Timestamp: "2026-08-10T12:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":75,"output_tokens":5}`), ClaudeMessageID: "append", ClaudeRequestID: "request"}}))
	after := read()
	encoded, err := json.Marshal([]result{before, after})
	require.NoError(t, err)
	digest := hexSHA256(encoded)
	require.NotEmpty(t, reads)
	for _, journaled := range reads {
		require.True(t, journaled)
	}
	require.NoError(t, checkUsageCacheFormatRecords(os.DirFS("testdata/usage-cache-format-digests"), usageCacheFormatVersion, digest))
}
