package rawsync

import (
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// Status summarizes the raw-sync metadata owned by one tenant.
type Status struct {
	SourceHeads       []SourceHeadStatus  `json:"source_heads"`
	ParseJobs         ParseJobCounts      `json:"parse_jobs"`
	ActiveDeviceCount int64               `json:"active_device_count"`
	Devices           []DeviceStatus      `json:"devices"`
	Uploads           UploadStatusSummary `json:"uploads"`
}

// SourceHeadStatus describes one source head and its current parse state.
type SourceHeadStatus struct {
	DeviceID         string           `json:"device_id"`
	ConfiguredRootID string           `json:"configured_root_id"`
	Provider         parser.AgentType `json:"provider"`
	SourceKey        string           `json:"source_key"`
	Generation       int64            `json:"generation"`
	LastAcceptedAt   *time.Time       `json:"last_accepted_at"`
	ParsePending     bool             `json:"parse_pending"`
	ParseLeased      bool             `json:"parse_leased"`
	ParseFailed      bool             `json:"parse_failed"`
}

// ParseJobCounts groups parse jobs by their durable processing state.
type ParseJobCounts struct {
	Ready      int64 `json:"ready"`
	Leased     int64 `json:"leased"`
	Retrying   int64 `json:"retrying"`
	Complete   int64 `json:"complete"`
	Failed     int64 `json:"failed"`
	Superseded int64 `json:"superseded"`
}

// DeviceStatus describes one unrevoked device and its latest token issuance.
type DeviceStatus struct {
	DeviceID   string     `json:"device_id"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

// UploadStatusSummary describes the stored-open upload backlog.
type UploadStatusSummary struct {
	OpenCount         int64             `json:"open_count"`
	PendingBytes      int64             `json:"pending_bytes"`
	OldestOpenSession *OpenUploadStatus `json:"oldest_open_session"`
}

// OpenUploadStatus identifies the oldest stored-open upload.
type OpenUploadStatus struct {
	_         struct{}  `json:"-" nullable:"true"`
	UploadID  string    `json:"upload_id"`
	CreatedAt time.Time `json:"created_at"`
}
