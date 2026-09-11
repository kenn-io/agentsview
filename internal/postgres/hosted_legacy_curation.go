package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func legacyPublicVariant(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "legacy~" + hex.EncodeToString(digest[:])
}

// Alias locks fence first publication before a raw group exists. A fixed 256
// schema-scoped buckets bounds shared-lock memory for large legacy batches.
// Collisions only add contention; acquire numeric bucket order before any group
// or session lock, and never call this helper from a raw group guard.
func lockHostedAliases(ctx context.Context, tx *sql.Tx, aliases []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT (hashtextextended(alias,0)&255)::int AS bucket FROM unnest($1::text[]) alias ORDER BY bucket`, aliases)
	if err != nil {
		return err
	}
	var buckets []int
	for rows.Next() {
		var bucket int
		if err = rows.Scan(&bucket); err != nil {
			rows.Close()
			return err
		}
		buckets = append(buckets, bucket)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, bucket := range buckets {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),$1)`, bucket); err != nil {
			return err
		}
	}
	return nil
}

func (h *HostedStore) legacyWrite(ctx context.Context, alias, id string, write func(*sql.Tx, string) (int64, error)) (int64, error) {
	tx, err := h.physical.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	aliases := []string{id, legacyPublicVariant(id), alias}
	if err = lockHostedAliases(ctx, tx, aliases); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.group_id FROM raw_session_groups g WHERE EXISTS(SELECT 1 FROM raw_session_public_aliases a WHERE a.group_id=g.group_id AND a.alias_id=ANY($1)) ORDER BY g.group_id FOR UPDATE OF g`, aliases)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			rows.Close()
			return 0, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE provenance_kind='legacy' AND id=$1 AND (id=$2 OR hosted_legacy_alias(id)=$2) FOR UPDATE`, id, alias).Scan(&current)
	if err == sql.ErrNoRows {
		return 0, &db.SessionIdentityError{State: "gone"}
	}
	if err != nil {
		return 0, err
	}
	raw, err := resolveRawIdentity(ctx, tx, alias)
	if err != nil {
		return 0, err
	}
	if raw.State != RawIdentityGone {
		return 0, &db.SessionIdentityError{State: "ambiguous", Variants: append(raw.Variants, legacyPublicVariant(id))}
	}
	n, err := write(tx, current)
	if err != nil {
		return 0, err
	}
	// Star/pin tables do not fire the sessions revision trigger.
	if _, err = tx.ExecContext(ctx, `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1`); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func legacyCurationTx(ctx context.Context, tx *sql.Tx, id, field string, value any) (int64, error) {
	var result sql.Result
	var err error
	switch field {
	case "display_name":
		result, err = tx.ExecContext(ctx, `UPDATE sessions SET display_name=$2,updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id, value)
	case "starred":
		if value.(bool) {
			result, err = tx.ExecContext(ctx, `INSERT INTO starred_sessions(session_id) VALUES($1) ON CONFLICT(session_id) DO NOTHING`, id)
		} else {
			result, err = tx.ExecContext(ctx, `DELETE FROM starred_sessions WHERE session_id=$1`, id)
		}
	case "trashed":
		if value.(bool) {
			result, err = tx.ExecContext(ctx, `UPDATE sessions SET deleted_at=NOW(),deletion_cause=NULL,updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE sessions SET deleted_at=NULL,deletion_cause=NULL,data_version=$2,updated_at=NOW() WHERE id=$1 AND deleted_at IS NOT NULL`, id, max(db.CurrentDataVersion()-1, 0))
		}
	default:
		return 0, fmt.Errorf("unsupported legacy curation field %q", field)
	}
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func deleteLegacyTrashedTx(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	ids, excluded, err := readPGTrashedSessionExclusions(ctx, tx, `s.id=$1 AND s.provenance_kind='legacy' AND s.deleted_at IS NOT NULL`, id)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if err = insertPGExcludedSessionIDs(ctx, tx, excluded); err != nil {
		return 0, err
	}
	n, err := deletePGTrashedSessionRows(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	if err = deleteLegacyExcludedSessionRows(ctx, tx, excluded); err != nil {
		return 0, err
	}
	return n, nil
}

// Legacy alias cleanup must never reach a raw row, even if an adopted archive
// has an alias spelling that later collides with a physical projection identity.
func deleteLegacyExcludedSessionRows(ctx context.Context, tx *sql.Tx, ids []string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE provenance_kind='legacy' AND id=ANY($1)`, ids)
	return err
}
