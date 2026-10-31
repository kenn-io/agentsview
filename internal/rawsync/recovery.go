package rawsync

import "time"

// RecoveryMetrics describes hosted processing lag and retained outcome history.
// Nil ages mean no known pending work; legacy provenance is counted as unknown.
type RecoveryMetrics struct {
	ParseLagSeconds               *float64           `json:"parse_lag_seconds"`
	OldestPendingSourceAgeSeconds *float64           `json:"oldest_pending_source_age_seconds"`
	OldestReadyAgeSeconds         *float64           `json:"oldest_ready_age_seconds"`
	OldestRetryingAgeSeconds      *float64           `json:"oldest_retrying_age_seconds"`
	OldestOpenUploadAgeSeconds    *float64           `json:"oldest_open_upload_age_seconds"`
	QueuedBytes                   int64              `json:"queued_bytes"`
	DegradedSources               DegradedSources    `json:"degraded_sources"`
	OutcomeBuckets                []JobOutcomeBucket `json:"outcome_buckets"`
}

// DegradedSources categories may overlap for the same current source.
type DegradedSources struct {
	IncompleteMembership    int64 `json:"incomplete_membership"`
	FailedWithoutProjection int64 `json:"failed_without_projection"`
	ServingOlderProjection  int64 `json:"serving_older_projection"`
	Unknown                 int64 `json:"unknown"`
}

// JobOutcomeBucket counts committed entries into a terminal or retry state.
type JobOutcomeBucket struct {
	BucketStart time.Time `json:"bucket_start"`
	Stage       string    `json:"stage"`
	Outcome     string    `json:"outcome"`
	Count       int64     `json:"count"`
}
