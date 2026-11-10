package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAggregateRowMemoCountsGroupMetadata(t *testing.T) {
	base := aggregateRowBytes(chUsageAggregateRow{})
	for _, tc := range []struct {
		name  string
		row   chUsageAggregateRow
		bytes int64
	}{
		{"key", chUsageAggregateRow{groupKey: "job-a"}, 5},
		{"title", chUsageAggregateRow{sessionName: "Daily digest · Oct 07 12:00"}, 28},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.bytes, aggregateRowBytes(tc.row)-base)
		})
	}
}

func TestDailyUsageMemoCountsDimensionEvidence(t *testing.T) {
	base := dailyUsageRowBytes(chDailyUsageGroupRow{})
	row := chDailyUsageGroupRow{inexactModels: []string{"model-a", "model-b"}, inexactProjects: []string{"project"}}
	assert.Equal(t, int64(3*16+21), dailyUsageRowBytes(row)-base)
}
