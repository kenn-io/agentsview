package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/notify"
)

// The notification subsystem stores no new tables: dedup cursors
// live in the existing archive_metadata KV (one row per session)
// and the diagnostics event log is a bounded JSON ring under a
// single key. The authoritative per-session facts it reads —
// termination_status, next_ordinal, local_modified_at — are
// maintained by the normal sync pipeline, so resyncs and backend
// swaps carry notification state with the archive.

const (
	notificationStateKeyPrefix = "notification_state:"
	// notificationEventsKey holds a bounded, newest-first JSON
	// ring of decided notifications. Diagnostics only: SSE is not
	// treated as reliable, but neither is this log — dedup
	// correctness comes from the per-session state keys.
	notificationEventsKey    = "notification_events"
	notificationEventsCap    = 100
	notificationCandidateCap = 64
)

// NotificationCandidates returns sessions whose transcript changed
// after since AND after the daemon's readyAt snapshot, bounded to
// the most recent notificationCandidateCap rows. Metadata-only
// mutations never touch local_modified_at, and initial sync /
// history rebuilds operate on pre-readyAt content, so both stay
// silent by construction.
func (d *DB) NotificationCandidates(
	ctx context.Context, since time.Time, readyAt time.Time, limit int,
) ([]notify.Snapshot, error) {
	if limit <= 0 || limit > notificationCandidateCap {
		limit = notificationCandidateCap
	}
	rows, err := d.getReader().QueryContext(ctx, `
		SELECT s.id, s.project, s.agent,
		       COALESCE(s.display_name, ''),
		       COALESCE(s.termination_status, ''),
		       s.next_ordinal,
		       COALESCE(s.ended_with_role, ''),
		       COALESCE(s.local_modified_at, ''),
		       COALESCE(s.relationship_type, ''),
		       s.is_automated,
		       (SELECT m.content FROM messages m
		        WHERE m.session_id = s.id
		        ORDER BY m.ordinal DESC LIMIT 1) AS last_content
		FROM sessions s
		WHERE s.termination_status IS NOT NULL
		  AND s.termination_status != ''
		  AND s.next_ordinal > 0
		  AND COALESCE(s.local_modified_at, '') > ?
		  AND COALESCE(s.local_modified_at, '') > ?
		ORDER BY s.local_modified_at DESC
		LIMIT ?`,
		since.UTC().Format(time.RFC3339Nano),
		readyAt.UTC().Format(time.RFC3339Nano),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("notification candidates: %w", err)
	}
	defer rows.Close()

	var out []notify.Snapshot
	for rows.Next() {
		var (
			s          notify.Snapshot
			terminated string
			modified   string
			automated  int
			content    sql.NullString
		)
		if err := rows.Scan(
			&s.SessionID, &s.Project, &s.Agent,
			&s.DisplayName,
			&terminated,
			&s.NextOrdinal,
			&s.LastRole,
			&modified,
			&s.RelationshipType,
			&automated,
			&content,
		); err != nil {
			return nil, fmt.Errorf("notification candidates scan: %w", err)
		}
		s.TerminationStatus = terminated
		s.IsAutomated = automated != 0
		if modified != "" {
			if t, err := time.Parse(time.RFC3339Nano, modified); err == nil {
				s.LocalModifiedAt = t
			}
		}
		if content.Valid {
			s.LastMessage = tailExcerpt(content.String)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// tailExcerpt keeps the last bodyMaxRunes characters: the end of a
// transcript line is what matters for a notification body.
func tailExcerpt(s string) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) > 160 {
		return "…" + string(runes[len(runes)-160:])
	}
	return string(runes)
}

// NotificationState loads the persisted dedup cursor for a
// session. A missing key yields the zero State.
func (d *DB) NotificationState(
	ctx context.Context, sessionID string,
) (notify.State, error) {
	var raw string
	err := d.getReader().QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?`,
		notificationStateKeyPrefix+sessionID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return notify.State{}, nil
	}
	if err != nil {
		return notify.State{}, fmt.Errorf("notification state: %w", err)
	}
	var st notify.State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return notify.State{}, nil
	}
	return st, nil
}

// SaveNotificationState upserts the dedup cursor for a session.
func (d *DB) SaveNotificationState(
	ctx context.Context, sessionID string, st notify.State,
) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("notification state marshal: %w", err)
	}
	_, err = d.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		notificationStateKeyPrefix+sessionID, string(raw),
	)
	if err != nil {
		return fmt.Errorf("notification state save: %w", err)
	}
	return nil
}

// RecordNotificationEvent appends a decided notification to the
// bounded diagnostics ring under one archive_metadata key.
func (d *DB) RecordNotificationEvent(
	ctx context.Context, n notify.Notification,
) error {
	tx, err := d.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notification event tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw string
	err = tx.QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?
	`, notificationEventsKey).Scan(&raw)
	var events []notify.Notification
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &events)
	}
	events = append([]notify.Notification{n}, events...)
	if len(events) > notificationEventsCap {
		events = events[:notificationEventsCap]
	}
	buf, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("notification event marshal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		notificationEventsKey, string(buf),
	); err != nil {
		return fmt.Errorf("notification event save: %w", err)
	}
	return tx.Commit()
}

// NotificationEvents returns the recent decided notifications,
// newest first. Diagnostics surface for the settings panel.
func (d *DB) NotificationEvents(
	ctx context.Context, limit int,
) ([]notify.Notification, error) {
	var raw string
	err := d.getReader().QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?`,
		notificationEventsKey,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notification events: %w", err)
	}
	var events []notify.Notification
	if err := json.Unmarshal([]byte(raw), &events); err != nil {
		return nil, nil
	}
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}
