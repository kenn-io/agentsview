package rawsync

import (
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// JobHealthQuery supplies the thresholds used by a hosted raw-sync health
// observation.
type JobHealthQuery struct {
	MaxAttempts       int32 `json:"max_attempts"`
	StaleAfterSeconds int32 `json:"stale_after_seconds"`
}

// Validate rejects thresholds that cannot describe a useful observation.
func (q JobHealthQuery) Validate() error {
	if q.MaxAttempts <= 0 {
		return fmt.Errorf("%w: max attempts must be positive", ErrInvalid)
	}
	if q.StaleAfterSeconds <= 0 {
		return fmt.Errorf("%w: stale-after seconds must be positive", ErrInvalid)
	}
	return nil
}

// JobHealthReport contains tenant-scoped raw parse-job observations.
type JobHealthReport struct {
	ObservedAt        time.Time `json:"observed_at"`
	MaxAttempts       int32     `json:"max_attempts"`
	StaleAfterSeconds int32     `json:"stale_after_seconds"`

	OrphanedManifests      []OrphanedManifest  `json:"orphaned_manifests"`
	OrphanedManifestCount  int64               `json:"orphaned_manifest_count"`
	ExpiredLeases          []ExpiredLease      `json:"expired_leases"`
	ExpiredLeaseCount      int64               `json:"expired_lease_count"`
	FailedJobsByErrorClass []JobFailureClass   `json:"failed_jobs_by_error_class"`
	FailedJobCount         int64               `json:"failed_job_count"`
	RetryingNearLimit      []JobAttemptWarning `json:"retrying_near_limit"`
	RetryingNearLimitCount int64               `json:"retrying_near_limit_count"`
	StaleSourceHeads       []StaleSourceHead   `json:"stale_source_heads"`
	StaleSourceHeadCount   int64               `json:"stale_source_head_count"`
}

// OrphanedManifest identifies a committed current head without a parse job.
type OrphanedManifest struct {
	ManifestID       string           `json:"manifest_id"`
	DeviceID         string           `json:"device_id"`
	Provider         parser.AgentType `json:"provider"`
	ConfiguredRootID string           `json:"configured_root_id"`
	SourceKeySHA256  string           `json:"source_key_sha256"`
	Generation       int64            `json:"generation"`
	Kind             ManifestKind     `json:"kind"`
	AcceptedAt       time.Time        `json:"accepted_at"`
}

// ExpiredLease identifies a parse job whose lease expired while still leased.
type ExpiredLease struct {
	JobID             int64            `json:"job_id"`
	DeviceID          string           `json:"device_id"`
	ManifestID        string           `json:"manifest_id"`
	ProcessingVersion string           `json:"processing_version"`
	Provider          parser.AgentType `json:"provider"`
	ConfiguredRootID  string           `json:"configured_root_id"`
	SourceKeySHA256   string           `json:"source_key_sha256"`
	AttemptCount      int              `json:"attempt_count"`
	LeaseExpiresAt    time.Time        `json:"lease_expires_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// JobFailureClass groups failed parse jobs by their stored error class.
type JobFailureClass struct {
	ErrorClass      string    `json:"error_class"`
	JobCount        int64     `json:"job_count"`
	LatestFailureAt time.Time `json:"latest_failure_at"`
}

// JobAttemptWarning identifies a retrying parse job near the caller's limit.
type JobAttemptWarning struct {
	JobID             int64            `json:"job_id"`
	DeviceID          string           `json:"device_id"`
	ManifestID        string           `json:"manifest_id"`
	ProcessingVersion string           `json:"processing_version"`
	Provider          parser.AgentType `json:"provider"`
	ConfiguredRootID  string           `json:"configured_root_id"`
	SourceKeySHA256   string           `json:"source_key_sha256"`
	AttemptCount      int              `json:"attempt_count"`
	AvailableAt       time.Time        `json:"available_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	LastErrorClass    string           `json:"last_error_class"`
}

// StaleSourceHead identifies a committed source head older than the query.
type StaleSourceHead struct {
	DeviceID         string           `json:"device_id"`
	Provider         parser.AgentType `json:"provider"`
	ConfiguredRootID string           `json:"configured_root_id"`
	SourceKeySHA256  string           `json:"source_key_sha256"`
	ManifestID       string           `json:"manifest_id"`
	Generation       int64            `json:"generation"`
	Kind             ManifestKind     `json:"kind"`
	UpdatedAt        time.Time        `json:"updated_at"`
}
