package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaudeSplitRunStart(t *testing.T) {
	line := func(s string) string { return s + "\n" }
	user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
	run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`)
	run2 := line(`{"type":"assistant","uuid":"a2","message":{"id":"m","content":[{"type":"tool_use","id":"t2","name":"Read","input":{}}]}}`)
	result := line(`{"type":"user","uuid":"r1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`)
	attach := line(`{"type":"attachment","uuid":"at1","content":"queued"}`)
	other := line(`{"type":"assistant","uuid":"o1","message":{"id":"m2","content":[{"type":"text","text":"other"}]}}`)
	broken := line(`{"type":"assistant","uuid":"a1"`)

	var long strings.Builder
	long.WriteString(user)
	for range 4096 {
		long.WriteString(line(`{"type":"assistant","uuid":"a` + strings.Repeat("x", 8) +
			`","message":{"id":"m","content":[{"type":"text","text":"` +
			strings.Repeat("y", 40) + `"}]}}`))
	}

	tests := []struct {
		name        string
		stored      []string // records before the sync offset
		appended    []string // records after it
		wantVerdict ClaudeSplitVerdict
		wantStart   int64
	}{
		{
			name:        "run straddles the offset",
			stored:      []string{user, run1},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitFound,
			wantStart:   int64(len(user)),
		},
		{
			name:        "attachment inside the run is skipped",
			stored:      []string{user, run1, attach},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitFound,
			wantStart:   int64(len(user)),
		},
		{
			name:        "run starting at the file start",
			stored:      []string{run1},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitFound,
			wantStart:   0,
		},
		{
			name:        "tool result between parallel tool calls ends the run",
			stored:      []string{user, run1, result},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitNone,
		},
		{
			name:        "different message id before the offset",
			stored:      []string{user, run1, other},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitNone,
		},
		{
			name:        "malformed record",
			stored:      []string{user, broken},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitUnknown,
		},
		{
			name:        "run spans a read chunk",
			stored:      []string{long.String()},
			appended:    []string{run2},
			wantVerdict: ClaudeSplitFound,
			wantStart:   int64(len(user)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := strings.Join(tt.stored, "")
			path := createTestFile(t, "split.jsonl",
				stored+strings.Join(tt.appended, ""))

			start, verdict := ClaudeSplitRunStart(path, int64(len(stored)), "m")
			assert.Equal(t, tt.wantVerdict, verdict)
			if tt.wantVerdict == ClaudeSplitFound {
				assert.Equal(t, tt.wantStart, start)
			}
		})
	}
}
