package rawcheckpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// ClientSourceStatus is one path-free source-chain checkpoint.
type ClientSourceStatus struct {
	Provider         parser.AgentType `json:"provider"`
	ConfiguredRootID string           `json:"configured_root_id"`
	SourceID         string           `json:"source_id"`
	LatestCaptureID  string           `json:"latest_capture_id"`
	Head             SourceHead       `json:"head"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// ClientStatus is the local, content-free operational state of raw sync.
type ClientStatus struct {
	DeviceID           string               `json:"device_id"`
	LastCaptureAt      *time.Time           `json:"last_capture_at,omitempty"`
	PendingGenerations int                  `json:"pending_generations"`
	PendingObjects     int                  `json:"pending_objects"`
	PendingObjectBytes int64                `json:"pending_object_bytes"`
	Outbox             OutboxUsage          `json:"outbox"`
	RetryAt            *time.Time           `json:"retry_at,omitempty"`
	PermanentFailures  int                  `json:"permanent_failures"`
	Sources            []ClientSourceStatus `json:"sources,omitempty"`
	Coverage           []CoverageState      `json:"coverage,omitempty"`
}

// ClientStatus reads a consistent operational snapshot without exposing local
// source paths, credentials, or captured bytes.
func (s *Store) ClientStatus(ctx context.Context) (ClientStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: begin status snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var schema int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema); err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: read status schema: %w", err)
	}
	var status ClientStatus
	err = tx.QueryRowContext(ctx,
		`SELECT device_id FROM device_config WHERE id = 1`,
	).Scan(&status.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		status.DeviceID = ""
	} else if err != nil {
		return ClientStatus{}, err
	}
	if schema == 1 {
		status.Outbox.LimitBytes = s.maxOutboxBytes
		if status.Outbox.LimitBytes == 0 {
			status.Outbox.LimitBytes = defaultMaxOutboxBytes
		}
		status.Sources, err = clientVersionOneSourceStatuses(ctx, tx)
		if err != nil {
			return ClientStatus{}, err
		}
		if err := tx.Commit(); err != nil {
			return ClientStatus{}, fmt.Errorf("rawcheckpoint: commit status snapshot: %w", err)
		}
		return status, nil
	}
	status.Outbox, err = clientStatusOutboxUsage(ctx, tx, s.maxOutboxBytes)
	if err != nil {
		return ClientStatus{}, err
	}
	var retryAt sql.NullString
	if schema == 2 {
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox_generations
			WHERE state IN ('queued', 'finalized')`,
		).Scan(&status.PendingGenerations)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT count(*), min(NULLIF(retry_at, ''))
			FROM outbox_generations
			WHERE state IN ('queued', 'finalized')`,
		).Scan(&status.PendingGenerations, &retryAt)
	}
	if err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: read generation status: %w", err)
	}
	var lastCapture sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT max(updated_at) FROM raw_sources
		WHERE latest_capture_id != ''`).Scan(&lastCapture); err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: read last capture: %w", err)
	}
	status.LastCaptureAt = parseOptionalTime(lastCapture)
	status.RetryAt = parseOptionalTime(retryAt)
	err = tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length), 0)
		FROM outbox_objects WHERE state = 'live' AND ref_count > 0`,
	).Scan(&status.PendingObjects, &status.PendingObjectBytes)
	if err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: read object status: %w", err)
	}
	if schema >= 3 {
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox_generations
			WHERE blocked = 1 AND error_class = ?`, string(GenerationFailurePermanent),
		).Scan(&status.PermanentFailures)
		if err != nil {
			return ClientStatus{}, fmt.Errorf("rawcheckpoint: read permanent failures: %w", err)
		}
	}
	status.Sources, err = clientSourceStatuses(ctx, tx)
	if err != nil {
		return ClientStatus{}, err
	}
	status.Coverage, err = clientCoverageStatuses(ctx, tx)
	if err != nil {
		return ClientStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return ClientStatus{}, fmt.Errorf("rawcheckpoint: commit status snapshot: %w", err)
	}
	return status, nil
}

type statusQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func clientStatusOutboxUsage(
	ctx context.Context,
	query statusQueryer,
	configuredLimit int64,
) (OutboxUsage, error) {
	usage := OutboxUsage{LimitBytes: configuredLimit}
	err := query.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(length) FROM outbox_objects WHERE state != 'remote'), 0) +
		COALESCE((SELECT sum(metadata_bytes) FROM outbox_generations), 0),
		COALESCE((SELECT sum(reserved_bytes) FROM outbox_reservations), 0)`,
	).Scan(&usage.UsedBytes, &usage.ReservedBytes)
	if err != nil {
		return OutboxUsage{}, fmt.Errorf("rawcheckpoint: read outbox usage: %w", err)
	}
	if usage.LimitBytes == 0 {
		var hasLimitColumn int
		if err := query.QueryRowContext(ctx, `SELECT count(*)
			FROM pragma_table_info('outbox_config')
			WHERE name = 'max_outbox_bytes'`).Scan(&hasLimitColumn); err != nil {
			return OutboxUsage{}, fmt.Errorf("rawcheckpoint: inspect outbox limit: %w", err)
		}
		if hasLimitColumn == 0 {
			usage.LimitBytes = defaultMaxOutboxBytes
		} else if err := query.QueryRowContext(ctx, `SELECT max_outbox_bytes
			FROM outbox_config WHERE id = 1`).Scan(&usage.LimitBytes); err != nil {
			return OutboxUsage{}, fmt.Errorf("rawcheckpoint: read outbox limit: %w", err)
		}
	}
	return usage, nil
}

func clientSourceStatuses(
	ctx context.Context,
	query statusQueryer,
) ([]ClientSourceStatus, error) {
	rows, err := query.QueryContext(ctx, `SELECT provider, configured_root_id,
		source_key, latest_capture_id, head_manifest_id, head_receipt,
		head_generation, updated_at FROM raw_sources
		ORDER BY provider, configured_root_id, source_key`)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read source status: %w", err)
	}
	defer rows.Close()
	var statuses []ClientSourceStatus
	for rows.Next() {
		var item ClientSourceStatus
		var sourceKey string
		var updatedAt string
		if err := rows.Scan(
			&item.Provider, &item.ConfiguredRootID,
			&sourceKey, &item.LatestCaptureID,
			&item.Head.ManifestID, &item.Head.Receipt, &item.Head.Generation,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: read source status: %w", err)
		}
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		item.Head.UpdatedAt = item.UpdatedAt
		item.SourceID = opaqueSourceStatusID(
			item.Provider, item.ConfiguredRootID, sourceKey,
		)
		statuses = append(statuses, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read source status: %w", err)
	}
	return statuses, nil
}

func clientVersionOneSourceStatuses(
	ctx context.Context,
	query statusQueryer,
) ([]ClientSourceStatus, error) {
	rows, err := query.QueryContext(ctx, `SELECT provider, configured_root_id,
		source_key, head_manifest_id, head_receipt, head_generation, updated_at
		FROM raw_sources ORDER BY provider, configured_root_id, source_key`)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read version-one source status: %w", err)
	}
	defer rows.Close()
	var statuses []ClientSourceStatus
	for rows.Next() {
		var item ClientSourceStatus
		var sourceKey string
		var updatedAt string
		if err := rows.Scan(
			&item.Provider, &item.ConfiguredRootID, &sourceKey,
			&item.Head.ManifestID, &item.Head.Receipt, &item.Head.Generation,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: read version-one source status: %w", err)
		}
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		item.Head.UpdatedAt = item.UpdatedAt
		item.SourceID = opaqueSourceStatusID(
			item.Provider, item.ConfiguredRootID, sourceKey,
		)
		statuses = append(statuses, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read version-one source status: %w", err)
	}
	return statuses, nil
}

func opaqueSourceStatusID(
	provider parser.AgentType,
	configuredRootID string,
	sourceKey string,
) string {
	digest := sha256.Sum256([]byte(
		string(provider) + "\x00" + configuredRootID + "\x00" + sourceKey,
	))
	return fmt.Sprintf("%x", digest[:16])
}

func clientCoverageStatuses(
	ctx context.Context,
	query statusQueryer,
) ([]CoverageState, error) {
	rows, err := query.QueryContext(ctx, `SELECT provider, configured_root_id,
		state, reason, degraded_at, recovered_at, updated_at FROM raw_coverage
		ORDER BY provider, configured_root_id`)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read coverage status: %w", err)
	}
	defer rows.Close()
	var statuses []CoverageState
	for rows.Next() {
		var item CoverageState
		var provider string
		var degradedAt, recoveredAt, updatedAt sql.NullString
		if err := rows.Scan(
			&provider, &item.ConfiguredRootID, &item.State, &item.Reason,
			&degradedAt, &recoveredAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: read coverage status: %w", err)
		}
		item.Provider = parser.AgentType(provider)
		item.DegradedAt = parseOptionalTime(degradedAt)
		item.RecoveredAt = parseOptionalTime(recoveredAt)
		if parsed := parseOptionalTime(updatedAt); parsed != nil {
			item.UpdatedAt = *parsed
		}
		statuses = append(statuses, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rawcheckpoint: read coverage status: %w", err)
	}
	return statuses, nil
}
