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
