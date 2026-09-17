package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// ReadRawSyncStatus reads one tenant's raw-sync metadata from a consistent,
// read-only PostgreSQL snapshot.
func (s *RawIngestStore) ReadRawSyncStatus(
	ctx context.Context,
	identity rawsync.AuthIdentity,
) (rawsync.Status, error) {
	if err := validateRawIngestIdentity(identity); err != nil {
		return rawsync.Status{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelRepeatableRead,
	})
	if err != nil {
		return rawsync.Status{}, fmt.Errorf("beginning raw sync status read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	status := rawsync.Status{
		SourceHeads: make([]rawsync.SourceHeadStatus, 0),
		Devices:     make([]rawsync.DeviceStatus, 0),
	}
	if err := readRawSyncStatusHeads(ctx, tx, identity.TenantID, &status); err != nil {
		return rawsync.Status{}, err
	}
	if err := readRawSyncStatusJobs(ctx, tx, identity.TenantID, &status); err != nil {
		return rawsync.Status{}, err
	}
	if err := readRawSyncStatusDevices(ctx, tx, identity.TenantID, &status); err != nil {
		return rawsync.Status{}, err
	}
	if err := readRawSyncStatusUploads(ctx, tx, identity.TenantID, &status); err != nil {
		return rawsync.Status{}, err
	}
	if err := tx.Commit(); err != nil {
		return rawsync.Status{}, fmt.Errorf("committing raw sync status read: %w", err)
	}
	committed = true
	return status, nil
}

func readRawSyncStatusHeads(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	status *rawsync.Status,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT head.device_id, head.configured_root_id, head.provider,
			head.source_key, head.generation, manifest.accepted_at,
			EXISTS (
				SELECT 1
				FROM raw_ingest_jobs AS job
				WHERE job.tenant_id = head.tenant_id
					AND job.manifest_id = head.manifest_id
					AND job.stage = 'parse'
					AND job.state IN ('ready', 'retrying')
			) AS parse_pending,
			EXISTS (
				SELECT 1
				FROM raw_ingest_jobs AS job
				WHERE job.tenant_id = head.tenant_id
					AND job.manifest_id = head.manifest_id
					AND job.stage = 'parse'
					AND job.state = 'leased'
			) AS parse_leased,
			EXISTS (
				SELECT 1
				FROM raw_ingest_jobs AS job
				WHERE job.tenant_id = head.tenant_id
					AND job.manifest_id = head.manifest_id
					AND job.stage = 'parse'
					AND job.state = 'failed'
			) AS parse_failed
		FROM raw_source_heads AS head
		LEFT JOIN raw_manifests AS manifest
			ON manifest.tenant_id = head.tenant_id
			AND manifest.manifest_id = head.manifest_id
		WHERE head.tenant_id = $1
		ORDER BY head.device_id, head.provider,
			head.configured_root_id, head.source_key`, tenantID)
	if err != nil {
		return fmt.Errorf("querying raw source heads: %w", err)
	}
	for rows.Next() {
		var (
			head           rawsync.SourceHeadStatus
			provider       string
			lastAcceptedAt *time.Time
		)
		if err := rows.Scan(
			&head.DeviceID, &head.ConfiguredRootID, &provider,
			&head.SourceKey, &head.Generation, &lastAcceptedAt,
			&head.ParsePending, &head.ParseLeased, &head.ParseFailed,
		); err != nil {
			return fmt.Errorf(
				"scanning raw source head: %w",
				finishRawSyncStatusRows(rows, err),
			)
		}
		head.Provider = parser.AgentType(provider)
		if lastAcceptedAt != nil {
			acceptedAt := lastAcceptedAt.UTC()
			head.LastAcceptedAt = &acceptedAt
		}
		status.SourceHeads = append(status.SourceHeads, head)
	}
	if err := finishRawSyncStatusRows(rows, nil); err != nil {
		return fmt.Errorf("finishing raw source head query: %w", err)
	}
	return nil
}

func readRawSyncStatusJobs(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	status *rawsync.Status,
) error {
	err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE state = 'ready'),
			COUNT(*) FILTER (WHERE state = 'leased'),
			COUNT(*) FILTER (WHERE state = 'retrying'),
			COUNT(*) FILTER (WHERE state = 'complete'),
			COUNT(*) FILTER (WHERE state = 'failed'),
			COUNT(*) FILTER (WHERE state = 'superseded')
		FROM raw_ingest_jobs
		WHERE tenant_id = $1 AND stage = 'parse'`, tenantID).Scan(
		&status.ParseJobs.Ready,
		&status.ParseJobs.Leased,
		&status.ParseJobs.Retrying,
		&status.ParseJobs.Complete,
		&status.ParseJobs.Failed,
		&status.ParseJobs.Superseded,
	)
	if err != nil {
		return fmt.Errorf("querying raw parse job counts: %w", err)
	}
	return nil
}

func readRawSyncStatusDevices(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	status *rawsync.Status,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT devices.device_id, MAX(tokens.issued_at), COUNT(*) OVER ()
		FROM raw_devices AS devices
		LEFT JOIN raw_device_tokens AS tokens
			ON tokens.tenant_id = devices.tenant_id
			AND tokens.device_id = devices.device_id
		WHERE devices.tenant_id = $1 AND devices.revoked_at IS NULL
		GROUP BY devices.device_id
		ORDER BY devices.device_id`, tenantID)
	if err != nil {
		return fmt.Errorf("querying raw device activity: %w", err)
	}
	for rows.Next() {
		var (
			device            rawsync.DeviceStatus
			lastSeenAt        *time.Time
			activeDeviceCount int64
		)
		if err := rows.Scan(&device.DeviceID, &lastSeenAt, &activeDeviceCount); err != nil {
			return fmt.Errorf(
				"scanning raw device activity: %w",
				finishRawSyncStatusRows(rows, err),
			)
		}
		if lastSeenAt != nil {
			seenAt := lastSeenAt.UTC()
			device.LastSeenAt = &seenAt
		}
		status.ActiveDeviceCount = activeDeviceCount
		status.Devices = append(status.Devices, device)
	}
	if err := finishRawSyncStatusRows(rows, nil); err != nil {
		return fmt.Errorf("finishing raw device activity query: %w", err)
	}
	return nil
}

func readRawSyncStatusUploads(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	status *rawsync.Status,
) error {
	var (
		oldestID      sql.NullString
		oldestCreated sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(size_bytes - offset_bytes), 0),
			(
				SELECT upload_id
				FROM raw_upload_sessions
				WHERE tenant_id = $1 AND state = 'open'
				ORDER BY created_at, upload_id
				LIMIT 1
			),
			(
				SELECT created_at
				FROM raw_upload_sessions
				WHERE tenant_id = $1 AND state = 'open'
				ORDER BY created_at, upload_id
				LIMIT 1
			)
		FROM raw_upload_sessions
		WHERE tenant_id = $1 AND state = 'open'`, tenantID).Scan(
		&status.Uploads.OpenCount,
		&status.Uploads.PendingBytes,
		&oldestID,
		&oldestCreated,
	)
	if err != nil {
		return fmt.Errorf("querying raw upload backlog: %w", err)
	}
	if oldestID.Valid {
		if !oldestCreated.Valid {
			return errors.New("querying raw upload backlog: oldest upload has no creation time")
		}
		status.Uploads.OldestOpenSession = &rawsync.OpenUploadStatus{
			UploadID:  oldestID.String,
			CreatedAt: oldestCreated.Time.UTC(),
		}
	}
	return nil
}

func finishRawSyncStatusRows(rows *sql.Rows, err error) error {
	return errors.Join(err, rows.Err(), rows.Close())
}

var _ rawsync.StatusStore = (*RawIngestStore)(nil)
