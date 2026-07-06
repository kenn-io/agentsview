package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTmpBenchPageComposition(t *testing.T) {
	d := testDB(t)
	for i := range benchContentSessions {
		sessionID := fmt.Sprintf("bench-search-%03d", i)
		require.NoError(t, d.UpsertSession(Session{
			ID: sessionID, Project: "bench", Machine: "local", Agent: "claude",
			MessageCount: benchContentMessages, UserMessageCount: 2,
		}))
		require.NoError(t, d.InsertMessages(benchContentSearchMessages(sessionID)))
	}
	for _, tc := range []struct{ pattern, mode string }{
		{"needle", "substring"}, {"runneedle", "fts"},
	} {
		page, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: tc.pattern, Mode: tc.mode, Limit: 50,
		})
		require.NoError(t, err)
		inRun, side := 0, 0
		var ords []int
		for _, m := range page.Matches {
			if m.Ordinal >= benchContentRunStart && m.Ordinal <= benchContentRunEnd {
				inRun++
			}
			if m.Sidechain {
				side++
			}
			ords = append(ords, m.Ordinal)
		}
		t.Logf("%s %s: %d matches, %d in-run, %d sidechain, ordinals=%v",
			tc.mode, tc.pattern, len(page.Matches), inRun, side, ords)
	}
}
