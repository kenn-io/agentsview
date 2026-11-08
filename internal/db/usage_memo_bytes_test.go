package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
)

func TestTopSessionMemoCountsGroupMetadata(t *testing.T) {
	base := db.TopSessionBytes(db.TopSessionEntry{})
	for _, tc := range []struct {
		name  string
		entry db.TopSessionEntry
		bytes int64
	}{
		{"machine", db.TopSessionEntry{Machine: "host-a.example"}, 14},
		{"key", db.TopSessionEntry{GroupKey: "job-a"}, 5},
		{"label", db.TopSessionEntry{GroupLabel: "Daily digest"}, 12},
		{"title", db.TopSessionEntry{SessionName: "Daily digest · Oct 07 12:00"}, 28},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.bytes, db.TopSessionBytes(tc.entry)-base)
		})
	}
}

func TestDailyUsageMemoCountsPairwiseSides(t *testing.T) {
	sessions := make([]string, 8192)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	models := make(map[string]db.UsagePairwiseValue, 256)
	for i := range 256 {
		models[fmt.Sprintf("model-%d", i)] = db.UsagePairwiseValue{Sessions: sessions}
	}
	result := db.DailyUsageResult{Pairwise: &db.UsagePairwiseSides{Models: models}}
	var reads kept.Reads[db.DailyUsageResult]
	loads := 0
	for range 2 {
		_, err := reads.Do(t.Context(), "selection", "revision", func(context.Context) (db.DailyUsageResult, error) { loads++; return result, nil })
		require.NoError(t, err)
	}
	assert.Equal(t, 2, loads)
}
