package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const bunCurationBatchSize = 500

// PinRowIDPolicy declares whether replicated pin identities belong to the
// target curation store or to a source-assigned read mirror.
type PinRowIDPolicy uint8

const (
	GeneratePinRowIDs PinRowIDPolicy = iota
	PreservePinRowIDs
)

type bunPinnedMessageReadRow struct {
	ID                  int64              `bun:"id"`
	SessionID           string             `bun:"session_id"`
	MessageID           int64              `bun:"message_id"`
	Ordinal             int                `bun:"ordinal"`
	Note                *string            `bun:"note"`
	CreatedAt           bunmodel.Timestamp `bun:"created_at"`
	Content             *string            `bun:"content"`
	Role                *string            `bun:"role"`
	SessionProject      *string            `bun:"session_project"`
	SessionAgent        *string            `bun:"session_agent"`
	SessionDisplayName  *string            `bun:"session_display_name"`
	SessionFirstMessage *string            `bun:"session_first_message"`
}

// StarSession marks an existing session as starred through the common
// operation-scoped curation writer. Repeating the operation is idempotent.
func (s *BunStore) StarSession(sessionID string) (bool, error) {
	ctx := context.Background()
	createdAt := bunmodel.NewTimestamp(time.Now())
	starred := false
	err := s.update(ctx, WriteCuration, func(store bun.IDB) error {
		return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			result, err := tx.NewRaw(`
				INSERT INTO starred_sessions (session_id, created_at)
				SELECT ?, ? WHERE EXISTS (
					SELECT 1 FROM sessions WHERE id = ?
				)
				ON CONFLICT (session_id) DO NOTHING`,
				sessionID, createdAt, sessionID,
			).Exec(ctx)
			if err != nil {
				return fmt.Errorf("starring session %s: %w", sessionID, err)
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting starred session %s: %w", sessionID, err)
			}
			if inserted > 0 {
				starred = true
				return nil
			}
			exists, err := tx.NewSelect().Table("sessions").
				Where("id = ?", sessionID).Exists(ctx)
			if err != nil {
				return fmt.Errorf("checking session %s: %w", sessionID, err)
			}
			starred = exists
			return nil
		})
	})
	if err != nil {
		return false, err
	}
	return starred, nil
}

// UnstarSession removes a session star.
func (s *BunStore) UnstarSession(sessionID string) error {
	ctx := context.Background()
	return s.update(ctx, WriteCuration, func(store bun.IDB) error {
		if _, err := store.NewDelete().Model((*bunmodel.StarredSession)(nil)).
			Where("session_id = ?", sessionID).Exec(ctx); err != nil {
			return fmt.Errorf("unstarring session %s: %w", sessionID, err)
		}
		return nil
	})
}

// ListStarredSessionIDs returns starred session IDs newest first.
func (s *BunStore) ListStarredSessionIDs(
	ctx context.Context,
) ([]string, error) {
	var rows []struct {
		SessionID string `bun:"session_id"`
	}
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Table("starred_sessions").Column("session_id").
			OrderExpr("created_at DESC").OrderExpr("session_id DESC").
			Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("listing starred sessions: %w", err)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.SessionID
	}
	return ids, nil
}

// BulkStarSessions stars each existing session atomically. Missing IDs are
// ignored so stale client-side curation cannot abort the batch.
func (s *BunStore) BulkStarSessions(sessionIDs []string) error {
	ctx := context.Background()
	createdAt := bunmodel.NewTimestamp(time.Now())
	return s.update(ctx, WriteCuration, func(store bun.IDB) error {
		if len(sessionIDs) == 0 {
			return nil
		}
		return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			for _, sessionID := range sessionIDs {
				if _, err := tx.NewRaw(`
					INSERT INTO starred_sessions (session_id, created_at)
					SELECT ?, ? WHERE EXISTS (
						SELECT 1 FROM sessions WHERE id = ?
					)
					ON CONFLICT (session_id) DO NOTHING`,
					sessionID, createdAt, sessionID,
				).Exec(ctx); err != nil {
					return fmt.Errorf("starring session %s: %w", sessionID, err)
				}
			}
			return nil
		})
	})
}

// PinMessage creates or updates a pin after resolving the public message ID to
// the canonical (session_id, ordinal) key. Generated IDs come from RETURNING on
// every writable engine.
func (s *BunStore) PinMessage(
	sessionID string, messageID int64, note *string,
) (int64, error) {
	ctx := context.Background()
	createdAt := bunmodel.NewTimestamp(time.Now())
	var pinID int64
	err := s.update(ctx, WriteCuration, func(store bun.IDB) error {
		return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			err := tx.NewRaw(`
				INSERT INTO pinned_messages (
					session_id, message_id, ordinal, source_uuid, note, created_at
				)
				SELECT ?, COALESCE(m.id, CAST(m.ordinal AS BIGINT)),
					m.ordinal, m.source_uuid, ?, ?
				FROM messages AS m
				WHERE m.session_id = ?
				  AND (m.id = ? OR (m.id IS NULL AND m.ordinal = ?))
				ON CONFLICT (session_id, ordinal) DO UPDATE SET
					message_id = excluded.message_id,
					source_uuid = excluded.source_uuid,
					note = excluded.note
				RETURNING id`,
				sessionID, note, createdAt, sessionID, messageID, messageID,
			).Scan(ctx, &pinID)
			if err == sql.ErrNoRows {
				pinID = 0
				return nil
			}
			if err != nil {
				return fmt.Errorf("pinning message: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return pinID, nil
}

// UnpinMessage removes the canonical pin addressed by its public message ID.
func (s *BunStore) UnpinMessage(sessionID string, messageID int64) error {
	ctx := context.Background()
	return s.update(ctx, WriteCuration, func(store bun.IDB) error {
		if _, err := store.NewRaw(`
			DELETE FROM pinned_messages
			WHERE session_id = ?
			  AND ordinal IN (
				SELECT ordinal FROM messages
				WHERE session_id = ?
				  AND COALESCE(id, CAST(ordinal AS BIGINT)) = ?
			  )`, sessionID, sessionID, messageID).Exec(ctx); err != nil {
			return fmt.Errorf("unpinning message: %w", err)
		}
		return nil
	})
}

// ListPinnedMessages returns session pins or the visible archive-wide pin page.
func (s *BunStore) ListPinnedMessages(
	ctx context.Context, sessionID string, project string,
) ([]PinnedMessage, error) {
	var rows []bunPinnedMessageReadRow
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().TableExpr("pinned_messages AS pin").
			ColumnExpr("pin.id AS id").
			ColumnExpr("pin.session_id AS session_id").
			ColumnExpr("COALESCE(message.id, CAST(pin.ordinal AS BIGINT)) AS message_id").
			ColumnExpr("pin.ordinal AS ordinal").
			ColumnExpr("pin.note AS note").
			ColumnExpr("pin.created_at AS created_at").
			Join("LEFT JOIN messages AS message").
			JoinOn("message.session_id = pin.session_id").
			JoinOn("message.ordinal = pin.ordinal")
		if sessionID != "" {
			return query.Where("pin.session_id = ?", sessionID).
				OrderExpr("pin.created_at DESC").OrderExpr("pin.id DESC").
				Scan(ctx, &rows)
		}
		query = query.
			ColumnExpr("message.content AS content").
			ColumnExpr("message.role AS role").
			ColumnExpr("session.project AS session_project").
			ColumnExpr("session.agent AS session_agent").
			ColumnExpr("COALESCE(session.display_name, session.session_name) AS session_display_name").
			ColumnExpr("session.first_message AS session_first_message").
			Join("JOIN sessions AS session").
			JoinOn("session.id = pin.session_id").
			JoinOn("session.deleted_at IS NULL")
		if project != "" {
			query = query.Where("session.project = ?", project)
		}
		return query.OrderExpr("pin.created_at DESC").OrderExpr("pin.id DESC").
			Limit(500).Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("listing pinned messages: %w", err)
	}
	var pins []PinnedMessage
	if len(rows) > 0 {
		pins = make([]PinnedMessage, len(rows))
	}
	for i, row := range rows {
		pins[i] = PinnedMessage{
			ID: row.ID, SessionID: row.SessionID, MessageID: row.MessageID,
			Ordinal: row.Ordinal, Note: row.Note,
			CreatedAt:           formatBunCurationTime(row.CreatedAt.Time),
			Content:             row.Content,
			Role:                row.Role,
			SessionProject:      row.SessionProject,
			SessionAgent:        row.SessionAgent,
			SessionDisplayName:  row.SessionDisplayName,
			SessionFirstMessage: row.SessionFirstMessage,
		}
	}
	return pins, nil
}

func formatBunCurationTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// UpsertStarredSessionRows writes canonical replicated star rows atomically.
func UpsertStarredSessionRows(
	ctx context.Context, store bun.IDB, rows []bunmodel.StarredSession,
) error {
	if len(rows) == 0 {
		return nil
	}
	return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for start := 0; start < len(rows); start += bunCurationBatchSize {
			end := min(start+bunCurationBatchSize, len(rows))
			batch := rows[start:end]
			if _, err := tx.NewInsert().Model(&batch).
				On("CONFLICT (session_id) DO UPDATE").
				Set("created_at = EXCLUDED.created_at").Exec(ctx); err != nil {
				return fmt.Errorf("upserting starred session rows: %w", err)
			}
		}
		return nil
	})
}

// UpsertPinnedMessageRows writes canonical replicated pin rows atomically. A
// pre-existing target row keeps its generated ID while curation fields refresh.
func UpsertPinnedMessageRows(
	ctx context.Context, store bun.IDB, rows []bunmodel.PinnedMessage,
	idPolicy PinRowIDPolicy,
) error {
	if len(rows) == 0 {
		return nil
	}
	return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, row := range rows {
			query := tx.NewInsert().Model(&row)
			switch idPolicy {
			case GeneratePinRowIDs:
				query = query.ExcludeColumn("id")
			case PreservePinRowIDs:
				if row.ID == 0 {
					return fmt.Errorf("preserving replicated pin id: source id is zero")
				}
			default:
				return fmt.Errorf("upserting pinned message rows: unknown id policy %d", idPolicy)
			}
			if _, err := query.
				On("CONFLICT (session_id, ordinal) DO UPDATE").
				Set("message_id = EXCLUDED.message_id").
				Set("source_uuid = EXCLUDED.source_uuid").
				Set("note = EXCLUDED.note").
				Set("created_at = EXCLUDED.created_at").Exec(ctx); err != nil {
				return fmt.Errorf("upserting pinned message rows: %w", err)
			}
		}
		return nil
	})
}
