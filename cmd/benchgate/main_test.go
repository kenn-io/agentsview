package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/perf/benchfmt"
)

func parseString(t *testing.T, input string) (benchSamples, []string) {
	t.Helper()
	got, syntaxErrs, err := parseBench(
		benchfmt.NewReader(strings.NewReader(input), "test"),
	)
	require.NoError(t, err)
	return got, syntaxErrs
}

func TestParseBench(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		want       benchSamples
		wantSyntax int
	}{
		{
			name: "full benchmem line with tidied units",
			input: "goos: linux\n" +
				"pkg: example.com/x\n" +
				"BenchmarkFoo-8   \t 100\t 1234567 ns/op\t 2345 B/op\t 67 allocs/op\n" +
				"PASS\nok  \texample.com/x\t1.2s\n",
			want: benchSamples{
				"example.com/x.Foo-8": {
					"sec/op":    {1234567e-9},
					"B/op":      {2345},
					"allocs/op": {67},
				},
			},
		},
		{
			name: "multiple counts collect all samples",
			input: "BenchmarkFoo-8 100 200 ns/op 9 allocs/op\n" +
				"BenchmarkFoo-8 100 150 ns/op 8 allocs/op\n" +
				"BenchmarkFoo-8 100 180 ns/op 10 allocs/op\n",
			want: benchSamples{
				"Foo-8": {
					"sec/op":    {200e-9, 150e-9, 180e-9},
					"allocs/op": {9, 8, 10},
				},
			},
		},
		{
			name: "same benchmark name in different packages stays separate",
			input: "pkg: example.com/a\n" +
				"BenchmarkScan-4 10 100 ns/op\n" +
				"pkg: example.com/b\n" +
				"BenchmarkScan-4 10 900 ns/op\n",
			want: benchSamples{
				"example.com/a.Scan-4": {"sec/op": {100e-9}},
				"example.com/b.Scan-4": {"sec/op": {900e-9}},
			},
		},
		{
			name: "log lines and headers are ignored",
			input: "2026/07/03 10:20:36 discovered 40 files in 0s\n" +
				"cpu: Apple M5 Max\n",
			want: benchSamples{},
		},
		{
			name: "result line corrupted by interleaved log output is a syntax error",
			input: "BenchmarkSyncAllWarmNoop-18   \t2026/07/03 15:39:07 discovered 40 files in 0s\n" +
				"       5\t   3581975 ns/op\t  212433 B/op\t    2760 allocs/op\n",
			want:       benchSamples{},
			wantSyntax: 1,
		},
		{
			name:  "custom ReportMetric units are kept",
			input: "BenchmarkBaz-2 10 900 ns/op 3 things/op\n",
			want: benchSamples{
				"Baz-2": {
					"sec/op":    {900e-9},
					"things/op": {3},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, syntaxErrs := parseString(t, tt.input)
			assert.Len(t, syntaxErrs, tt.wantSyntax)
			require.Len(t, got, len(tt.want))
			for name, wantUnits := range tt.want {
				gotUnits, ok := got[name]
				require.True(t, ok, "missing benchmark %s", name)
				for unit, wantVals := range wantUnits {
					assert.InDeltaSlice(
						t, wantVals, gotUnits[unit], 1e-15,
						"%s %s", name, unit,
					)
				}
			}
		})
	}
}

func testGates() []gate {
	return []gate{
		{unit: "allocs/op", maxRatio: 1.25, floor: 64, worstCase: true},
		{unit: "B/op", maxRatio: 1.35, floor: 16_384, worstCase: true},
		{
			unit: "sec/op", maxRatio: 2.0, floor: 100_000e-9,
			needSignificance: true,
		},
	}
}

// noisy fabricates a benchmark sample of n values spread ±2% around
// center, so significance tests have a realistic distribution.
func noisy(center float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = center * (0.98 + 0.01*float64(i%5))
	}
	return out
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name       string
		old, new   benchSamples
		wantUnits  []string // units of expected violations, in order
		wantReport []string // substrings that must appear in the report
	}{
		{
			name: "within thresholds passes",
			old: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1e-3, 6),
					"B/op":      {100_000},
					"allocs/op": {1000},
				},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1.5e-3, 6),
					"B/op":      {120_000},
					"allocs/op": {1100},
				},
			},
			wantUnits: nil,
		},
		{
			name: "alloc regression fails even with a single run",
			old: benchSamples{
				"BenchmarkFoo-8": {"allocs/op": {1000}},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {"allocs/op": {2000}},
			},
			wantUnits: []string{"allocs/op"},
		},
		{
			name: "significant time blowup fails",
			old: benchSamples{
				"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 6)},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {"sec/op": noisy(5e-3, 6)},
			},
			wantUnits: []string{"sec/op"},
		},
		{
			name: "time blowup without significance is reported, not gated",
			old: benchSamples{
				"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 5)},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op": append(noisy(1e-3, 4), 5e-3),
				},
			},
			wantUnits:  nil,
			wantReport: []string{"not significant, not gated"},
		},
		{
			name: "tiny baseline below floor is not gated",
			old: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    {500e-9},
					"B/op":      {128},
					"allocs/op": {3},
				},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    {5000e-9},
					"B/op":      {1280},
					"allocs/op": {30},
				},
			},
			wantUnits:  nil,
			wantReport: []string{"not gated"},
		},
		{
			name: "new benchmark without baseline is reported, not gated",
			old:  benchSamples{},
			new: benchSamples{
				"BenchmarkNew-8": {"allocs/op": {99999}},
			},
			wantUnits:  nil,
			wantReport: []string{"no baseline to compare"},
		},
		{
			name: "removed benchmark is reported, not gated",
			old: benchSamples{
				"BenchmarkGone-8": {"sec/op": {1e-3}},
			},
			new:        benchSamples{},
			wantUnits:  nil,
			wantReport: []string{"missing from candidate"},
		},
		{
			name: "gated unit missing from the baseline is reported, not gated",
			old: benchSamples{
				"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 6)},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1e-3, 6),
					"allocs/op": {90000},
				},
			},
			wantUnits:  nil,
			wantReport: []string{"allocs/op missing from baseline, not gated"},
		},
		{
			name: "custom ReportMetric unit is reported as ungated",
			old: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1e-3, 6),
					"things/op": {3},
				},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1e-3, 6),
					"things/op": {900},
				},
			},
			wantUnits:  nil,
			wantReport: []string{"things/op has no gate, not gated"},
		},
		{
			name: "multiple regressions are all reported",
			old: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(1e-3, 6),
					"B/op":      {100_000},
					"allocs/op": {1000},
				},
			},
			new: benchSamples{
				"BenchmarkFoo-8": {
					"sec/op":    noisy(9e-3, 6),
					"B/op":      {900_000},
					"allocs/op": {9000},
				},
			},
			wantUnits: []string{"allocs/op", "B/op", "sec/op"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, violations, issues := compare(tt.old, tt.new, testGates())
			units := make([]string, 0, len(violations))
			for _, v := range violations {
				units = append(units, v.unit)
			}
			assert.Empty(t, issues)
			if len(tt.wantUnits) == 0 {
				assert.Empty(t, violations)
			} else {
				assert.Equal(t, tt.wantUnits, units)
			}
			joined := strings.Join(report, "\n")
			for _, want := range tt.wantReport {
				assert.Contains(t, joined, want)
			}
		})
	}
}

// TestCompareOutlierRunPolicy pins the split policy for a single
// outlier among repeated -count runs of one benchmark. allocs/op is
// deterministic for identical code and iteration counts, so one
// outlier run means a real intermittent allocation path and must
// fail even though the median is unchanged. Wall-clock time is
// summarized by its median, so one slow run on a noisy runner cannot
// fail the gate on its own.
func TestCompareOutlierRunPolicy(t *testing.T) {
	t.Run("alloc outlier run fails", func(t *testing.T) {
		old := benchSamples{
			"BenchmarkFoo-8": {"allocs/op": {1000, 1000, 1000, 1000, 1000}},
		}
		next := benchSamples{
			"BenchmarkFoo-8": {"allocs/op": {1000, 1000, 9000, 1000, 1000}},
		}
		_, violations, issues := compare(old, next, testGates())
		require.Len(t, violations, 1)
		assert.Empty(t, issues)
		assert.Equal(t, "allocs/op", violations[0].unit)
		assert.InDelta(t, 9000, violations[0].new, 1e-9,
			"the worst run is what gets gated")
	})

	t.Run("time outlier run does not fail", func(t *testing.T) {
		old := benchSamples{
			"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 6)},
		}
		next := benchSamples{
			"BenchmarkFoo-8": {"sec/op": append(noisy(1e-3, 5), 9e-3)},
		}
		_, violations, issues := compare(old, next, testGates())
		assert.Empty(t, issues)
		assert.Empty(t, violations)
	})
}

// TestCompareMissingCandidateUnit pins the asymmetric missing-unit
// policy: a gated unit the baseline has but the candidate lost
// (e.g. -benchmem dropped from the candidate run) is a config error
// so the gate exits 2 instead of silently disabling that metric.
func TestCompareMissingCandidateUnit(t *testing.T) {
	old := benchSamples{
		"BenchmarkFoo-8": {
			"sec/op":    noisy(1e-3, 6),
			"allocs/op": {1000},
		},
	}
	next := benchSamples{
		"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 6)},
	}
	report, violations, issues := compare(old, next, testGates())
	assert.Empty(t, violations)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].msg,
		"allocs/op present in baseline but missing from candidate")
	assert.Contains(t, strings.Join(report, "\n"),
		"allocs/op missing from candidate")
}

// TestCompareTimeGateSampleCounts pins the asymmetric sample-count
// policy: too few candidate samples is a configuration error (the
// candidate run is under the workflow's control), while a short
// baseline is merely reported and not gated (the base run may
// legitimately be partial).
func TestCompareTimeGateSampleCounts(t *testing.T) {
	t.Run("too few candidate samples is a config issue", func(t *testing.T) {
		old := benchSamples{"BenchmarkFoo-8": {"sec/op": noisy(1e-3, 6)}}
		next := benchSamples{"BenchmarkFoo-8": {"sec/op": {2e-3}}}
		_, violations, issues := compare(old, next, testGates())
		assert.Empty(t, violations)
		require.Len(t, issues, 1)
		assert.Contains(t, issues[0].msg, "at least 5 candidate samples")
	})

	t.Run("short baseline is reported, not a config issue", func(t *testing.T) {
		old := benchSamples{"BenchmarkFoo-8": {"sec/op": {1e-3, 1e-3}}}
		next := benchSamples{"BenchmarkFoo-8": {"sec/op": noisy(9e-3, 6)}}
		report, violations, issues := compare(old, next, testGates())
		assert.Empty(t, violations)
		assert.Empty(t, issues)
		assert.Contains(t, strings.Join(report, "\n"),
			"baseline has only 2 sample(s)")
	})
}

func TestRender(t *testing.T) {
	sampleViolation := violation{
		name: "BenchmarkFoo-8", unit: "allocs/op",
		old: 1000, new: 2000, ratio: 2.0, maxRatio: 1.25,
	}
	tests := []struct {
		name     string
		r        results
		wantCode int
		wantOut  []string
	}{
		{
			name:     "clean run passes",
			r:        results{report: []string{"BenchmarkFoo-8: ok"}, newCount: 1},
			wantCode: 0,
			wantOut:  []string{"no regressions beyond thresholds"},
		},
		{
			name: "violations exit 1",
			r: results{
				violations: []violation{sampleViolation},
				newCount:   1,
			},
			wantCode: 1,
			wantOut:  []string{"1 regression(s)", "allocs/op regressed 2.00x"},
		},
		{
			name: "violations still print when a config issue forces exit 2",
			r: results{
				violations: []violation{sampleViolation},
				issues:     []configIssue{{name: "BenchmarkFoo-8", msg: "too few"}},
				newCount:   1,
			},
			wantCode: 2,
			wantOut: []string{
				"allocs/op regressed 2.00x",
				"invalid benchmark configuration",
			},
		},
		{
			name: "corrupted capture exits 2 and is described",
			r: results{
				newCount:  1,
				newSyntax: []string{"test:3: no iteration count"},
			},
			wantCode: 2,
			wantOut: []string{
				"candidate capture is corrupted",
				"no iteration count",
			},
		},
		{
			name:     "empty candidate exits 2",
			r:        results{newCount: 0},
			wantCode: 2,
			wantOut:  []string{"candidate output contains no benchmarks"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, code := render(tt.r)
			assert.Equal(t, tt.wantCode, code)
			for _, want := range tt.wantOut {
				assert.Contains(t, out, want)
			}
		})
	}
}

func TestViolationString(t *testing.T) {
	v := violation{
		name: "BenchmarkFoo-8", unit: "allocs/op",
		old: 1000, new: 2000, ratio: 2.0, maxRatio: 1.25,
	}
	got := v.String()
	assert.Contains(t, got, "BenchmarkFoo-8")
	assert.Contains(t, got, "allocs/op regressed 2.00x")
	assert.Contains(t, got, "limit 1.25x")
}
