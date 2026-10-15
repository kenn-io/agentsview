package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// TestSummarizeToolResultEventsContextLatestEventAlwaysDecides covers both
// directions of a call with two result events, one real-text and one
// withheld by content policy (blank Content, a kept ContentLength): the
// chronologically later event — events[len(events)-1] — must always drive
// the summary and length, whether it is the withheld one or the real-text
// one. A later withheld correction must never fall back to an earlier
// event's stale real text.
func TestSummarizeToolResultEventsContextLatestEventAlwaysDecides(t *testing.T) {
	realText := "inline result"

	tests := []struct {
		name       string
		events     []db.ToolResultEvent
		wantText   string
		wantLength int
	}{
		{
			name: "withheld then real: the later real event decides",
			events: []db.ToolResultEvent{
				{Content: "", ContentLength: 22},
				{Content: realText, ContentLength: len(realText)},
			},
			wantText: realText, wantLength: len(realText),
		},
		{
			name: "real then withheld: the later withheld event decides, not the earlier text",
			events: []db.ToolResultEvent{
				{Content: realText, ContentLength: len(realText)},
				{Content: "", ContentLength: 22},
			},
			wantText: "", wantLength: 22,
		},
		{
			// A genuinely empty, non-participating trailing event (zero
			// length, not a withheld correction) must not discard a good
			// earlier summary.
			name: "real then a genuinely empty trailing event: the earlier text survives",
			events: []db.ToolResultEvent{
				{Content: realText, ContentLength: len(realText)},
				{Content: "", ContentLength: 0},
			},
			wantText: realText, wantLength: len(realText),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summary, length, err := ingest.SummarizeToolResultEventsContext(
				t.Context(), tc.events,
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantText, summary)
			assert.Equal(t, tc.wantLength, length)
		})
	}
}
