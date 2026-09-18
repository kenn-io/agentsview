package db

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssignBucketDurationEdges(t *testing.T) {
	cases := []struct {
		v    float64
		want int
	}{
		{0, 0},
		{0.5, 0},
		{1, 1},
		{4.999, 1},
		{5, 2},
		{19.999, 2},
		{20, 3},
		{59.999, 3},
		{60, 4},
		{120, 5},
		{120.1, 5},
		{9999, 5},
	}
	for _, c := range cases {
		got := assignBucket(durationMinutesEdges, c.v)
		assert.Equal(t, c.want, got, "durationMinutes v=%v", c.v)
	}
}

func TestAssignBucketUserMessagesAll(t *testing.T) {
	cases := []struct {
		v    float64
		want int // index into userMessagesEdgesAll (7 edges → 6 buckets)
	}{
		{0, 0},
		{1, 0},
		{1.9, 0}, // scope_all bucket [0,2)
		{2, 1},
		{5, 1},
		{5.9, 1}, // [2,6)
		{6, 2},
		{15.9, 2}, // [6,16)
		{16, 3},
		{30.9, 3},
		{31, 4},
		{50.9, 4},
		{51, 5},
		{10000, 5},
	}
	for _, c := range cases {
		got := assignBucket(userMessagesEdgesAll, c.v)
		assert.Equal(t, c.want, got, "user_messages scope_all v=%v", c.v)
	}
}

func TestBuildEmptyBucketsTopIsUnbounded(t *testing.T) {
	b := buildEmptyBuckets(durationMinutesEdges)
	require.Len(t, b, 6)
	top := b[len(b)-1]
	assert.Nil(t, top.Edge[1], "top bucket hi should be nil (JSON null)")
	assert.False(t, math.IsInf(*top.Edge[0], 1),
		"top bucket lo should be finite, got +Inf")
}

// TestEdgeListsShape pins every v1 edge list to the spec's bucket counts
// and guards against accidental reordering. Each entry also catches the
// unused-linter on edge lists whose first consumer lives in a later task.
func TestEdgeListsShape(t *testing.T) {
	cases := []struct {
		name        string
		edges       []float64
		wantBuckets int
		wantTopInf  bool
	}{
		{"durationMinutes", durationMinutesEdges, 6, true},
		{"userMessagesAll", userMessagesEdgesAll, 6, true},
		{"userMessagesHuman", userMessagesEdgesHuman, 5, true},
		{"peakContext", peakContextEdges, 6, true},
		{"toolsPerTurn", toolsPerTurnEdges, 6, true},
		{"cacheHitRatio", cacheHitRatioEdges, 5, false}, // inclusive of 1.0 via 1.000001
	}
	for _, c := range cases {
		assert.Equal(t, c.wantBuckets, len(c.edges)-1, "%s: buckets", c.name)
		for i := 1; i < len(c.edges); i++ {
			assert.Greater(t, c.edges[i], c.edges[i-1],
				"%s: edges must be strictly increasing; edges[%d]=%v <= edges[%d]=%v",
				c.name, i, c.edges[i], i-1, c.edges[i-1])
		}
		topIsInf := math.IsInf(c.edges[len(c.edges)-1], 1)
		assert.Equal(t, c.wantTopInf, topIsInf, "%s: top edge +Inf", c.name)
	}
}
