package artifact

import (
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

var (
	// ErrArtifactExportRejected identifies a deterministic session-shape
	// failure that can be finalized without retrying the same generation
	// forever.
	ErrArtifactExportRejected = errors.New("artifact export rejected")

	// ErrArtifactExportUnsettled identifies a full export that made progress
	// but reached its bounded drain-round limit while work remained queued.
	ErrArtifactExportUnsettled = errors.New("artifact export queue did not settle")
)

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

func artifactExportLoadLimits(limits artifactLimits) db.ArtifactExportLoadLimits {
	return db.ArtifactExportLoadLimits{
		Messages:            limits.sessionMessages,
		UsageEvents:         limits.manifestUsageEvents,
		MessageToolCalls:    limits.messageToolCalls,
		ToolResultEvents:    limits.toolResultEvents,
		SessionToolCalls:    limits.sessionToolCalls,
		SessionResultEvents: limits.sessionResultEvents,
		MessageBytes:        limits.sessionDecodedBytes,
		UsageBytes:          manifestDecodedLimit,
	}
}

func rejectArtifactExportf(format string, args ...any) error {
	return fmt.Errorf(
		"%w: %s", ErrArtifactExportRejected, fmt.Sprintf(format, args...),
	)
}

func classifyArtifactExportLoadError(err error) error {
	if errors.Is(err, db.ErrArtifactExportLimit) {
		return fmt.Errorf("%w: %w", ErrArtifactExportRejected, err)
	}
	return err
}

func isDeterministicArtifactExportError(err error) bool {
	return errors.Is(err, ErrArtifactExportRejected) ||
		errors.Is(err, db.ErrArtifactExportLimit)
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

type futureArtifactVersionError struct {
	Kind    Kind
	Version int
}

func (e *futureArtifactVersionError) Error() string {
	return fmt.Sprintf("%s has future artifact version %d", e.Kind, e.Version)
}

func (e *futureArtifactVersionError) Unwrap() error {
	return errFutureArtifactVersion
}
