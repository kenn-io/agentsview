package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

var ccObserved = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// ccCounts is a token breakdown with no cache writes or reasoning, whose
// Total is input plus output as OpenAI reports it.
func ccCounts(input, cached, output int64) mirror.TokenCounts {
	return mirror.TokenCounts{Input: input, CachedInput: cached, Output: output, Total: input + output}
}

func ccTokens(join, turn string, total, last mirror.TokenCounts, minute int) mirror.TokenUsageData {
	return mirror.TokenUsageData{
		JoinID: join, TurnID: turn, Total: total, Last: last,
		ObservedAt: ccObserved.Add(time.Duration(minute) * time.Minute),
	}
}

func TestUsageIncrementsAndBaselines(t *testing.T) {
	recs := []mirror.TokenUsageData{
		ccTokens("join-a", "turn-1", ccCounts(1000, 200, 100), ccCounts(500, 0, 50), 0),
		ccTokens("join-a", "turn-1", ccCounts(1500, 400, 150), ccCounts(500, 200, 50), 1),
		// A reconnect repeats the total: nothing new.
		ccTokens("join-b", "turn-1", ccCounts(1500, 400, 150), ccCounts(500, 200, 50), 2),
		// The total drops: a fresh counter, whose first record adds its whole total.
		ccTokens("join-b", "turn-2", ccCounts(300, 0, 40), ccCounts(100, 0, 10), 3),
		// The same total as the second record recurs in the new baseline.
		ccTokens("join-b", "turn-2", ccCounts(1500, 400, 150), ccCounts(900, 0, 70), 4),
	}
	events, totalOutput, peakContext := codexCloudUsageEvents("thread-a", recs, "gpt-6")

	assert.Equal(t, []ParsedUsageEvent{
		{
			SessionID: "codex-cloud:thread-a", Source: "session", Model: "gpt-6",
			InputTokens: 800, CacheReadInputTokens: 200, OutputTokens: 100,
			OccurredAt: "2026-10-01T12:00:00Z", DedupKey: "thread-a:0:1100/1000/200/0/100/0",
		},
		{
			SessionID: "codex-cloud:thread-a", Source: usagefacts.CodexCloudRequestSource, Model: "gpt-6",
			InputTokens: 300, CacheReadInputTokens: 200, OutputTokens: 50,
			OccurredAt: "2026-10-01T12:01:00Z", DedupKey: "thread-a:0:1650/1500/400/0/150/0",
		},
		{
			SessionID: "codex-cloud:thread-a", Source: "session", Model: "gpt-6",
			InputTokens: 300, OutputTokens: 40,
			OccurredAt: "2026-10-01T12:03:00Z", DedupKey: "thread-a:1:340/300/0/0/40/0",
		},
		{
			SessionID: "codex-cloud:thread-a", Source: "session", Model: "gpt-6",
			InputTokens: 800, CacheReadInputTokens: 400, OutputTokens: 110,
			OccurredAt: "2026-10-01T12:04:00Z", DedupKey: "thread-a:1:1650/1500/400/0/150/0",
		},
	}, events)
	assert.Equal(t, int64(300), totalOutput, "100 + 50 + 40 + 110 across both baselines")
	assert.Equal(t, int64(900), peakContext, "largest last input")
}

func TestRequestScopedWhenIncreaseEqualsLast(t *testing.T) {
	const request = usagefacts.CodexCloudRequestSource
	tests := []struct {
		name string
		recs []mirror.TokenUsageData
		want []string
	}{
		{
			name: "first record of a thread is one call",
			recs: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", ccCounts(300, 0, 30), ccCounts(300, 0, 30), 0),
			},
			want: []string{request},
		},
		{
			name: "first record of a later join whose increase equals last",
			recs: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", ccCounts(1000, 0, 100), ccCounts(400, 0, 40), 0),
				ccTokens("join-b", "turn-2", ccCounts(1300, 0, 130), ccCounts(300, 0, 30), 1),
			},
			want: []string{"session", request},
		},
		{
			name: "increase differing from last in one field",
			recs: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", ccCounts(1000, 0, 100), ccCounts(400, 0, 40), 0),
				ccTokens("join-b", "turn-2", ccCounts(1300, 100, 130), ccCounts(300, 0, 30), 1),
			},
			want: []string{"session", "session"},
		},
		{
			name: "first record after a reset equal to last",
			recs: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", ccCounts(1000, 0, 100), ccCounts(400, 0, 40), 0),
				ccTokens("join-b", "turn-2", ccCounts(50, 0, 5), ccCounts(50, 0, 5), 1),
			},
			want: []string{"session", request},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, _, _ := codexCloudUsageEvents("thread-a", tt.recs, "gpt-6")
			sources := make([]string, len(events))
			for i, ev := range events {
				sources[i] = ev.Source
				assert.Nil(t, ev.MessageOrdinal)
			}
			assert.Equal(t, tt.want, sources)
		})
	}
}

func TestUsageEventFieldsAndModel(t *testing.T) {
	tests := []struct {
		name     string
		total    mirror.TokenCounts
		model    string
		fallback string
		want     ParsedUsageEvent
	}{
		{
			name: "cache writes come out of uncached input; reasoning stays inside output",
			total: mirror.TokenCounts{
				Input: 100, CachedInput: 30, CacheWriteInput: 20, Output: 5, ReasoningOutput: 4, Total: 105,
			},
			model: "gpt-6-sol", fallback: "gpt-6",
			want: ParsedUsageEvent{
				Model: "gpt-6-sol", InputTokens: 50, CacheCreationInputTokens: 20,
				CacheReadInputTokens: 30, OutputTokens: 5,
			},
		},
		{
			name:  "cache writes are capped at uncached input",
			total: mirror.TokenCounts{Input: 100, CachedInput: 30, CacheWriteInput: 90, Output: 5, Total: 105},
			// The listing model fills in when the record has none.
			fallback: "gpt-6",
			want: ParsedUsageEvent{
				Model: "gpt-6", CacheCreationInputTokens: 70, CacheReadInputTokens: 30, OutputTokens: 5,
			},
		},
		{
			name:  "uncached input is floored at zero",
			total: mirror.TokenCounts{Input: 10, CachedInput: 20, Output: 1, Total: 11},
			want:  ParsedUsageEvent{Model: "unknown", CacheReadInputTokens: 20, OutputTokens: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := ccTokens("join-a", "turn-1", tt.total, mirror.TokenCounts{}, 0)
			rec.Model = tt.model
			events, _, _ := codexCloudUsageEvents("thread-a", []mirror.TokenUsageData{rec}, tt.fallback)
			require.Len(t, events, 1)
			got := events[0]
			assert.Equal(t, tt.want, ParsedUsageEvent{
				Model: got.Model, InputTokens: got.InputTokens,
				CacheCreationInputTokens: got.CacheCreationInputTokens,
				CacheReadInputTokens:     got.CacheReadInputTokens,
				OutputTokens:             got.OutputTokens, ReasoningTokens: got.ReasoningTokens,
			})
		})
	}
}

func TestPriceBandAppliesAbove272k(t *testing.T) {
	// The gpt-6-astra rows from docs/internal/session-format-sources.md.
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gpt-6-astra",
		Rates: export.ModelRates{
			InputPerMTok:      money.MustParseDollars("10"),
			OutputPerMTok:     money.MustParseDollars("50"),
			CacheWritePerMTok: money.MustParseDollars("12.50"),
			CacheReadPerMTok:  money.MustParseDollars("1"),
			Bands: []export.PricingBand{{
				AboveInputTokens:  272_000,
				InputPerMTok:      money.MustParseDollars("20"),
				OutputPerMTok:     money.MustParseDollars("75"),
				CacheWritePerMTok: money.MustParseDollars("25"),
				CacheReadPerMTok:  money.MustParseDollars("2"),
			}},
		},
	}})
	cost := func(t *testing.T, last mirror.TokenCounts) money.Money {
		t.Helper()
		rec := ccTokens("join-a", "turn-1", ccCounts(300_000, 0, 1000), last, 0)
		rec.Model = "gpt-6-astra"
		events, _, _ := codexCloudUsageEvents("thread-a", []mirror.TokenUsageData{rec}, "")
		require.Len(t, events, 1)
		ev := events[0]
		lookup := resolver.Lookup(ev.Model)
		require.True(t, lookup.OK)
		got, err := lookup.Rates.CostForTokensScoped(
			ev.MessageOrdinal != nil || usagefacts.SourceIsRequestScoped(ev.Source),
			ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens,
			ev.CacheCreationInputTokens, 0, ev.CacheReadInputTokens,
		)
		require.NoError(t, err)
		return got
	}

	// One call of 300,000 input tokens: 0.3 * $20 + 0.001 * $75.
	assert.Equal(t, money.MustParseDollars("6.075"), cost(t, ccCounts(300_000, 0, 1000)))
	// The same increase spread over two calls is priced at base rates:
	// 0.3 * $10 + 0.001 * $50.
	assert.Equal(t, money.MustParseDollars("3.05"), cost(t, ccCounts(150_000, 0, 500)))
}

func TestUsageCountsARepeatedFlushOnce(t *testing.T) {
	// A flush whose state save failed after its records were written is
	// appended again: the file holds the second and third records twice.
	first := ccTokens("join-a", "turn-1", ccCounts(100, 0, 10), ccCounts(100, 0, 10), 0)
	second := ccTokens("join-a", "turn-1", ccCounts(200, 0, 20), ccCounts(100, 0, 10), 1)
	third := ccTokens("join-a", "turn-1", ccCounts(300, 0, 30), ccCounts(100, 0, 10), 2)
	recs := []mirror.TokenUsageData{first, second, third, second, third}

	events, totalOutput, _ := codexCloudUsageEvents("thread-a", recs, "gpt-6")

	var input int
	for _, ev := range events {
		input += ev.InputTokens
	}
	assert.Len(t, events, 3)
	assert.Equal(t, 300, input, "the running total counts once")
	assert.Equal(t, int64(30), totalOutput)
}

func TestUsageResetIsNotLogged(t *testing.T) {
	logs := captureLog(t)
	recs := []mirror.TokenUsageData{
		ccTokens("join-a", "turn-1", ccCounts(300, 0, 30), ccCounts(300, 0, 30), 0),
		ccTokens("join-b", "turn-2", ccCounts(50, 0, 5), ccCounts(50, 0, 5), 1),
	}

	codexCloudUsageEvents("thread-a", recs, "gpt-6")

	assert.NotContains(t, logs.String(), "thread-a", "a reparse would repeat the line for every past reset")
}

func TestUsageKeepsEqualTotalsFromDifferentJoins(t *testing.T) {
	// Two connections can report the same totals; only an exact repeat of
	// one connection's record is dropped. A drop then a return to the same
	// total is a new baseline and counts again.
	high := ccCounts(300, 0, 30)
	recs := []mirror.TokenUsageData{
		ccTokens("join-a", "turn-1", high, high, 0),
		ccTokens("join-b", "turn-2", ccCounts(50, 0, 5), ccCounts(50, 0, 5), 1),
		ccTokens("join-b", "turn-2", high, ccCounts(250, 0, 25), 2),
	}

	events, _, _ := codexCloudUsageEvents("thread-a", recs, "gpt-6")

	var input int
	for _, ev := range events {
		input += ev.InputTokens
	}
	assert.Equal(t, 600, input)
}

func ccTurn(id, status string, startedAt float64) mirror.Turn {
	return mirror.Turn{ID: id, Status: status, StartedAt: &startedAt}
}

func ccCompleted(join, turn string, minute int) mirror.TurnCompletedData {
	return mirror.TurnCompletedData{
		JoinID: join, TurnID: turn, ObservedAt: ccObserved.Add(time.Duration(minute) * time.Minute),
	}
}

func TestCoverageRules(t *testing.T) {
	small, big := ccCounts(1000, 0, 100), ccCounts(2000, 0, 200)
	tests := []struct {
		name        string
		turns       []mirror.Turn
		tokens      []mirror.TokenUsageData
		completions []mirror.TurnCompletedData
		want        int
	}{
		{
			name: "turns before the first record are captured",
			turns: []mirror.Turn{
				ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200),
				ccTurn("turn-3", "completed", 300),
			},
			tokens:      []mirror.TokenUsageData{ccTokens("join-a", "turn-3", small, small, 0)},
			completions: []mirror.TurnCompletedData{ccCompleted("join-a", "turn-3", 1)},
			want:        0,
		},
		{
			name:        "a turn after the last record is not captured",
			turns:       []mirror.Turn{ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200)},
			tokens:      []mirror.TokenUsageData{ccTokens("join-a", "turn-1", small, small, 0)},
			completions: []mirror.TurnCompletedData{ccCompleted("join-a", "turn-1", 1)},
			want:        1,
		},
		{
			name:   "turn/completed seen with no token update is not captured",
			turns:  []mirror.Turn{ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200)},
			tokens: []mirror.TokenUsageData{ccTokens("join-a", "turn-1", small, small, 0)},
			completions: []mirror.TurnCompletedData{
				ccCompleted("join-a", "turn-1", 1), ccCompleted("join-b", "turn-2", 5),
			},
			want: 1,
		},
		{
			name:        "token record and completion from different joins",
			turns:       []mirror.Turn{ccTurn("turn-1", "completed", 100)},
			tokens:      []mirror.TokenUsageData{ccTokens("join-a", "turn-1", small, small, 0)},
			completions: []mirror.TurnCompletedData{ccCompleted("join-b", "turn-1", 5)},
			want:        1,
		},
		{
			name: "turns between baselines are not captured",
			turns: []mirror.Turn{
				ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200),
				ccTurn("turn-3", "completed", 300), ccTurn("turn-4", "completed", 400),
			},
			tokens: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", big, big, 0),
				ccTokens("join-b", "turn-4", small, small, 10),
			},
			completions: []mirror.TurnCompletedData{
				ccCompleted("join-a", "turn-1", 1), ccCompleted("join-b", "turn-4", 11),
			},
			want: 2,
		},
		{
			name: "without a reset a later turn covers the same gap",
			turns: []mirror.Turn{
				ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200),
				ccTurn("turn-3", "completed", 300), ccTurn("turn-4", "completed", 400),
			},
			tokens: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", small, small, 0),
				ccTokens("join-b", "turn-4", big, small, 10),
			},
			completions: []mirror.TurnCompletedData{
				ccCompleted("join-a", "turn-1", 1), ccCompleted("join-b", "turn-4", 11),
			},
			want: 0,
		},
		{
			// A retried monitor save can append an earlier batch again after
			// newer records. The repeat must not read as a counter reset.
			name: "a repeated batch after newer records is not a reset",
			turns: []mirror.Turn{
				ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200),
				ccTurn("turn-3", "completed", 300), ccTurn("turn-4", "completed", 400),
			},
			tokens: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", small, small, 0),
				ccTokens("join-b", "turn-4", big, small, 10),
				ccTokens("join-a", "turn-1", small, small, 0),
			},
			completions: []mirror.TurnCompletedData{
				ccCompleted("join-a", "turn-1", 1), ccCompleted("join-b", "turn-4", 11),
				ccCompleted("join-a", "turn-1", 1),
			},
			want: 0,
		},
		{
			name:  "left after 6 hours with no completion and no later turn",
			turns: []mirror.Turn{ccTurn("turn-1", "completed", 100)},
			tokens: []mirror.TokenUsageData{
				ccTokens("join-a", "turn-1", small, small, 0),
				ccTokens("join-a", "turn-1", big, small, 300),
			},
			want: 1,
		},
		{
			name:        "a running turn is not counted",
			turns:       []mirror.Turn{ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "inProgress", 200)},
			tokens:      []mirror.TokenUsageData{ccTokens("join-a", "turn-1", small, small, 0)},
			completions: []mirror.TurnCompletedData{ccCompleted("join-a", "turn-1", 1)},
			want:        0,
		},
		{
			name:   "a completion record finishes a turn the listing still shows running",
			turns:  []mirror.Turn{ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "inProgress", 200)},
			tokens: []mirror.TokenUsageData{ccTokens("join-a", "turn-1", small, small, 0)},
			completions: []mirror.TurnCompletedData{
				ccCompleted("join-a", "turn-1", 1), ccCompleted("join-b", "turn-2", 5),
			},
			want: 1,
		},
		{
			name:   "a record for a turn not yet listed covers the listed turns",
			turns:  []mirror.Turn{ccTurn("turn-1", "completed", 100), ccTurn("turn-2", "completed", 200)},
			tokens: []mirror.TokenUsageData{ccTokens("join-a", "turn-3", small, small, 0)},
			want:   0,
		},
		{
			name: "turns are ordered by start time, not record order",
			turns: []mirror.Turn{
				ccTurn("turn-2", "completed", 200), ccTurn("turn-1", "completed", 100),
			},
			// turn-2 started later, so its record covers turn-1; turn-2
			// itself has no completion record.
			tokens: []mirror.TokenUsageData{ccTokens("join-a", "turn-2", small, small, 0)},
			want:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexCloudCoverage(tt.turns, tt.tokens, tt.completions)
			assert.Equal(t, tt.want, got.UncapturedTurns)
		})
	}
}

func TestCoverageLastCaptureIsNewestTokenRecord(t *testing.T) {
	small := ccCounts(1000, 0, 100)
	got := codexCloudCoverage(
		[]mirror.Turn{ccTurn("turn-1", "completed", 100)},
		[]mirror.TokenUsageData{
			ccTokens("join-a", "turn-1", small, small, 7),
			ccTokens("join-a", "turn-1", small, small, 3),
		},
		[]mirror.TurnCompletedData{ccCompleted("join-a", "turn-1", 9)},
	)
	require.NotNil(t, got.LastCaptureAt)
	assert.Equal(t, ccObserved.Add(7*time.Minute), *got.LastCaptureAt,
		"a completion record is not a capture")

	none := codexCloudCoverage([]mirror.Turn{ccTurn("turn-1", "completed", 100)}, nil, nil)
	assert.Nil(t, none.LastCaptureAt)
	assert.Equal(t, 1, none.UncapturedTurns)
}

func ccDataRecord(t *testing.T, kind mirror.Kind, data any) mirror.Record {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	return mirror.Record{Kind: kind, FetchedAt: codexCloudFetched, Data: jsontext.Value(raw)}
}

func TestCodexCloudFileUsageAndCoverage(t *testing.T) {
	first := ccTokens("join-a", "turn-1", ccCounts(1000, 0, 100), ccCounts(1000, 0, 100), 0)
	first.Model = "gpt-6-sol"
	// A later join sees a running total far past 2,000,000 tokens.
	second := ccTokens("join-b", "turn-2",
		ccCounts(3_001_000, 500_000, 2_100_100), ccCounts(1000, 0, 0), 30)
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccDataRecord(t, mirror.KindTurn, ccTurn("turn-1", "completed", 1759320000)),
		ccDataRecord(t, mirror.KindTurn, ccTurn("turn-2", "completed", 1759320600)),
		ccDataRecord(t, mirror.KindTurn, ccTurn("turn-3", "inProgress", 1759321200)),
		ccDataRecord(t, mirror.KindTokenUsage, first),
		ccDataRecord(t, mirror.KindTurnCompleted, ccCompleted("join-a", "turn-1", 1)),
		ccDataRecord(t, mirror.KindTokenUsage, second),
	)
	result := parseCodexCloud(t, path)

	want := []ParsedUsageEvent{
		{
			SessionID: "codex-cloud:thread-a", Source: usagefacts.CodexCloudRequestSource, Model: "gpt-6-sol",
			InputTokens: 1000, OutputTokens: 100,
			OccurredAt: "2026-10-01T12:00:00Z", DedupKey: "thread-a:0:1100/1000/0/0/100/0",
		},
		{
			SessionID: "codex-cloud:thread-a", Source: "session", Model: "gpt-6",
			InputTokens: 2_500_000, CacheReadInputTokens: 500_000, OutputTokens: 2_100_000,
			OccurredAt: "2026-10-01T12:30:00Z", DedupKey: "thread-a:0:5101100/3001000/500000/0/2100100/0",
		},
	}
	assert.Equal(t, want, result.UsageEvents)
	assert.Equal(t, want, result.Session.UsageEvents)
	assert.Equal(t, 2_100_100, result.Session.TotalOutputTokens)
	assert.True(t, result.Session.HasTotalOutputTokens)
	assert.Equal(t, 1000, result.Session.PeakContextTokens)
	assert.True(t, result.Session.HasPeakContextTokens)
	lastCapture := ccObserved.Add(30 * time.Minute)
	assert.Equal(t, &ParsedCaptureCoverage{LastCaptureAt: &lastCapture, UncapturedTurns: 1},
		result.Session.CaptureCoverage, "turn-2 has no completion and no later record")
}

func TestCodexCloudFileWithoutTokenRecordsHasNoCoverage(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccDataRecord(t, mirror.KindTurn, ccTurn("turn-1", "completed", 1759320000)),
	)
	result := parseCodexCloud(t, path)
	assert.Empty(t, result.UsageEvents)
	assert.False(t, result.Session.HasTotalOutputTokens)
	assert.False(t, result.Session.HasPeakContextTokens)
	assert.Nil(t, result.Session.CaptureCoverage)
}
