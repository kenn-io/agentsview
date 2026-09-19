package db

import "math"

// Schema v1 bucket boundaries. Changing any of these requires a
// schema_version bump (see session-analytics-design spec).

// Half-open intervals [lo, hi). The last bucket has hi = +Inf.
var durationMinutesEdges = []float64{0, 1, 5, 20, 60, 120, math.Inf(1)}

// user_messages scope_all: [0,2), [2,5], [6,15], [16,30], [31,50], [51,inf)
// user_messages scope_human: [2,5], [6,15], [16,30], [31,50], [51,inf)
// Values below 2 are filtered before accumulating scope_human, preserving
// the v1 bucket shape while keeping human mean and bucket counts consistent.
// Represented as two separate edge lists for clarity.
var (
	userMessagesEdgesAll   = []float64{0, 2, 6, 16, 31, 51, math.Inf(1)}
	userMessagesEdgesHuman = []float64{2, 6, 16, 31, 51, math.Inf(1)}
)

var (
	peakContextEdges  = []float64{0, 10_000, 50_000, 100_000, 150_000, 200_000, math.Inf(1)}
	toolsPerTurnEdges = []float64{0, 1, 2, 4, 7, 11, math.Inf(1)}
)

// cacheHitRatioEdges uses 1.000001 as an internal sentinel so values of
// exactly 1.0 fall into the last bucket under assignBucket's half-open
// rule. This sentinel must NOT be exposed via the v1 JSON schema; use
// buildCacheHitRatioBuckets to materialize buckets with the public
// upper edge clamped back to 1.0.
var cacheHitRatioEdges = []float64{0, 0.25, 0.5, 0.75, 0.95, 1.000001}

// assignBucket returns the index i such that edges[i] <= v < edges[i+1],
// or -1 if v < edges[0] or v >= edges[len-1] (shouldn't happen given Inf upper).
func assignBucket(edges []float64, v float64) int {
	for i := range len(edges) - 1 {
		if v >= edges[i] && v < edges[i+1] {
			return i
		}
	}
	return -1
}

// buildEmptyBuckets returns a pre-sized bucket slice matching edges[i]..edges[i+1].
// Top bucket's hi is represented as JSON null by leaving Edge[1] as nil pointer.
func buildEmptyBuckets(edges []float64) []DistributionBucketV1 {
	out := make([]DistributionBucketV1, 0, len(edges)-1)
	for i := range len(edges) - 1 {
		lo := edges[i]
		var hiPtr *float64
		if !math.IsInf(edges[i+1], 1) {
			hi := edges[i+1]
			hiPtr = &hi
		}
		loPtr := lo
		out = append(out, DistributionBucketV1{
			Edge:  [2]*float64{&loPtr, hiPtr},
			Count: 0,
		})
	}
	return out
}

// buildCacheHitRatioBuckets returns the cache-hit-ratio buckets with the
// internal 1.000001 sentinel rewritten to 1.0 for the JSON schema. The
// sentinel exists only so assignBucket can place ratio == 1.0 into the
// last bucket; the JSON contract advertises a clean [0.95, 1.0] band.
func buildCacheHitRatioBuckets() []DistributionBucketV1 {
	buckets := buildEmptyBuckets(cacheHitRatioEdges)
	if n := len(buckets); n > 0 && buckets[n-1].Edge[1] != nil {
		one := 1.0
		buckets[n-1].Edge[1] = &one
	}
	return buckets
}
