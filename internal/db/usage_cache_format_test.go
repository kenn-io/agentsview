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
		return fmt.Errorf("usage cache format %d output changed or has multiple records (digest %s); if this PR added the record, replace it; otherwise bump to format %d and record the new digest", version, digest, version+1)
	}
	return nil
}

func TestUsageCacheFormat(t *testing.T) {
	database := testDB(t)
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
	for _, session := range []struct{ id, project, agent string }{{"computed", "project-a", "claude"}, {"reasoning", "project-reasoning", "codex"}, {"duplicate", "project-b", "claude"}, {"reported", "project-c", "copilot"}, {"activity", "activity", "copilot"}, {"posit", "project-posit", "posit-assistant"}, {"priority", "project-priority", "codex"}, {"openai", "project-openai", "codex"}, {"alias", "project-alias", "codex"}, {"recorded", "project-recorded", "opencode"}, {"recorded-priced", "project-recorded-priced", "opencode"}} {
		insertSession(t, database, session.id, session.project, func(s *Session) { s.Agent = session.agent; s.StartedAt = &started })
	}
	require.NoError(t, database.InsertMessages(t.Context(), []Message{
		{SessionID: "computed", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_1h_input_tokens":40},"cache_read_input_tokens":30,"server_tool_use":{"web_search_requests":2}}`), ClaudeMessageID: "first", ClaudeRequestID: "request"},
		{SessionID: "computed", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T10:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":50,"output_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "duplicate", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T10:01:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":50,"output_tokens":15}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "computed", Ordinal: 2, Role: "assistant", Timestamp: "2026-08-10T10:02:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_1h_input_tokens":20}}`), ClaudeMessageID: "no-request"},
		{SessionID: "computed", Ordinal: 3, Role: "assistant", Timestamp: "2026-08-10T10:03:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":10,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_1h_input_tokens":30}}`), ClaudeMessageID: "no-request"},
		{SessionID: "reasoning", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T11:00:00Z", Model: "format-model", SourceUUID: "reasoning-source", TokenUsage: []byte(`{"reasoning_tokens":100}`)},
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
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Breakdowns: true}
	cache := warmJournaledUsage(t, database, filter)
	read := func() [][][]any {
		var result [][][]any
		for _, query := range []string{
			`SELECT f.cached_session_id, s.session_id, f.fact_index, f.source, f.message_ordinal, f.timestamp_ms, f.timestamp_ns,
				f.raw_timestamp, f.uses_session_start, f.model, f.provider_id, f.input_tokens, f.output_tokens,
				f.reasoning_tokens, f.cache_creation_tokens, f.cache_creation_1h_tokens, f.cache_read_tokens,
				f.web_search_requests, f.reported_cost_microdollars, f.cost_source, f.cost_status, f.request_scoped,
				f.claude_message_id, f.claude_request_id, f.source_uuid, f.usage_dedup_key, f.token_eligible, f.activity_eligible
				FROM usage_facts f JOIN usage_cached_sessions s ON s.id=f.cached_session_id ORDER BY s.session_id, f.fact_index`,
			`SELECT d.rollup_install_id, i.session_id, d.local_date, d.reported_model, d.provider_id, d.priced_model, d.matched_pattern,
				d.rate_ok, d.rate_hash, d.pricing_timestamp, d.band_threshold, d.input_tokens, d.output_tokens,
				d.reasoning_tokens, d.cache_creation_tokens, d.cache_read_tokens, d.web_search_requests,
				d.estimated_cost_microdollars, d.savings_microdollars, d.authoritative_cost_microdollars,
				d.computed_request_count, d.computed_aggregate_count, d.reported_count, d.base_request_count,
				d.discarded_snapshot_output_tokens FROM usage_daily_rollups d
				JOIN usage_rollup_installs i ON i.id=d.rollup_install_id
				JOIN usage_rollup_timezones z ON z.id=i.timezone_id WHERE z.timezone_name='UTC'
				ORDER BY i.session_id, d.local_date, d.reported_model, d.provider_id, d.rate_hash, d.band_threshold`,
		} {
			rows := usageSpoolFactValues(t, cache.db, query)
			require.NotEmpty(t, rows)
			result = append(result, rows)
		}
		return result
	}
	before := read()
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "computed", Ordinal: 4, Role: "assistant", Timestamp: "2026-08-10T12:00:00Z", Model: "format-model", TokenUsage: []byte(`{"input_tokens":75,"output_tokens":5}`), ClaudeMessageID: "append", ClaudeRequestID: "request"}}))
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	after := read()
	encoded, err := json.Marshal([][][][]any{before, after})
	require.NoError(t, err)
	digest := hexSHA256(encoded)
	require.NoError(t, checkUsageCacheFormatRecords(os.DirFS("testdata/usage-cache-format-digests"), usageCacheFormatVersion, digest))
}
