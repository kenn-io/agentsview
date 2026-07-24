package artifact

import "errors"

const (
	originStateKey    = "artifact_origin_id"
	segmentTargetSize = int64(32 << 20)

	// Cardinality caps complement the byte caps: 4,096 records keeps one
	// segment's decoded object graph bounded, while 32,768 records and 256 MiB
	// leave ample room for unusually long sessions without letting many valid
	// chunks amplify during aggregation. Sixteen references accommodate uneven
	// 32 MiB chunks; the aggregate byte cap remains the final session bound.
	maxManifestSegments    = 16
	maxManifestUsageEvents = 32_768
	maxSegmentMessages     = 4_096
	maxSessionMessages     = 32_768
	maxSessionDecodedBytes = int64(256 << 20)

	// Nested collections need independent caps because compact empty objects can
	// amplify far beyond the decoded byte budget when unmarshaled. A message may
	// still describe unusually wide tool fan-out, and one tool may retain a long
	// result history. Segment totals keep one decoded chunk modest; session totals
	// allow eight full nested-budget segments, matching the message-count ratio.
	maxMessageToolCalls    = 256
	maxToolResultEvents    = 1_024
	maxSegmentToolCalls    = 8_192
	maxSegmentResultEvents = 32_768
	maxSessionToolCalls    = 65_536
	maxSessionResultEvents = 262_144
)

const (
	checkpointFloorPageSize = 128
	checkpointDecodedLimit  = int64(64 << 20)
)

// artifactLimits bounds decoded collection cardinality in addition to raw
// bytes. The production values are intentionally generous for real sessions
// while preventing small JSON records from amplifying into unbounded Go
// object graphs.
type artifactLimits struct {
	manifestSegments    int
	manifestUsageEvents int
	segmentMessages     int
	sessionMessages     int
	sessionDecodedBytes int64
	messageToolCalls    int
	toolResultEvents    int
	segmentToolCalls    int
	segmentResultEvents int
	sessionToolCalls    int
	sessionResultEvents int
}

func productionArtifactLimits() artifactLimits {
	return artifactLimits{
		manifestSegments:    maxManifestSegments,
		manifestUsageEvents: maxManifestUsageEvents,
		segmentMessages:     maxSegmentMessages,
		sessionMessages:     maxSessionMessages,
		sessionDecodedBytes: maxSessionDecodedBytes,
		messageToolCalls:    maxMessageToolCalls,
		toolResultEvents:    maxToolResultEvents,
		segmentToolCalls:    maxSegmentToolCalls,
		segmentResultEvents: maxSegmentResultEvents,
		sessionToolCalls:    maxSessionToolCalls,
		sessionResultEvents: maxSessionResultEvents,
	}
}

type nestedCollectionCounts struct {
	toolCalls    int
	resultEvents int
}

type segmentPreflight struct {
	records [][]byte
	nested  nestedCollectionCounts
}

func exceedsCollectionLimit(current, additional, limit int) bool {
	return current > limit || additional > limit-current
}

var errFutureArtifactVersion = errors.New("future artifact version")
