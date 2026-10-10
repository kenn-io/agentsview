package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
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

func TestTopSessionMemoCountsGroupMetadata(t *testing.T) {
	base := topSessionBytes(db.TopSessionEntry{})
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
			assert.Equal(t, tc.bytes, topSessionBytes(tc.entry)-base)
		})
	}
}
