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
	notificationEventsKey = "notification_events"
	notificationEventsCap = 100
	// notificationCandidateCap is the default batch size when the
	// caller requests none (limit <= 0). It is deliberately not a
	// ceiling: the Hub detects "batch full" via len == limit, so
	// clamping a larger request to a smaller cap would make a
	// still-full store look exhausted and strand the rest of a
	// burst.
	notificationCandidateCap = 64
)

// notificationTimestampLayout must stay byte-identical to the
// strftime('%Y-%m-%dT%H:%M:%fZ') that every writer of
// sessions.local_modified_at uses: fixed width, always three
// fractional digits. time.RFC3339Nano must not be used here — it
// strips trailing zeros (".060" becomes ".06"), which sorts *above*
// ".061"..".069" under the text comparison this query relies on,
// silently dropping those rows.
const notificationTimestampLayout = "2006-01-02T15:04:05.000Z"

// NotificationCandidates returns sessions whose transcript changed
// after cursor AND after the daemon's readyAt snapshot, ordered by
// (local_modified_at, id) ascending and bounded to limit rows (or
// notificationCandidateCap when limit <= 0).
//
// The cursor is a total lower bound in that same ordering: a row
// qualifies when its timestamp is strictly greater than the
// cursor's, or — when the cursor carries a session id — equal to it
// with a greater id. That tiebreak is load-bearing: the Hub
// processes the batch and resumes after the last row it saw, and
// rows sharing one local_modified_at are normal (a single sync pass
// stamps many sessions with the same strftime('now') value), so a
// timestamp-only cursor would drop every unprocessed row that ties
// with the batch's newest. An empty cursor id is a plain, strictly
// greater time bound (the look-back overlap). Ordering and
// comparison must stay consistent.
//
// Metadata-only mutations never touch local_modified_at, and
// initial sync / history rebuilds operate on pre-readyAt content,
// so both stay silent by construction.
func (d *DB) NotificationCandidates(
	ctx context.Context, cursor notify.Cursor, readyAt time.Time, limit int,
) ([]notify.Snapshot, error) {
	if limit <= 0 {
		limit = notificationCandidateCap
	}
	since := cursor.Since.UTC().Format(notificationTimestampLayout)
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
		  AND (COALESCE(s.local_modified_at, '') > ?
		       OR (? != ''
		           AND COALESCE(s.local_modified_at, '') = ?
		           AND s.id > ?))
		  AND COALESCE(s.local_modified_at, '') > ?
		ORDER BY s.local_modified_at ASC, s.id ASC
		LIMIT ?`,
		since, cursor.ID, since, cursor.ID,
		readyAt.UTC().Format(notificationTimestampLayout),
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
// session. A missing key yields the zero State and a nil error —
// legitimately never notified. A key that exists but cannot be
// parsed yields an error instead: reporting a zero State there
// would be indistinguishable from "never notified" and would
// re-fire an already-delivered notification.
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
		return notify.State{}, fmt.Errorf(
			"notification state corrupt for %s: %w", sessionID, err)
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

// NotificationEvents returns the recent decided notifications, newest first,
// capped at limit when limit > 0. It reads the bounded ring written by
// RecordNotificationEvent.
//
// Only tests call it: no HTTP route, CLI command, or other production code
// reads it today, so it is not currently a settings-panel diagnostics surface.
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
