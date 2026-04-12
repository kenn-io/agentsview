package db

import (
	"context"
	"encoding/json"
	"math"
	"testing"
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
