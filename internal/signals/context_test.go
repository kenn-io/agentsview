package signals

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeContextPressure_NoData(t *testing.T) {
	got := ComputeContextPressure(nil, 0, "")
	assert.Equal(t, 0, got.CompactionCount)
	assert.Nil(t, got.PressureMax)
}

func TestCountCompactions(t *testing.T) {
	tests := []struct {
		name   string
		tokens []ContextTokenRow
		want   int
	}{
		{
			"empty",
			nil,
			0,
		},
		{
			"steady growth no drops",
			[]ContextTokenRow{
				{ContextTokens: 1000, HasContextTokens: true},
				{ContextTokens: 2000, HasContextTokens: true},
				{ContextTokens: 3000, HasContextTokens: true},
				{ContextTokens: 4000, HasContextTokens: true},
			},
			0,
		},
		{
			"one compaction over 30 percent drop",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 6000, HasContextTokens: true},
			},
			1,
		},
		{
			"two compactions",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 5000, HasContextTokens: true},
				{ContextTokens: 8000, HasContextTokens: true},
				{ContextTokens: 3000, HasContextTokens: true},
			},
			2,
		},
		{
			"drop exactly 30 percent not compaction",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 7000, HasContextTokens: true},
			},
			0,
		},
		{
			"drop 29 percent not compaction",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 7100, HasContextTokens: true},
			},
			0,
		},
		{
			"drop 31 percent is compaction",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 6900, HasContextTokens: true},
			},
			1,
		},
		{
			"skip rows without HasContextTokens",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
				{ContextTokens: 0, HasContextTokens: false},
				{ContextTokens: 0, HasContextTokens: false},
				{ContextTokens: 5000, HasContextTokens: true},
			},
			1,
		},
		{
			"all rows without HasContextTokens",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: false},
				{ContextTokens: 5000, HasContextTokens: false},
			},
			0,
		},
		{
			"single row no compaction",
			[]ContextTokenRow{
				{ContextTokens: 10000, HasContextTokens: true},
			},
			0,
		},
		{
			"drop from zero prev not compaction",
			[]ContextTokenRow{
				{ContextTokens: 0, HasContextTokens: true},
				{ContextTokens: 5000, HasContextTokens: true},
			},
			0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, countCompactions(tt.tokens))
		})
	}
}

func TestComputePressure(t *testing.T) {
	tests := []struct {
		name     string
		peak     int
		model    string
		wantNil  bool
		wantVal  float64
		wantPrec float64 // tolerance for float comparison
	}{
		{
			"known model claude-sonnet-4-5",
			100_000, "claude-sonnet-4-5",
			false, 0.5, 1e-9,
		},
		{
			"known model full window",
			200_000, "claude-sonnet-4-5",
			false, 1.0, 1e-9,
		},
		{
			"known model gpt-4o",
			64_000, "gpt-4o",
			false, 0.5, 1e-9,
		},
		{
			"unknown model returns nil",
			100_000, "unknown-model-xyz",
			true, 0, 0,
		},
		{
			"zero peak returns nil",
			0, "claude-sonnet-4-5",
			true, 0, 0,
		},
		{
			"negative peak returns nil",
			-1, "claude-sonnet-4-5",
			true, 0, 0,
		},
		{
			"empty model returns nil",
			100_000, "",
			true, 0, 0,
		},
		{
			"prefix match with version suffix",
			100_000, "claude-sonnet-4-5-20250101",
			false, 0.5, 1e-9,
		},
		{
			"prefix match gpt-4o-mini before gpt-4o",
			64_000, "gpt-4o-mini",
			false, 0.5, 1e-9,
		},
		{
			"prefix match gpt-4o-mini with suffix",
			64_000, "gpt-4o-mini-2025-01-01",
			false, 0.5, 1e-9,
		},
		{
			"gemini prefix match",
			500_000, "gemini-2.5-pro-preview",
			false, 0.5, 1e-9,
		},
		{
			"o3 exact match",
			100_000, "o3",
			false, 0.5, 1e-9,
		},
		{
			"o3 prefix match",
			100_000, "o3-2025-04-16",
			false, 0.5, 1e-9,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePressure(tt.peak, tt.model)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.LessOrEqual(t, math.Abs(*got-tt.wantVal), tt.wantPrec)
		})
	}
}

func TestLookupWindowSize(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  int
	}{
		{"exact claude-opus-4-6", "claude-opus-4-6", 1_000_000},
		{"exact gpt-4o-mini", "gpt-4o-mini", 128_000},
		{"exact gpt-4o", "gpt-4o", 128_000},
		{
			"prefix claude-sonnet-4-5-20250101",
			"claude-sonnet-4-5-20250101", 200_000,
		},
		{
			"prefix gpt-4o-mini-2025",
			"gpt-4o-mini-2025", 128_000,
		},
		{"unknown", "llama-3", 0},
		{"empty", "", 0},
		{
			"gpt-4o with date suffix",
			"gpt-4o-2024-08-06", 128_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lookupWindowSize(tt.model))
		})
	}
}

func TestComputeContextPressure_Combined(t *testing.T) {
	tokens := []ContextTokenRow{
		{ContextTokens: 50000, HasContextTokens: true},
		{ContextTokens: 100000, HasContextTokens: true},
		{ContextTokens: 150000, HasContextTokens: true},
		{ContextTokens: 50000, HasContextTokens: true},
		{ContextTokens: 80000, HasContextTokens: true},
	}
	got := ComputeContextPressure(
		tokens, 150000, "claude-sonnet-4-5",
	)
	assert.Equal(t, 1, got.CompactionCount)
	require.NotNil(t, got.PressureMax)
	assert.LessOrEqual(t, math.Abs(*got.PressureMax-0.75), 1e-9)
}

func TestCountMidTaskCompactions(t *testing.T) {
	tests := []struct {
		name             string
		boundaryOrdinals []int
		toolCalls        []ToolCallOrdinal
		wantMidTaskCount int
	}{
		{
			name:             "no boundaries",
			toolCalls:        []ToolCallOrdinal{{1, "Read"}},
			wantMidTaskCount: 0,
		},
		{
			name:             "no tool calls",
			boundaryOrdinals: []int{10},
			wantMidTaskCount: 0,
		},
		{
			name:             "boundary at start no tools before",
			boundaryOrdinals: []int{1},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"}, {3, "Edit"},
			},
			wantMidTaskCount: 0,
		},
		{
			name:             "boundary at end no tools after",
			boundaryOrdinals: []int{10},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"}, {3, "Edit"},
			},
			wantMidTaskCount: 0,
		},
		{
			name:             "no overlap between before and after",
			boundaryOrdinals: []int{5},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"},
				{3, "Read"},
				{4, "Edit"},
				{6, "Bash"},
				{7, "Grep"},
				{8, "Glob"},
			},
			wantMidTaskCount: 0,
		},
		{
			name:             "overlap one tool insufficient",
			boundaryOrdinals: []int{5},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"},
				{3, "Bash"},
				{4, "Grep"},
				{6, "Read"},
				{7, "Glob"},
				{8, "Write"},
			},
			wantMidTaskCount: 0,
		},
		{
			name:             "overlap two tools triggers",
			boundaryOrdinals: []int{5},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"},
				{3, "Edit"},
				{4, "Bash"},
				{6, "Read"},
				{7, "Edit"},
				{8, "Grep"},
			},
			wantMidTaskCount: 1,
		},
		{
			name:             "multiple boundaries each evaluated",
			boundaryOrdinals: []int{5, 15},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"},
				{3, "Edit"},
				{6, "Read"},
				{7, "Edit"},
				{12, "Bash"},
				{13, "Grep"},
				{16, "Bash"},
				{17, "Grep"},
			},
			wantMidTaskCount: 2,
		},
		{
			name:             "before window limited to last 10",
			boundaryOrdinals: []int{20},
			// 11 distinct names before — only last 10 count.
			toolCalls: []ToolCallOrdinal{
				{1, "Read"},
				{2, "Read"},
				{3, "T1"},
				{4, "T2"},
				{5, "T3"},
				{6, "T4"},
				{7, "T5"},
				{8, "T6"},
				{9, "T7"},
				{10, "T8"},
				{11, "T9"},
				{12, "T10"},
				// After: includes Read which was first 2 ordinals,
				// pushed out of the last-10 window.
				{21, "Read"},
				{22, "Read"},
			},
			// "Read" not in last-10 before window, so no overlap.
			wantMidTaskCount: 0,
		},
		{
			name:             "after window limited to first 5",
			boundaryOrdinals: []int{5},
			toolCalls: []ToolCallOrdinal{
				{2, "Read"},
				{3, "Edit"},
				// First 5 after the boundary: A B C D E
				{6, "A"},
				{7, "B"},
				{8, "C"},
				{9, "D"},
				{10, "E"},
				// Read/Edit appear later, beyond the after window.
				{11, "Read"},
				{12, "Edit"},
			},
			wantMidTaskCount: 0,
		},
		{
			// Before has [Bash, Read]; after has Bash repeated.
			// Raw match count is 4, but only one distinct tool
			// name overlaps — should not trigger the threshold.
			name:             "single tool repeated does not inflate",
			boundaryOrdinals: []int{5},
			toolCalls: []ToolCallOrdinal{
				{2, "Bash"},
				{3, "Read"},
				{6, "Bash"},
				{7, "Bash"},
				{8, "Bash"},
				{9, "Bash"},
			},
			wantMidTaskCount: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CountMidTaskCompactions(
				tc.boundaryOrdinals, tc.toolCalls,
			)
			assert.Equal(t, tc.wantMidTaskCount, got)
		})
	}
}
