package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func usdPtr(u USD) *USD { return &u }

// Ports digest.rs accumulate_stats (:1169-1203) through the fold-level
// assertions of run_review_annotates_recurring_signal_with_session_cost,
// run_review_no_recurrence_annotation_for_new_signals and
// stats_only_session_counts_toward_spend_and_personas.
func TestSpendSummaryAccumulate(t *testing.T) {
	tests := []struct {
		name      string
		usage     []SessionUsage
		total     string // "" = nil
		withStats int
		withCost  int
		in, out   uint64
		roles     map[string]string
		models    map[string]string
	}{
		{
			name: "run_review_annotates_recurring_signal_with_session_cost",
			usage: []SessionUsage{{
				SubjectID: "sess-r_explore", Role: "explore",
				InputTokens: 100, OutputTokens: 10,
				CostUSD: usdPtr(mustUSD(t, "4.2")),
			}},
			total: "4.2", withStats: 1, withCost: 1, in: 100, out: 10,
			roles: map[string]string{"explore": "4.2"},
		},
		{
			name: "run_review_no_recurrence_annotation_for_new_signals",
			usage: []SessionUsage{{
				SubjectID: "sess-n", InputTokens: 1, OutputTokens: 1,
				CostUSD: usdPtr(mustUSD(t, "1.0")),
			}},
			total: "1.0", withStats: 1, withCost: 1, in: 1, out: 1,
			roles: map[string]string{"(root)": "1.0"},
		},
		{
			name: "stats_only_session_counts_toward_spend_and_personas",
			usage: []SessionUsage{{
				SubjectID: "sess-stats", InputTokens: 7, OutputTokens: 3,
			}},
			total: "", withStats: 1, withCost: 0, in: 7, out: 3,
		},
		{
			name: "sums_keep_the_max_scale_and_model_costs_fold",
			usage: []SessionUsage{
				{SubjectID: "a", Role: RootRole, CostUSD: usdPtr(mustUSD(t, "1.2")),
					ModelCosts: map[string]USD{"claude-opus-5": mustUSD(t, "1.2")}},
				{SubjectID: "b", Role: "subagent", CostUSD: usdPtr(mustUSD(t, "3")),
					ModelCosts: map[string]USD{"claude-opus-5": mustUSD(t, "3")}},
			},
			total: "4.2", withStats: 2, withCost: 2,
			roles:  map[string]string{"(root)": "1.2", "subagent": "3"},
			models: map[string]string{"claude-opus-5": "4.2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s SpendSummary
			for _, u := range tt.usage {
				cost := s.Accumulate(u)
				if u.CostUSD == nil {
					assert.Nil(t, cost)
				} else {
					require.NotNil(t, cost)
					assert.Equal(t, u.CostUSD.String(), cost.String())
				}
			}
			if tt.total == "" {
				assert.Nil(t, s.Total)
			} else {
				require.NotNil(t, s.Total)
				assert.Equal(t, tt.total, s.Total.String())
			}
			assert.Equal(t, tt.withStats, s.SessionsWithStats)
			assert.Equal(t, tt.withCost, s.SessionsWithCost)
			assert.Equal(t, tt.in, s.InputTokens)
			assert.Equal(t, tt.out, s.OutputTokens)
			assert.Equal(t, tt.roles, usdStrings(s.RoleCosts))
			assert.Equal(t, tt.models, usdStrings(s.ModelCosts))
		})
	}
}

func usdStrings(m map[string]USD) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

// Ports recurrence_cost_annotations (digest.rs:1208-1243), keyed by
// fingerprint instead of title.
func TestRecurrenceCostAnnotations(t *testing.T) {
	corr := Signal{Kind: KindCorrection, SubjectID: "sess-r_explore", Text: "no, use the gh cli for calendar"}
	corrOther := corr
	corrOther.SubjectID = "sess-2"
	errSig := Signal{Kind: KindError, SubjectID: "sess-3", ToolName: "bash", Text: "boom"}
	def := Signal{Kind: KindDeferral, SubjectID: "sess-r_explore", Label: "next session"}
	frus := Signal{Kind: KindFrustration, SubjectID: "sess-r_explore", Text: "this is broken again"}
	intr := Signal{Kind: KindInterruption, SubjectID: "sess-r_explore", Text: "[Request interrupted by user]"}
	costs := map[string]USD{
		"sess-r_explore": mustUSD(t, "4.2"),
		"sess-2":         mustUSD(t, "0.05"),
	}
	tests := []struct {
		name string
		sigs []Signal
		open map[string]bool
		want map[string]string
	}{
		{
			name: "run_review_annotates_recurring_signal_with_session_cost",
			sigs: []Signal{corr},
			open: map[string]bool{corr.Fingerprint(): true},
			want: map[string]string{corr.Fingerprint(): "$4.20"},
		},
		{
			name: "run_review_no_recurrence_annotation_for_new_signals",
			sigs: []Signal{corr},
			open: map[string]bool{"fl1:other": true},
			want: map[string]string{},
		},
		{
			name: "distinct_sessions_sum_with_max_scale",
			// corr and corrOther have different titles (the correction
			// title carries the session id), so give both fingerprints.
			sigs: []Signal{corr, corr, corrOther},
			open: map[string]bool{corr.Fingerprint(): true, corrOther.Fingerprint(): true},
			want: map[string]string{
				corr.Fingerprint():      "$4.20",
				corrOther.Fingerprint(): "$0.05",
			},
		},
		{
			name: "sessions_without_cost_give_no_annotation",
			sigs: []Signal{errSig},
			open: map[string]bool{errSig.Fingerprint(): true},
			want: map[string]string{},
		},
		{
			name: "deferrals_never_annotated",
			sigs: []Signal{def},
			open: map[string]bool{def.Fingerprint(): true},
			want: map[string]string{},
		},
		{
			name: "frustration_annotated_like_corrections",
			sigs: []Signal{frus},
			open: map[string]bool{frus.Fingerprint(): true},
			want: map[string]string{frus.Fingerprint(): "$4.20"},
		},
		{
			name: "interruptions_never_annotated",
			sigs: []Signal{intr},
			open: map[string]bool{intr.Fingerprint(): true},
			want: map[string]string{},
		},
		{
			name: "empty_open_set_short_circuits",
			sigs: []Signal{corr},
			open: nil,
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecurrenceCostAnnotations(tt.sigs, tt.open, costs)
			formatted := map[string]string{}
			for fp, u := range got {
				formatted[fp] = FormatUSD(u)
			}
			assert.Equal(t, tt.want, formatted)
		})
	}
}

func TestCmpUSD(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal_values_different_scale", "4.2", "4.200000", 0},
		{"greater", "224.55406", "104.943456", 1},
		{"less", "1", "1.000001", -1},
		{"negative", "-0.5", "0", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cmpUSD(mustUSD(t, tt.a), mustUSD(t, tt.b)))
		})
	}
	assert.Equal(t, 0, cmpUSD(USD{}, mustUSD(t, "0")), "zero value compares as 0")
}

// format_usd lives in PR 1's usd.go; the render contract depends on it,
// so the jilog test is re-pinned here (roadmap PR 4 acceptance).
func TestFormatUSDRenderContract(t *testing.T) {
	tests := []struct{ in, want string }{
		{"4.2", "$4.20"},
		{"7", "$7.00"},
		{"0.0003", "$0.0003"},
		{"4.20", "$4.20"},
		{"332.138392", "$332.138392"},
	}
	for _, tt := range tests {
		t.Run("format_usd_pads_cents_but_keeps_subcent_precision/"+tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatUSD(mustUSD(t, tt.in)))
		})
	}
	assert.Equal(t, "$1300.250000", FormatUSD(USDFromMicros(1_300_250_000)))
}

// Ports archive_spend.rs parses_daily_rows_and_summarizes_yesterday_and_week
// (:269-330), minus usage-daily JSON parsing replaced by the native rollup query.
func archiveRows() []DailySpend {
	return []DailySpend{
		{Date: "2026-09-14", Total: USDFromMicros(1_500_000),
			Agents: map[string]USD{"codex": USDFromMicros(1_500_000)},
			Models: map[string]USD{"gpt-5.6-sol": USDFromMicros(1_500_000)}},
		{Date: "2026-09-15", Total: USDFromMicros(332_138_392),
			Agents: map[string]USD{
				"codex": USDFromMicros(224_554_060), "claude": USDFromMicros(104_943_456),
				"cowork": USDFromMicros(2_640_876),
			},
			Models: map[string]USD{
				"gpt-6-astra": USDFromMicros(125_784_488), "claude-opus-5": USDFromMicros(107_584_332),
				"gpt-5.6-sol": USDFromMicros(98_769_572),
			}},
		{Date: "2026-09-16", Total: USDFromMicros(999)},
	}
}

func names(ranked []NamedUSD) []string {
	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.Name)
	}
	return out
}

func TestArchiveWindow(t *testing.T) {
	tests := []struct{ date, from, to string }{
		{"2026-09-16", "2026-09-09", "2026-09-15"},
		{"2026-09-18", "2026-09-11", "2026-09-17"},
		{"2026-03-01", "2026-02-22", "2026-02-28"},
	}
	for _, tt := range tests {
		t.Run(tt.date, func(t *testing.T) {
			from, to, err := ArchiveWindow(tt.date)
			require.NoError(t, err)
			assert.Equal(t, tt.from, from)
			assert.Equal(t, tt.to, to)
		})
	}
	_, _, err := ArchiveWindow("2026-9-16")
	require.Error(t, err)
}

func TestSummarizeArchiveSpend(t *testing.T) {
	t.Run("parses_daily_rows_and_summarizes_yesterday_and_week", func(t *testing.T) {
		spend, err := SummarizeArchiveSpend(archiveRows(), "2026-09-16", "Asia/Dhaka")
		require.NoError(t, err)
		require.NotNil(t, spend, "rows in window")
		assert.Equal(t, "Asia/Dhaka", spend.Timezone)
		assert.Equal(t, "2026-09-09", spend.WeekFrom)
		assert.Equal(t, "2026-09-15", spend.WeekTo)
		require.NotNil(t, spend.Yesterday)
		assert.Equal(t, "332.138392", spend.Yesterday.Total.String())
		assert.Equal(t, 2, spend.Week.Days)
		assert.Equal(t, "333.638392", spend.Week.Total.String())
		assert.Equal(t, "226.054060", spend.Week.Agents["codex"].String())
		assert.Equal(t, []string{"codex", "claude", "cowork"}, names(spend.Week.AgentsByCost()))
		assert.Equal(t, []string{"gpt-6-astra", "claude-opus-5"}, names(spend.Week.TopModels(2)))
	})
	t.Run("yesterday_absent_week_still_summarized", func(t *testing.T) {
		spend, err := SummarizeArchiveSpend(archiveRows(), "2026-09-18", "UTC")
		require.NoError(t, err)
		require.NotNil(t, spend)
		assert.Nil(t, spend.Yesterday)
		assert.Equal(t, 3, spend.Week.Days)
	})
	t.Run("nothing_in_window_is_nil", func(t *testing.T) {
		spend, err := SummarizeArchiveSpend(archiveRows(), "2027-01-01", "UTC")
		require.NoError(t, err)
		assert.Nil(t, spend)
		spend, err = SummarizeArchiveSpend(nil, "2026-09-16", "UTC")
		require.NoError(t, err)
		assert.Nil(t, spend)
	})
	t.Run("window_edges", func(t *testing.T) {
		edge := func(d string) DailySpend { return DailySpend{Date: d, Total: mustUSD(t, "1")} }
		spend, err := SummarizeArchiveSpend([]DailySpend{edge("2026-09-08"), edge("2026-09-09")}, "2026-09-16", "UTC")
		require.NoError(t, err)
		require.NotNil(t, spend)
		assert.Equal(t, 1, spend.Week.Days)
	})
	t.Run("top_5_truncation_descending", func(t *testing.T) {
		many := DailySpend{Date: "2026-09-15", Total: mustUSD(t, "1"), Models: map[string]USD{}}
		for i := range 8 {
			many.Models["m"+string(rune('0'+i))] = mustUSD(t, string(rune('0'+i)))
		}
		spend, err := SummarizeArchiveSpend([]DailySpend{many}, "2026-09-16", "UTC")
		require.NoError(t, err)
		require.NotNil(t, spend)
		assert.Equal(t, []string{"m7", "m6", "m5", "m4", "m3"}, names(spend.Week.TopModels(5)))
	})
	t.Run("ties_break_by_name", func(t *testing.T) {
		p := PeriodSpend{Agents: map[string]USD{"b": mustUSD(t, "1.00"), "a": mustUSD(t, "1"), "c": mustUSD(t, "2")}}
		assert.Equal(t, []string{"c", "a", "b"}, names(p.AgentsByCost()))
	})
	t.Run("malformed_row_date_is_an_error", func(t *testing.T) {
		_, err := SummarizeArchiveSpend([]DailySpend{{Date: "15/09/2026", Total: mustUSD(t, "1")}}, "2026-09-16", "UTC")
		require.Error(t, err)
	})
}
