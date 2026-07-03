package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBench(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]benchResult
	}{
		{
			name: "full benchmem line",
			input: "goos: linux\n" +
				"BenchmarkFoo-8   \t 100\t 1234567 ns/op\t 2345 B/op\t 67 allocs/op\n" +
				"PASS\nok  \tpkg\t1.2s\n",
			want: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 1234567, "B/op": 2345, "allocs/op": 67,
				},
			},
		},
		{
			name: "multiple counts keep the minimum per metric",
			input: "BenchmarkFoo-8 100 200 ns/op 50 B/op 9 allocs/op\n" +
				"BenchmarkFoo-8 100 150 ns/op 60 B/op 8 allocs/op\n" +
				"BenchmarkFoo-8 100 180 ns/op 40 B/op 10 allocs/op\n",
			want: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 150, "B/op": 40, "allocs/op": 8,
				},
			},
		},
		{
			name:  "time-only line without -benchmem",
			input: "BenchmarkBar-4 500 0.5 ns/op\n",
			want: map[string]benchResult{
				"BenchmarkBar-4": {"ns/op": 0.5},
			},
		},
		{
			name: "log lines and headers are ignored",
			input: "2026/07/03 10:20:36 discovered 40 files in 0s\n" +
				"BenchmarkSync says hello\n" +
				"cpu: Apple M5 Max\n",
			want: map[string]benchResult{},
		},
		{
			name:  "custom ReportMetric units are kept",
			input: "BenchmarkBaz-2 10 900 ns/op 3 sessions/op\n",
			want: map[string]benchResult{
				"BenchmarkBaz-2": {"ns/op": 900, "sessions/op": 3},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBench(strings.NewReader(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func testGates() []gate {
	return []gate{
		{unit: "allocs/op", maxRatio: 1.25, floor: 64},
		{unit: "B/op", maxRatio: 1.35, floor: 16_384},
		{unit: "ns/op", maxRatio: 2.0, floor: 100_000},
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name       string
		old, new   map[string]benchResult
		wantUnits  []string // units of expected violations, in order
		wantReport []string // substrings that must appear in the report
	}{
		{
			name: "within thresholds passes",
			old: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 1_000_000, "B/op": 100_000, "allocs/op": 1000,
				},
			},
			new: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 1_500_000, "B/op": 120_000, "allocs/op": 1100,
				},
			},
			wantUnits: nil,
		},
		{
			name: "alloc regression fails",
			old: map[string]benchResult{
				"BenchmarkFoo-8": {"ns/op": 1_000_000, "allocs/op": 1000},
			},
			new: map[string]benchResult{
				"BenchmarkFoo-8": {"ns/op": 1_000_000, "allocs/op": 2000},
			},
			wantUnits: []string{"allocs/op"},
		},
		{
			name: "time blowup fails",
			old: map[string]benchResult{
				"BenchmarkFoo-8": {"ns/op": 1_000_000},
			},
			new: map[string]benchResult{
				"BenchmarkFoo-8": {"ns/op": 5_000_000},
			},
			wantUnits: []string{"ns/op"},
		},
		{
			name: "tiny baseline below floor is not gated",
			old: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 500, "B/op": 128, "allocs/op": 3,
				},
			},
			new: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 5000, "B/op": 1280, "allocs/op": 30,
				},
			},
			wantUnits:  nil,
			wantReport: []string{"not gated"},
		},
		{
			name: "new benchmark without baseline is reported, not gated",
			old:  map[string]benchResult{},
			new: map[string]benchResult{
				"BenchmarkNew-8": {"ns/op": 1_000_000, "allocs/op": 99999},
			},
			wantUnits:  nil,
			wantReport: []string{"no baseline to compare"},
		},
		{
			name: "removed benchmark is reported, not gated",
			old: map[string]benchResult{
				"BenchmarkGone-8": {"ns/op": 1_000_000},
			},
			new:        map[string]benchResult{},
			wantUnits:  nil,
			wantReport: []string{"missing from candidate"},
		},
		{
			name: "multiple regressions are all reported",
			old: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 1_000_000, "B/op": 100_000, "allocs/op": 1000,
				},
			},
			new: map[string]benchResult{
				"BenchmarkFoo-8": {
					"ns/op": 9_000_000, "B/op": 900_000, "allocs/op": 9000,
				},
			},
			wantUnits: []string{"allocs/op", "B/op", "ns/op"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, violations := compare(tt.old, tt.new, testGates())
			units := make([]string, 0, len(violations))
			for _, v := range violations {
				units = append(units, v.unit)
			}
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

func TestViolationString(t *testing.T) {
	v := violation{
		name: "BenchmarkFoo-8", unit: "allocs/op",
		old: 1000, new: 2000, ratio: 2.0, maxRatio: 1.25,
	}
	assert.Equal(
		t,
		"BenchmarkFoo-8: allocs/op regressed 2.00x "+
			"(1000 -> 2000, limit 1.25x)",
		v.String(),
	)
}
