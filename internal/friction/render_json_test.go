package friction

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/serdejson"
)

// digestReportFixture ports commands/review.rs digest_report() (:418-462),
// scrubbed: persona helper@general, backend kata, digest_path is the
// public_url-unset form.
func digestReportFixture(t *testing.T) (DigestSnapshot, SummaryMeta) {
	t.Helper()
	path := "friction:2026-05-10"
	snapshot := DigestSnapshot{
		Date:            "2026-05-10",
		P0Alerts:        map[string][]string{"bash": {"session-a", "session-b"}},
		Personas:        map[PersonaKey]*PersonaCounts{{"helper", "general"}: {Sessions: 2, Corrections: 1, Patterns: 1, InputTokens: 5000, OutputTokens: 250}},
		SessionsScanned: 3,
	}
	meta := SummaryMeta{
		DigestPath: &path,
		CreatedIssues: []IssueRef{{
			ID: "#42", Backend: "kata", Title: "tracked issue",
			URL: "https://example.com/issues/42",
		}},
	}
	return snapshot, meta
}

func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	v, err := serdejson.Decode(b)
	require.NoError(t, err)
	obj, ok := v.(map[string]any)
	require.True(t, ok, "top level must be an object")
	return obj
}

func TestRenderSummaryJSON(t *testing.T) {
	t.Run("json_output_has_documented_keys", func(t *testing.T) {
		s, m := digestReportFixture(t)
		obj := decodeObject(t, RenderSummaryJSON(s, m))
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		assert.Equal(t, []string{
			"corrections", "created_issues", "deferrals", "digest_path", "errors",
			"frustrations", "interruptions", "p0_alerts", "patterns", "personas", "schema_version", "sessions_scanned",
			"spend", "tracker_failures", "workarounds",
		}, keys)
		assert.Equal(t, map[string]any{
			"helper@general": map[string]any{
				"persona": "helper", "channel": "general",
				"sessions": serdejson.Number("2"), "corrections": serdejson.Number("1"),
				"errors": serdejson.Number("0"), "workarounds": serdejson.Number("0"),
				"deferrals": serdejson.Number("0"), "patterns": serdejson.Number("1"),
				"input_tokens": serdejson.Number("5000"), "output_tokens": serdejson.Number("250"),
				"cost_usd": nil,
			},
		}, obj["personas"])
		assert.Equal(t, serdejson.Number("3"), obj["schema_version"])
		assert.Equal(t, serdejson.Number("3"), obj["sessions_scanned"])
		assert.Equal(t, map[string]any{"bash": []any{"session-a", "session-b"}}, obj["p0_alerts"])
		assert.Equal(t, []any{map[string]any{
			"id": "#42", "backend": "kata", "title": "tracked issue",
			"url": "https://example.com/issues/42",
		}}, obj["created_issues"])
	})
	t.Run("json_dry_run_uses_null_digest_path", func(t *testing.T) {
		s, m := digestReportFixture(t)
		m.DigestPath = nil
		m.CreatedIssues = nil
		out := RenderSummaryJSON(s, m)
		obj := decodeObject(t, out)
		assert.Nil(t, obj["digest_path"])
		assert.Equal(t, []any{}, obj["created_issues"])
		assert.Contains(t, string(out), "\"created_issues\": [],\n")
	})
	t.Run("review_json_carries_archive_spend_only_when_present", func(t *testing.T) {
		s, m := digestReportFixture(t)
		_, present := decodeObject(t, RenderSummaryJSON(s, m))["archive_spend"]
		assert.False(t, present, "no key when absent")
		s.ArchiveSpend = &ArchiveSpend{
			Week: PeriodSpend{
				Total: USDFromMicros(2_101_500_000), Days: 7,
				Agents: map[string]USD{"codex": USDFromMicros(1_300_250_000)},
			},
			WeekFrom: "2026-09-09", WeekTo: "2026-09-15", Timezone: "Asia/Dhaka",
		}
		obj := decodeObject(t, RenderSummaryJSON(s, m))
		archive, ok := obj["archive_spend"].(map[string]any)
		require.True(t, ok)
		week := archive["week"].(map[string]any)
		assert.Equal(t, "2101.500000", week["total_usd"])
		assert.Equal(t, serdejson.Number("7"), week["days"])
		assert.Equal(t, map[string]any{"codex": "1300.250000"}, week["agents_usd"])
		assert.Equal(t, map[string]any{}, week["models_usd"])
		assert.Equal(t, "2026-09-09", archive["week_from"])
		assert.Equal(t, "Asia/Dhaka", archive["timezone"])
		assert.Nil(t, archive["yesterday"])
		assert.Equal(t, serdejson.Number("3"), obj["schema_version"])
	})
	t.Run("spend_object_uses_decimal_strings", func(t *testing.T) {
		s, m := digestReportFixture(t)
		s.Spend = &SpendSummary{
			Total: new(mustUSD(t, "4.2")), SessionsWithStats: 3, SessionsWithCost: 2,
			InputTokens: 1000, OutputTokens: 50,
			RoleCosts:  map[string]USD{"(root)": mustUSD(t, "1.2")},
			ModelCosts: map[string]USD{"claude-opus-5": mustUSD(t, "4.2")},
		}
		spend := decodeObject(t, RenderSummaryJSON(s, m))["spend"].(map[string]any)
		assert.Equal(t, map[string]any{
			"total_usd":           "4.2",
			"sessions_with_stats": serdejson.Number("3"),
			"sessions_with_cost":  serdejson.Number("2"),
			"input_tokens":        serdejson.Number("1000"),
			"output_tokens":       serdejson.Number("50"),
			"role_costs_usd":      map[string]any{"(root)": "1.2"},
			"model_costs_usd":     map[string]any{"claude-opus-5": "4.2"},
		}, spend)
	})
	t.Run("counts_come_from_signals", func(t *testing.T) {
		s, m := digestReportFixture(t)
		s.Signals = []Signal{{Kind: KindError}, {Kind: KindError}, {Kind: KindDeferral}}
		obj := decodeObject(t, RenderSummaryJSON(s, m))
		assert.Equal(t, serdejson.Number("2"), obj["errors"])
		assert.Equal(t, serdejson.Number("1"), obj["deferrals"])
		assert.Equal(t, serdejson.Number("0"), obj["corrections"])
	})
	t.Run("frustration_and_interruption_keys_always_present", func(t *testing.T) {
		s, m := digestReportFixture(t)
		obj := decodeObject(t, RenderSummaryJSON(s, m))
		assert.Equal(t, serdejson.Number("0"), obj["frustrations"])
		assert.Equal(t, serdejson.Number("0"), obj["interruptions"])

		s.Signals = []Signal{{Kind: KindFrustration}, {Kind: KindFrustration}, {Kind: KindInterruption}}
		obj = decodeObject(t, RenderSummaryJSON(s, m))
		assert.Equal(t, serdejson.Number("2"), obj["frustrations"])
		assert.Equal(t, serdejson.Number("1"), obj["interruptions"])
		assert.Equal(t, serdejson.Number("3"), obj["schema_version"])
	})
	t.Run("empty_issue_url_is_null", func(t *testing.T) {
		s, m := digestReportFixture(t)
		m.CreatedIssues[0].URL = ""
		issue := decodeObject(t, RenderSummaryJSON(s, m))["created_issues"].([]any)[0].(map[string]any)
		assert.Nil(t, issue["url"])
	})
	t.Run("ends_with_one_newline", func(t *testing.T) {
		s, m := digestReportFixture(t)
		out := RenderSummaryJSON(s, m)
		require.NotEmpty(t, out)
		assert.Equal(t, byte('\n'), out[len(out)-1])
		assert.NotEqual(t, byte('\n'), out[len(out)-2])
	})
}

// Ports commands/review.rs:148-195 (no jilog unit test exists; the lines
// are pinned here, including the raw-decimal Spend quirk).
func TestHumanSummary(t *testing.T) {
	path := "friction:2026-09-16"
	tests := []struct {
		name string
		snap DigestSnapshot
		meta SummaryMeta
		want string
	}{
		{
			name: "counts_only_dry_run",
			snap: DigestSnapshot{SessionsScanned: 0},
			want: "0 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns, 0 P0 alert(s), 0 session(s) scanned\n",
		},
		{
			name: "spend_line_prints_the_raw_decimal",
			snap: DigestSnapshot{
				Signals:         []Signal{{Kind: KindCorrection}, {Kind: KindPattern}},
				P0Alerts:        map[string][]string{"bash": {"a", "b", "c"}},
				SessionsScanned: 3,
				Spend:           &SpendSummary{Total: new(mustUSD(t, "4.2")), SessionsWithStats: 3, SessionsWithCost: 2},
			},
			meta: SummaryMeta{DigestPath: &path},
			want: "1 corrections, 0 errors, 0 workarounds, 0 deferrals, 1 patterns, 1 P0 alert(s), 3 session(s) scanned\n" +
				"Spend: $4.2 across 2 of 3 session(s) with usage data\n" +
				"Digest: friction:2026-09-16\n",
		},
		{
			name: "no_total_no_spend_line",
			snap: DigestSnapshot{Spend: &SpendSummary{SessionsWithStats: 1}},
			want: "0 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns, 0 P0 alert(s), 0 session(s) scanned\n",
		},
		{
			name: "archive_issues_and_failures",
			snap: DigestSnapshot{
				ArchiveSpend: &ArchiveSpend{
					Week:     PeriodSpend{Total: USDFromMicros(2_101_500_000), Days: 7},
					WeekFrom: "2026-09-09", WeekTo: "2026-09-15",
				},
			},
			meta: SummaryMeta{
				DigestPath:      &path,
				CreatedIssues:   []IssueRef{{ID: "#1"}, {ID: "#2"}},
				TrackerFailures: 1,
			},
			want: "0 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns, 0 P0 alert(s), 0 session(s) scanned\n" +
				"Archive spend: n/a yesterday, $2101.500000 trailing 7d (7 day(s))\n" +
				"Digest: friction:2026-09-16\n" +
				"Created 2 issue(s)\n" +
				"Tracker failures: 1 (affected sessions retry next run)\n",
		},
		{
			name: "frustration_and_interruption_counts_appended_when_nonzero",
			snap: DigestSnapshot{
				Signals:         []Signal{{Kind: KindFrustration}, {Kind: KindCorrection}},
				SessionsScanned: 1,
			},
			want: "1 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns, 0 P0 alert(s), 1 session(s) scanned, 1 frustrations, 0 interruptions\n",
		},
		{
			name: "archive_with_yesterday",
			snap: DigestSnapshot{
				ArchiveSpend: &ArchiveSpend{
					Yesterday: &PeriodSpend{Total: USDFromMicros(332_138_392), Days: 1},
					Week:      PeriodSpend{Total: USDFromMicros(333_638_392), Days: 2},
				},
			},
			want: "0 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns, 0 P0 alert(s), 0 session(s) scanned\n" +
				"Archive spend: $332.138392 yesterday, $333.638392 trailing 7d (2 day(s))\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HumanSummary(tt.snap, tt.meta))
		})
	}
}
