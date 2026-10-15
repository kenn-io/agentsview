package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// TestSummarizeToolResultEventsContextLatestEventAlwaysDecides covers which
// event decides a call's summary and length, walking backward from the end
// and skipping any event that doesn't participate
// (db.ToolResultEventParticipates): a withheld correction (blank Content, a
// kept ContentLength, SummaryParticipates explicitly true) always wins over
// an earlier event's stale real text, a genuinely empty, non-participating
// trailing event (blank, zero length, no explicit participation) is skipped
// rather than treated as an authoritative correction, and a withheld
// correction still wins even when a non-participating empty event trails
// behind it.
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
				{Content: "", ContentLength: 22, SummaryParticipates: new(true)},
				{Content: realText, ContentLength: len(realText)},
			},
			wantText: realText, wantLength: len(realText),
		},
		{
			name: "real then withheld: the later withheld event decides, not the earlier text",
			events: []db.ToolResultEvent{
				{Content: realText, ContentLength: len(realText)},
				{Content: "", ContentLength: 22, SummaryParticipates: new(true)},
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
		{
			// roborev's exact repro: a withheld correction must still win
			// even with a non-participating empty event trailing behind it.
			name: "real, then withheld, then a trailing empty event: the withheld correction still wins",
			events: []db.ToolResultEvent{
				{Content: realText, ContentLength: len(realText)},
				{Content: "", ContentLength: 22, SummaryParticipates: new(true)},
				{Content: "", ContentLength: 0},
			},
			wantText: "", wantLength: 22,
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
