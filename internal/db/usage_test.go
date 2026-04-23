package db

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/config"
)

func TestGetDailyUsageEmpty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-01-01",
		To:   "2024-12-31",
	})
	requireNoError(t, err, "GetDailyUsage empty")

	if result.Daily == nil {
		t.Fatal("Daily should be non-nil empty slice")
	}
	if len(result.Daily) != 0 {
		t.Errorf("got %d daily entries, want 0",
			len(result.Daily))
	}
	if result.Totals.TotalCost != 0 {
		t.Errorf("TotalCost = %v, want 0",
			result.Totals.TotalCost)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
		s.EndedAt = Ptr("2024-06-15T11:00:00Z")
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

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}

	day := result.Daily[0]
	if day.Date != "2024-06-15" {
		t.Errorf("Date = %q, want %q",
			day.Date, "2024-06-15")
	}
	if day.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000",
			day.InputTokens)
	}
	if day.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500",
			day.OutputTokens)
	}
	if day.CacheCreationTokens != 200 {
		t.Errorf("CacheCreationTokens = %d, want 200",
			day.CacheCreationTokens)
	}
	if day.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300",
			day.CacheReadTokens)
	}

	// Cost = (1000*3.0 + 500*15.0 + 200*3.75 + 300*0.30) / 1_000_000
	//      = (3000 + 7500 + 750 + 90) / 1_000_000
	//      = 11340 / 1_000_000
	//      = 0.01134
	wantCost := 0.01134
	if math.Abs(day.TotalCost-wantCost) > 1e-9 {
		t.Errorf("TotalCost = %v, want %v",
			day.TotalCost, wantCost)
	}

	if len(day.ModelsUsed) != 1 ||
		day.ModelsUsed[0] != "claude-sonnet-4-20250514" {
		t.Errorf("ModelsUsed = %v, want [claude-sonnet-4-20250514]",
			day.ModelsUsed)
	}

	// Totals should match single day
	if result.Totals.InputTokens != 1000 {
		t.Errorf("Totals.InputTokens = %d, want 1000",
			result.Totals.InputTokens)
	}
	if math.Abs(result.Totals.TotalCost-wantCost) > 1e-9 {
		t.Errorf("Totals.TotalCost = %v, want %v",
			result.Totals.TotalCost, wantCost)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-opus", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-opus-4-6", TokenUsage: tokens,
	})

	insertSession(t, d, "s-sonnet", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:05:00Z")
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
	if math.Abs(
		result.Totals.CacheSavings-wantSavings,
	) > 1e-9 {
		t.Errorf(
			"Totals.CacheSavings = %v, want %v",
			result.Totals.CacheSavings, wantSavings,
		)
	}

	// Falsification: if the code had used Sonnet rates for
	// both rows the total would be 2 * 1.95 = 3.90, which
	// differs from wantSavings by >$7. Assert we're nowhere
	// near that value so a regression to a single-rate path
	// trips the test.
	if math.Abs(result.Totals.CacheSavings-3.90) < 0.1 {
		t.Errorf(
			"CacheSavings = %v looks like single-rate path; "+
				"expected per-model math",
			result.Totals.CacheSavings,
		)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}

	day := result.Daily[0]
	if day.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000 (claude only)",
			day.InputTokens)
	}
	if day.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500 (claude only)",
			day.OutputTokens)
	}
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
		s.StartedAt = Ptr("2024-06-10T08:00:00Z")
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
		s.StartedAt = Ptr("2024-06-11T08:00:00Z")
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

	if len(result.Daily) != 2 {
		t.Fatalf("got %d daily entries, want 2",
			len(result.Daily))
	}

	// Day 1: check totals
	d1 := result.Daily[0]
	if d1.Date != "2024-06-10" {
		t.Errorf("day1 Date = %q, want 2024-06-10", d1.Date)
	}
	if d1.InputTokens != 300 {
		t.Errorf("day1 InputTokens = %d, want 300",
			d1.InputTokens)
	}
	if d1.OutputTokens != 150 {
		t.Errorf("day1 OutputTokens = %d, want 150",
			d1.OutputTokens)
	}
	if len(d1.ModelsUsed) != 2 {
		t.Errorf("day1 ModelsUsed count = %d, want 2",
			len(d1.ModelsUsed))
	}

	// Day 2
	d2 := result.Daily[1]
	if d2.Date != "2024-06-11" {
		t.Errorf("day2 Date = %q, want 2024-06-11", d2.Date)
	}
	if d2.InputTokens != 300 {
		t.Errorf("day2 InputTokens = %d, want 300",
			d2.InputTokens)
	}

	// Totals should sum both days
	wantTotalInput := 600
	if result.Totals.InputTokens != wantTotalInput {
		t.Errorf("Totals.InputTokens = %d, want %d",
			result.Totals.InputTokens, wantTotalInput)
	}
	wantTotalOutput := 300
	if result.Totals.OutputTokens != wantTotalOutput {
		t.Errorf("Totals.OutputTokens = %d, want %d",
			result.Totals.OutputTokens, wantTotalOutput)
	}

	// Cost check: day1 model-a = (100*2+50*10)/1e6 = 0.0007
	//             day1 model-b = (200*4+100*20)/1e6 = 0.0028
	//             day2 model-a = (300*2+150*10)/1e6 = 0.0021
	//             total = 0.0056
	wantTotalCost := 0.0056
	if math.Abs(result.Totals.TotalCost-wantTotalCost) > 1e-9 {
		t.Errorf("Totals.TotalCost = %v, want %v",
			result.Totals.TotalCost, wantTotalCost)
	}
}

func TestGetDailyUsageNoPricing(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess1",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "unknown-model",
		TokenUsage: json.RawMessage(`{"input_tokens":500,"output_tokens":250}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage no pricing")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}

	day := result.Daily[0]
	if day.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500",
			day.InputTokens)
	}
	if day.OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250",
			day.OutputTokens)
	}
	if day.TotalCost != 0 {
		t.Errorf("TotalCost = %v, want 0 (no pricing)",
			day.TotalCost)
	}
	if len(day.ModelsUsed) != 1 ||
		day.ModelsUsed[0] != "unknown-model" {
		t.Errorf("ModelsUsed = %v, want [unknown-model]",
			day.ModelsUsed)
	}
}

// TestGetDailyUsageTruncatedTokenJSON documents what happens when
// a message lands in the DB with truncated token_usage — gjson is
// permissive and still extracts the leading fields, so the valid
// data is preserved. This is why we don't run gjson.Valid on the
// hot aggregation path: the realistic corruption modes reachable
// from our parsers don't produce silent zeros.
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
			// Truncated mid-key. gjson still finds the two
			// leading numeric fields and extracts them.
			TokenUsage: json.RawMessage(
				`{"input_tokens":9999,"output_tokens":4242,"ca`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage truncated")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
	day := result.Daily[0]
	// 1000 (valid row) + 9999 (truncated but still parseable)
	if day.InputTokens != 10999 {
		t.Errorf("InputTokens = %d, want 10999 "+
			"(gjson should extract leading fields from truncated JSON)",
			day.InputTokens)
	}
	if day.OutputTokens != 4742 {
		t.Errorf("OutputTokens = %d, want 4742", day.OutputTokens)
	}
}

func TestGetDailyUsage_DedupesByClaudeMessageAndRequestID(t *testing.T) {
	d := testDB(t)
	if err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheCreationPerMTok: 18.75,
		CacheReadPerMTok:     1.50,
	}}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.getWriter().Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
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
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if len(result.Daily) != 1 {
		t.Fatalf("daily entries = %d, want 1", len(result.Daily))
	}
	day := result.Daily[0]
	if day.InputTokens != 120 {
		t.Errorf("input = %d, want 120", day.InputTokens)
	}
	if day.OutputTokens != 580 {
		t.Errorf("output = %d, want 580", day.OutputTokens)
	}
	if day.CacheCreationTokens != 1200 {
		t.Errorf("cache_cr = %d, want 1200", day.CacheCreationTokens)
	}
	if day.CacheReadTokens != 55000 {
		t.Errorf("cache_rd = %d, want 55000", day.CacheReadTokens)
	}
}

func TestGetDailyUsage_MissingDedupKeysCountedEveryTime(t *testing.T) {
	d := testDB(t)
	if err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-opus-4-6",
		OutputPerMTok: 75.0,
	}}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.getWriter().Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	          VALUES ('s1', 'proj', 'local', 'claude', ?, ?)`,
		"2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")

	usage := `{"input_tokens":0,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
	for _, ord := range []int{0, 1} {
		mustExec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 model, token_usage,
			 claude_message_id, claude_request_id,
			 has_output_tokens)
			VALUES ('s1', ?, 'assistant', '', '2026-04-10T10:02:00Z',
			        'claude-opus-4-6', ?, '', '', 1)`, ord, usage)
	}

	result, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if len(result.Daily) != 1 || result.Daily[0].OutputTokens != 20 {
		t.Errorf("output = %v, want 20 (both no-key rows counted)", result.Daily)
	}
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
		StartedAt: Ptr("2026-04-01T10:00:00Z"),
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

	if len(result.Daily) != 1 {
		t.Fatalf("expected 1 day, got %d", len(result.Daily))
	}
	if result.Daily[0].InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000",
			result.Daily[0].InputTokens)
	}
}

func TestGetDailyUsageProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-a", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-a",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-b", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-b",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":1000}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:    "2024-06-01",
		To:      "2024-06-30",
		Project: "proj-a",
	})
	requireNoError(t, err, "GetDailyUsage project filter")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
	day := result.Daily[0]
	if day.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000 (proj-a only)",
			day.InputTokens)
	}
	if result.Totals.InputTokens != 1000 {
		t.Errorf("Totals.InputTokens = %d, want 1000",
			result.Totals.InputTokens)
	}
}

func TestGetDailyUsageModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet", InputPerMTok: 3.0,
			OutputPerMTok: 15.0},
		{ModelPattern: "gpt-5", InputPerMTok: 2.5,
			OutputPerMTok: 10.0},
	}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID:  "sess1",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":800}`),
		},
		Message{
			SessionID:  "sess1",
			Ordinal:    1,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:31:00Z",
			Model:      "gpt-5",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5",
	})
	requireNoError(t, err, "GetDailyUsage model filter")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
	day := result.Daily[0]
	if day.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000 (gpt-5 only)",
			day.InputTokens)
	}
	if len(day.ModelsUsed) != 1 || day.ModelsUsed[0] != "gpt-5" {
		t.Errorf("ModelsUsed = %v, want [gpt-5]",
			day.ModelsUsed)
	}
}

func TestGetDailyUsageProjectBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-a", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-a",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-b", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-b",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage project breakdowns")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
	day := result.Daily[0]
	if len(day.ProjectBreakdowns) != 2 {
		t.Fatalf("ProjectBreakdowns len = %d, want 2",
			len(day.ProjectBreakdowns))
	}

	projMap := make(map[string]ProjectBreakdown)
	var projCostSum float64
	for _, pb := range day.ProjectBreakdowns {
		projMap[pb.Project] = pb
		projCostSum += pb.Cost
	}
	for _, name := range []string{"proj-a", "proj-b"} {
		pb, ok := projMap[name]
		if !ok {
			t.Errorf("missing ProjectBreakdown for %s", name)
			continue
		}
		if pb.InputTokens != 1000 {
			t.Errorf("%s InputTokens = %d, want 1000",
				name, pb.InputTokens)
		}
	}
	if math.Abs(projCostSum-day.TotalCost) > 1e-9 {
		t.Errorf("sum(ProjectBreakdowns.Cost) = %v, "+
			"want TotalCost = %v", projCostSum, day.TotalCost)
	}
}

func TestGetDailyUsageAgentBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-claude", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-claude",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-codex", "proj1", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-codex",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage agent breakdowns")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
	day := result.Daily[0]
	if len(day.AgentBreakdowns) != 2 {
		t.Fatalf("AgentBreakdowns len = %d, want 2",
			len(day.AgentBreakdowns))
	}

	agentMap := make(map[string]AgentBreakdown)
	var agentCostSum float64
	for _, ab := range day.AgentBreakdowns {
		agentMap[ab.Agent] = ab
		agentCostSum += ab.Cost
	}
	for _, name := range []string{"claude", "codex"} {
		ab, ok := agentMap[name]
		if !ok {
			t.Errorf("missing AgentBreakdown for %s", name)
			continue
		}
		if ab.InputTokens != 1000 {
			t.Errorf("%s InputTokens = %d, want 1000",
				name, ab.InputTokens)
		}
	}
	if math.Abs(agentCostSum-day.TotalCost) > 1e-9 {
		t.Errorf("sum(AgentBreakdowns.Cost) = %v, "+
			"want TotalCost = %v", agentCostSum, day.TotalCost)
	}
}

func TestGetDailyUsageBreakdownInvariant(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", InputPerMTok: 2.0,
			OutputPerMTok: 10.0},
		{ModelPattern: "model-b", InputPerMTok: 4.0,
			OutputPerMTok: 20.0},
	}), "UpsertModelPricing")

	// 2 projects x 2 agents = 4 sessions, each with 2 messages
	// from different models.
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
		sid := "sess-" + strconv.Itoa(i)
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
				Model:      "model-b",
				TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
			},
		)
	}

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage breakdown invariant")

	if len(result.Daily) != 1 {
		t.Fatalf("got %d daily entries, want 1",
			len(result.Daily))
	}
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

	if math.Abs(modelCostSum-day.TotalCost) > 1e-9 {
		t.Errorf("sum(ModelBreakdowns.Cost) = %v, "+
			"want TotalCost = %v", modelCostSum, day.TotalCost)
	}
	if math.Abs(projectCostSum-day.TotalCost) > 1e-9 {
		t.Errorf("sum(ProjectBreakdowns.Cost) = %v, "+
			"want TotalCost = %v", projectCostSum, day.TotalCost)
	}
	if math.Abs(agentCostSum-day.TotalCost) > 1e-9 {
		t.Errorf("sum(AgentBreakdowns.Cost) = %v, "+
			"want TotalCost = %v", agentCostSum, day.TotalCost)
	}
	if math.Abs(modelCostSum-projectCostSum) > 1e-9 {
		t.Errorf("model cost sum %v != project cost sum %v",
			modelCostSum, projectCostSum)
	}
	if math.Abs(modelCostSum-agentCostSum) > 1e-9 {
		t.Errorf("model cost sum %v != agent cost sum %v",
			modelCostSum, agentCostSum)
	}
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
		s.DisplayName = Ptr("Big Session")
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.DisplayName = Ptr("Small Session")
		s.StartedAt = Ptr("2024-06-15T11:00:00Z")
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

	if len(top) != 2 {
		t.Fatalf("got %d entries, want 2", len(top))
	}

	// Ordered cost desc — sBig first
	if top[0].SessionID != "sBig" {
		t.Errorf("top[0].SessionID = %q, want sBig",
			top[0].SessionID)
	}
	if top[0].DisplayName != "Big Session" {
		t.Errorf("top[0].DisplayName = %q, want Big Session",
			top[0].DisplayName)
	}
	if top[0].Project != "proj-a" {
		t.Errorf("top[0].Project = %q, want proj-a",
			top[0].Project)
	}
	if top[0].Agent != "claude" {
		t.Errorf("top[0].Agent = %q, want claude",
			top[0].Agent)
	}
	// TotalTokens = 5000 + 2000 + 1000 + 3000 = 11000
	if top[0].TotalTokens != 11000 {
		t.Errorf("top[0].TotalTokens = %d, want 11000",
			top[0].TotalTokens)
	}
	if top[0].Cost <= 0 {
		t.Errorf("top[0].Cost = %v, want > 0", top[0].Cost)
	}

	if top[1].SessionID != "sSmall" {
		t.Errorf("top[1].SessionID = %q, want sSmall",
			top[1].SessionID)
	}
	if top[0].Cost <= top[1].Cost {
		t.Errorf("top[0].Cost (%v) should be > top[1].Cost (%v)",
			top[0].Cost, top[1].Cost)
	}
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

	// Session with display_name set — should use display_name.
	insertSession(t, d, "s-dn", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.DisplayName = Ptr("My Custom Name")
		s.FirstMessage = Ptr("some first message")
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.FirstMessage = Ptr("fix the login bug")
		s.StartedAt = Ptr("2024-06-15T11:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T12:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T13:00:00Z")
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

	if len(top) != 4 {
		t.Fatalf("got %d entries, want 4", len(top))
	}

	// Build a map for easy lookup (order is by cost, all equal
	// here so secondary sort is by session ID).
	byID := make(map[string]TopSessionEntry)
	for _, e := range top {
		byID[e.SessionID] = e
	}

	if got := byID["s-dn"].DisplayName; got != "My Custom Name" {
		t.Errorf("s-dn DisplayName = %q, want %q",
			got, "My Custom Name")
	}
	if got := byID["s-fm"].DisplayName; got != "fix the login bug" {
		t.Errorf("s-fm DisplayName = %q, want %q",
			got, "fix the login bug")
	}
	if got := byID["s-proj"].DisplayName; got != "my-project" {
		t.Errorf("s-proj DisplayName = %q, want %q",
			got, "my-project")
	}
	if got := byID["s-id"].DisplayName; got != "s-id" {
		t.Errorf("s-id DisplayName = %q, want %q",
			got, "s-id")
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	// Forked session starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:01:00Z")
		s.ParentSessionID = Ptr("s-parent")
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

	if len(top) != 2 {
		t.Fatalf("got %d entries, want 2", len(top))
	}

	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	if !ok {
		t.Fatal("s-parent missing from top sessions")
	}
	// Parent owns shared: 1000+500+200+3000 = 4700 tokens.
	if parent.TotalTokens != 4700 {
		t.Errorf("parent.TotalTokens = %d, want 4700",
			parent.TotalTokens)
	}

	fork, ok := byID["s-fork"]
	if !ok {
		t.Fatal("s-fork missing from top sessions")
	}
	// Fork should only own the unique message: 10+20 = 30
	// tokens. If the dedup were missing, the shared row would
	// be counted again and this would jump to 4730.
	if fork.TotalTokens != 30 {
		t.Errorf("fork.TotalTokens = %d, want 30 "+
			"(shared message should be deduped)",
			fork.TotalTokens)
	}

	// Total across both entries must equal the undeduped
	// message sum: parent 4700 + fork 30 = 4730.
	total := parent.TotalTokens + fork.TotalTokens
	if total != 4730 {
		t.Errorf("sum of per-session totals = %d, want 4730",
			total)
	}
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
			s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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

	if len(top) != 3 {
		t.Fatalf("got %d entries, want 3", len(top))
	}
}

func TestGetUsageSessionCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// s1: proj-a / claude — TWO messages across TWO days
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T11:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T12:00:00Z")
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

	if counts.Total != 3 {
		t.Errorf("Total = %d, want 3", counts.Total)
	}
	if counts.ByProject["proj-a"] != 2 {
		t.Errorf("ByProject[proj-a] = %d, want 2",
			counts.ByProject["proj-a"])
	}
	if counts.ByProject["proj-b"] != 1 {
		t.Errorf("ByProject[proj-b] = %d, want 1",
			counts.ByProject["proj-b"])
	}
	if counts.ByAgent["claude"] != 2 {
		t.Errorf("ByAgent[claude] = %d, want 2",
			counts.ByAgent["claude"])
	}
	if counts.ByAgent["codex"] != 1 {
		t.Errorf("ByAgent[codex] = %d, want 1",
			counts.ByAgent["codex"])
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	// Fork starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:01:00Z")
		s.ParentSessionID = Ptr("s-parent")
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

	if counts.Total != 1 {
		t.Errorf(
			"Total = %d, want 1 (fork should dedup out)",
			counts.Total,
		)
	}
	if counts.ByProject["proj"] != 1 {
		t.Errorf(
			"ByProject[proj] = %d, want 1",
			counts.ByProject["proj"],
		)
	}
	if counts.ByAgent["claude"] != 1 {
		t.Errorf(
			"ByAgent[claude] = %d, want 1",
			counts.ByAgent["claude"],
		)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("GetDailyUsage InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}

	// GetUsageSessionCounts
	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts parity")
	if counts.Total != 1 {
		t.Errorf("GetUsageSessionCounts Total = %d, want 1",
			counts.Total)
	}

	// GetTopSessionsByCost
	top, err := d.GetTopSessionsByCost(ctx, filter, 20)
	requireNoError(t, err, "GetTopSessionsByCost parity")
	if len(top) != 1 {
		t.Fatalf("GetTopSessionsByCost len = %d, want 1",
			len(top))
	}
	if top[0].SessionID != "good" {
		t.Errorf("GetTopSessionsByCost[0].SessionID = %q, "+
			"want good", top[0].SessionID)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sB", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sC", "proj-c", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
	if daily.Totals.InputTokens != 2000 {
		t.Errorf("exclude proj-b: InputTokens = %d, want 2000",
			daily.Totals.InputTokens)
	}

	// Exclude two projects (comma-separated).
	f2 := base
	f2.ExcludeProject = "proj-a,proj-c"
	daily, err = d.GetDailyUsage(ctx, f2)
	requireNoError(t, err, "GetDailyUsage exclude two")
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("exclude a+c: InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}

	// GetTopSessionsByCost with exclude.
	top, err := d.GetTopSessionsByCost(ctx, f1, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude")
	if len(top) != 2 {
		t.Fatalf("exclude proj-b: top len = %d, want 2", len(top))
	}
	for _, ts := range top {
		if ts.Project == "proj-b" {
			t.Errorf("excluded proj-b still in top sessions")
		}
	}

	// GetUsageSessionCounts with exclude.
	counts, err := d.GetUsageSessionCounts(ctx, f1)
	requireNoError(t, err, "GetUsageSessionCounts exclude")
	if counts.Total != 2 {
		t.Errorf("exclude proj-b: Total = %d, want 2", counts.Total)
	}
	if counts.ByProject["proj-b"] != 0 {
		t.Errorf("excluded proj-b count = %d, want 0",
			counts.ByProject["proj-b"])
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("exclude codex: InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}
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
		s.StartedAt = Ptr("2024-06-15T10:00:00Z")
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
	if daily.Totals.InputTokens != 1000 {
		t.Errorf("exclude opus: InputTokens = %d, want 1000",
			daily.Totals.InputTokens)
	}
	if len(daily.Daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(daily.Daily))
	}
	for _, mb := range daily.Daily[0].ModelBreakdowns {
		if mb.ModelName == "opus" {
			t.Errorf("excluded model opus still in breakdowns")
		}
	}
}

func BenchmarkGetDailyUsage(b *testing.B) {
	d := testDB(&testing.T{})
	ctx := context.Background()

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
			StartedAt:    Ptr(startTime.Format(time.RFC3339)),
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
	tests := []struct {
		name     string
		dbRates  []ModelPricing
		custom   map[string]config.CustomModelRate
		model    string
		input    int // input tokens
		output   int // output tokens
		wantCost float64
	}{
		{
			name:     "db pricing only",
			dbRates:  []ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0, OutputPerMTok: 4.0}},
			model:    "acme-ultra-2.1",
			input:    1_000_000,
			output:   100_000,
			wantCost: 1.4, // 1M*$1/M + 100k*$4/M
		},
		{
			name:    "custom overrides db for same model",
			dbRates: []ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0, OutputPerMTok: 4.0}},
			custom:  map[string]config.CustomModelRate{"acme-ultra-2.1": {Input: 2.0, Output: 8.0}},
			model:   "acme-ultra-2.1",
			input:   1_000_000,
			output:  100_000,
			wantCost: 2.8, // 1M*$2/M + 100k*$8/M
		},
		{
			name:    "custom for unknown model, no db entry",
			custom:  map[string]config.CustomModelRate{"my-custom-model": {Input: 1.5, Output: 6.0}},
			model:   "my-custom-model",
			input:   500_000,
			output:  50_000,
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
			name:    "custom only affects targeted model",
			dbRates: []ModelPricing{{ModelPattern: "db-model", InputPerMTok: 3.0, OutputPerMTok: 10.0}},
			custom:  map[string]config.CustomModelRate{"other-model": {Input: 99.0, Output: 99.0}},
			model:   "db-model",
			input:   1_000_000,
			output:  100_000,
			wantCost: 4.0, // 1M*$3/M + 100k*$10/M -- db rates, not custom
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			if len(tt.dbRates) > 0 {
				requireNoError(t, d.UpsertModelPricing(tt.dbRates), "UpsertModelPricing")
			}
			if tt.custom != nil {
				d.SetCustomPricing(tt.custom)
			}

			insertSession(t, d, "s1", "proj", func(s *Session) {
				s.StartedAt = Ptr("2024-06-15T10:00:00Z")
			})
			insertMessages(t, d, Message{
				SessionID:  "s1",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:30:00Z",
				Model:      tt.model,
				TokenUsage: json.RawMessage(`{"input_tokens":` + strconv.Itoa(tt.input) + `,"output_tokens":` + strconv.Itoa(tt.output) + `}`),
			})

			result, err := d.GetDailyUsage(context.Background(), UsageFilter{
				From: "2024-06-01", To: "2024-06-30",
			})
			requireNoError(t, err, "GetDailyUsage")

			if math.Abs(result.Totals.TotalCost-tt.wantCost) > 0.01 {
				t.Errorf("TotalCost = %.4f, want %.4f", result.Totals.TotalCost, tt.wantCost)
			}
		})
	}
}
