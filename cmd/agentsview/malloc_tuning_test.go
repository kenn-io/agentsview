package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The daemon installs each glibc malloc knob only when the operator has
// not already set the matching environment variable.
func TestPlanMallocTuning(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want mallocTuningPlan
	}{
		{
			name: "sets both knobs when the environment is clean",
			env:  map[string]string{},
			want: mallocTuningPlan{SetArenaMax: true, SetTrimThreshold: true},
		},
		{
			name: "leaves the arena count to the operator",
			env:  map[string]string{"MALLOC_ARENA_MAX": "8"},
			want: mallocTuningPlan{SetArenaMax: false, SetTrimThreshold: true},
		},
		{
			name: "leaves the trim threshold to the operator",
			env:  map[string]string{"MALLOC_TRIM_THRESHOLD_": "131072"},
			want: mallocTuningPlan{SetArenaMax: true, SetTrimThreshold: false},
		},
		{
			name: "defers on both when both are set",
			env: map[string]string{
				"MALLOC_ARENA_MAX":       "1",
				"MALLOC_TRIM_THRESHOLD_": "0",
			},
			want: mallocTuningPlan{SetArenaMax: false, SetTrimThreshold: false},
		},
		{
			name: "treats an empty value as unset, matching glibc",
			env: map[string]string{
				"MALLOC_ARENA_MAX":       "",
				"MALLOC_TRIM_THRESHOLD_": "",
			},
			want: mallocTuningPlan{SetArenaMax: true, SetTrimThreshold: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planMallocTuning(func(key string) string {
				return tt.env[key]
			})
			assert.Equal(t, tt.want, got)
		})
	}
}
