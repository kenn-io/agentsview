package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTruncateWidthPreservesVisibleWidth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		width int
		want  string
	}{
		{name: "already fits", value: "abc", width: 3, want: "abc"},
		{name: "ascii", value: "abcdef", width: 4, want: "abc…"},
		{name: "wide unicode", value: "世界hello", width: 4, want: "世…"},
		{name: "ellipsis only", value: "abcdef", width: 1, want: "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, truncateWidth(tt.value, tt.width))
		})
	}
}

func TestTruncateWidthScalesLinearlyWithInputLength(t *testing.T) {
	smallInput := strings.Repeat("x", 256)
	largeInput := strings.Repeat("x", 4096)
	var smallOutput, largeOutput string

	smallStart := time.Now()
	for range 16 {
		smallOutput = truncateWidth(smallInput, 8)
	}
	smallDuration := time.Since(smallStart)
	largeStart := time.Now()
	largeOutput = truncateWidth(largeInput, 8)
	largeDuration := time.Since(largeStart)

	assert.Equal(t, "xxxxxxx…", smallOutput)
	assert.Equal(t, "xxxxxxx…", largeOutput)
	assert.Less(t, largeDuration, smallDuration*3,
		"16x more input must not cause quadratic truncation work")
}
