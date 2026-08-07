package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const bunMutationBatchSize = 400

// RenameSession sets or clears the user-owned display name of an active
// session.
func (s *BunStore) RenameSession(id string, displayName *string) error {
	ctx := context.Background()
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		query := store.NewUpdate().Model((*bunmodel.Session)(nil)).
			Set("display_name = ?", displayName).
			Where("id = ?", id).
			Where("deleted_at IS NULL")
		s.backend.Capabilities().SessionMutations.ApplyTouch(
			query, bunmodel.NewTimestamp(time.Now().UTC()),
		)
		if _, err := query.Exec(ctx); err != nil {
			return fmt.Errorf("renaming session %s: %w", id, err)
		}
		return nil
	})
	return err
}

// SoftDeleteSession moves an active session to user trash. A recoverable
// source-missing tombstone becomes user trash so a later source return cannot
// revive a session the user explicitly removed.
func (s *BunStore) SoftDeleteSession(id string) error {
	ctx := context.Background()
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		now := bunmodel.NewTimestamp(time.Now().UTC())
		query := store.NewUpdate().Model((*bunmodel.Session)(nil)).
			Set("deletion_cause = NULL").
			Where("id = ?", id).
			Where("(deleted_at IS NULL OR deletion_cause = ?)", deletionCauseSourceMissing)
		s.backend.Capabilities().SessionMutations.ApplySoftDelete(query, now)
		if _, err := query.Exec(ctx); err != nil {
			return fmt.Errorf("soft deleting session %s: %w", id, err)
		}
		return nil
	})
	return err
}

// SoftDeleteSessions moves multiple active or source-missing sessions to user
// trash in one transaction and returns the number changed.
func (s *BunStore) SoftDeleteSessions(ids []string) (int, error) {
	ctx := context.Background()
	total := 0
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		if len(ids) == 0 {
			return nil
		}
		transactionTotal := 0
		err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			now := bunmodel.NewTimestamp(time.Now().UTC())
			for start := 0; start < len(ids); start += bunMutationBatchSize {
				end := min(start+bunMutationBatchSize, len(ids))
				query := tx.NewUpdate().Model((*bunmodel.Session)(nil)).
					Set("deletion_cause = NULL").
					Where("id IN (?)", bun.List(ids[start:end])).
					Where("(deleted_at IS NULL OR deletion_cause = ?)", deletionCauseSourceMissing)
				s.backend.Capabilities().SessionMutations.ApplySoftDelete(query, now)
				result, err := query.Exec(ctx)
				if err != nil {
					return fmt.Errorf("soft deleting sessions: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("counting soft deleted sessions: %w", err)
				}
				transactionTotal += int(count)
			}
			return nil
		})
		if err == nil {
			total = transactionTotal
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// RestoreSession restores one user-trashed session. Source-missing tombstones
// remain protected for watcher reconciliation.
func (s *BunStore) RestoreSession(id string) (int64, error) {
	ctx := context.Background()
	var restored int64
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		var transactionRestored int64
		err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			query := tx.NewUpdate().Model((*bunmodel.Session)(nil)).
				Set("deleted_at = NULL").
				Set("deletion_cause = NULL").
				Where("id = ?", id).
				Where("deleted_at IS NOT NULL").
				Where("deletion_cause IS NULL")
			s.backend.Capabilities().SessionMutations.ApplyTouch(
				query, bunmodel.NewTimestamp(time.Now().UTC()),
			)
			result, err := query.Exec(ctx)
			if err != nil {
				return fmt.Errorf("restoring session %s: %w", id, err)
			}
			transactionRestored, err = result.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting restored session %s: %w", id, err)
			}
			if transactionRestored > 0 {
				return s.backend.Capabilities().SessionMutations.AfterRestore(ctx, tx, id)
			}
			return nil
		})
		if err == nil {
			restored = transactionRestored
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return restored, nil
}

// ListTrashedSessions returns user-trashed sessions newest first. Recoverable
// source-missing tombstones are not user trash.
func (s *BunStore) ListTrashedSessions(ctx context.Context) ([]Session, error) {
	sessions := []Session{}
	err := s.view(ctx, func(store bun.IDB) error {
		var rows []bunmodel.Session
		query := store.NewSelect().Model(&rows).
			Where("deleted_at IS NOT NULL").
			Where("deletion_cause IS NULL")
		orderExpr := s.backend.TimestampOrderExpr("deleted_at")
		if err := query.OrderExpr(orderExpr + " DESC").
			OrderExpr("id ASC").Limit(500).Scan(ctx); err != nil {
			return fmt.Errorf("listing trashed sessions: %w", err)
		}
		sessions = make([]Session, 0, len(rows))
		for _, row := range rows {
			sessions = append(sessions, visibleSessionFromBunRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// DeleteSessionIfTrashed permanently deletes one user-trashed session and
// records its canonical and alias identities as exclusions.
func (s *BunStore) DeleteSessionIfTrashed(id string) (int64, error) {
	ctx := context.Background()
	var deleted int64
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		var transactionDeleted int64
		err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			result, err := tx.NewUpdate().Model((*bunmodel.Session)(nil)).
				Set("deleted_at = deleted_at").
				Where("id = ?", id).
				Where("deleted_at IS NOT NULL").
				Where("deletion_cause IS NULL").Exec(ctx)
			if err != nil {
				return fmt.Errorf("locking trashed session %s: %w", id, err)
			}
			locked, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting locked trashed session %s: %w", id, err)
			}
			if locked == 0 {
				return nil
			}
			rows, err := selectUserTrashRows(ctx, tx, "id = ?", id)
			if err != nil {
				return fmt.Errorf("loading trashed session %s: %w", id, err)
			}
			if err := s.permanentlyDeleteSessionRows(ctx, tx, rows); err != nil {
				return fmt.Errorf("permanently deleting trashed session %s: %w", id, err)
			}
			transactionDeleted = int64(len(rows))
			return nil
		})
		if err == nil {
			deleted = transactionDeleted
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// EmptyTrash permanently deletes every user-trashed session and records its
// canonical and alias identities as exclusions.
func (s *BunStore) EmptyTrash() (int, error) {
	ctx := context.Background()
	deleted := 0
	err := s.update(ctx, WriteSessionManagement, func(store bun.IDB) error {
		transactionDeleted := 0
		err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewUpdate().Model((*bunmodel.Session)(nil)).
				Set("deleted_at = deleted_at").
				Where("deleted_at IS NOT NULL").
				Where("deletion_cause IS NULL").Exec(ctx); err != nil {
				return fmt.Errorf("locking trashed sessions: %w", err)
			}
			rows, err := selectUserTrashRows(ctx, tx, "1 = 1")
			if err != nil {
				return fmt.Errorf("loading trashed sessions: %w", err)
			}
			if err := s.permanentlyDeleteSessionRows(ctx, tx, rows); err != nil {
				return fmt.Errorf("emptying trash: %w", err)
			}
			transactionDeleted = len(rows)
			return nil
		})
		if err == nil {
			deleted = transactionDeleted
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func selectUserTrashRows(
	ctx context.Context, store bun.IDB, where string, args ...any,
) ([]bunmodel.Session, error) {
	var rows []bunmodel.Session
	err := store.NewSelect().Model(&rows).
		Column("id", "agent", "file_path").
		Where(where, args...).
		Where("deleted_at IS NOT NULL").
		Where("deletion_cause IS NULL").
		Scan(ctx)
	return rows, err
}

func (s *BunStore) permanentlyDeleteSessionRows(
	ctx context.Context, store bun.Tx, rows []bunmodel.Session,
) error {
	if len(rows) == 0 {
		return nil
	}
	selectedIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		selectedIDs = append(selectedIDs, row.ID)
	}
	excludedIDs, err := connectedSessionAliasIDs(ctx, store, selectedIDs)
	if err != nil {
		return fmt.Errorf("loading session aliases: %w", err)
	}
	for _, row := range rows {
		filePath := sql.NullString{}
		if row.FilePath != nil {
			filePath = sql.NullString{String: *row.FilePath, Valid: true}
		}
		if aliasID := vibeFallbackAliasID(row.ID, row.Agent, filePath); aliasID != "" {
			excludedIDs = appendUniqueString(excludedIDs, aliasID)
		}
	}

	now := bunmodel.NewTimestamp(time.Now().UTC())
	for start := 0; start < len(excludedIDs); start += bunMutationBatchSize {
		end := min(start+bunMutationBatchSize, len(excludedIDs))
		exclusions := make([]bunmodel.ExcludedSession, 0, end-start)
		for _, id := range excludedIDs[start:end] {
			exclusions = append(exclusions, bunmodel.ExcludedSession{ID: id, CreatedAt: now})
		}
		if _, err := store.NewInsert().Model(&exclusions).
			On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
			return fmt.Errorf("recording excluded session ids: %w", err)
		}
	}
	if err := s.backend.Capabilities().SessionMutations.BeforeDelete(
		ctx, store, excludedIDs,
	); err != nil {
		return err
	}
	for start := 0; start < len(excludedIDs); start += bunMutationBatchSize {
		end := min(start+bunMutationBatchSize, len(excludedIDs))
		if _, err := store.NewDelete().Model((*bunmodel.Session)(nil)).
			Where("id IN (?)", bun.List(excludedIDs[start:end])).Exec(ctx); err != nil {
			return fmt.Errorf("deleting excluded session rows: %w", err)
		}
	}
	return nil
}

func connectedSessionAliasIDs(
	ctx context.Context, store bun.IDB, initial []string,
) ([]string, error) {
	all := make([]string, 0, len(initial))
	seen := make(map[string]struct{}, len(initial))
	frontier := make([]string, 0, len(initial))
	for _, id := range initial {
		if _, ok := seen[id]; ok || id == "" {
			continue
		}
		seen[id] = struct{}{}
		all = append(all, id)
		frontier = append(frontier, id)
	}
	for len(frontier) > 0 {
		next := []string{}
		for start := 0; start < len(frontier); start += bunMutationBatchSize {
			end := min(start+bunMutationBatchSize, len(frontier))
			var aliases []bunmodel.SessionAlias
			if err := store.NewSelect().Model(&aliases).
				Where("session_id IN (?) OR alias_id IN (?)",
					bun.List(frontier[start:end]), bun.List(frontier[start:end])).
				Scan(ctx); err != nil {
				return nil, err
			}
			for _, alias := range aliases {
				for _, id := range []string{alias.SessionID, alias.AliasID} {
					if _, ok := seen[id]; ok || id == "" {
						continue
					}
					seen[id] = struct{}{}
					all = append(all, id)
					next = append(next, id)
				}
			}
		}
		frontier = next
	}
	return all, nil
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
