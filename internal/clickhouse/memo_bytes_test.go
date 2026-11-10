package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAggregateRowMemoCountsGroupMetadata(t *testing.T) {
	base := aggregateRowBytes(CHUsageAggregateRow{})
	for _, tc := range []struct {
		name  string
		row   CHUsageAggregateRow
		bytes int64
	}{
		{"key", CHUsageAggregateRow{GroupKey: "job-a"}, 5},
		{"title", CHUsageAggregateRow{SessionName: "Daily digest · Oct 07 12:00"}, 28},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.bytes, aggregateRowBytes(tc.row)-base)
		})
	}
}

func TestDailyUsageMemoCountsDimensionEvidence(t *testing.T) {
	base := dailyUsageRowBytes(chDailyUsageGroupRow{})
	row := chDailyUsageGroupRow{InexactModels: []string{"model-a", "model-b"}, InexactProjects: []string{"project"}}
	assert.Equal(t, int64(3*16+21), dailyUsageRowBytes(row)-base)
}
