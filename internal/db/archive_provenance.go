package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Check before any session update; a row without provenance belongs to another
// ingestion route even if its namespaced ID happens to match.
func (d *DB) checkArchiveSessionProvenanceTx(ctx context.Context, tx *sql.Tx, write SessionBatchWrite) error {
	p := write.ArchiveProvenance
	if owners := d.archiveRebuildProvenance.Load(); owners != nil {
		if stored, ok := (*owners)[write.Session.ID]; ok {
			if p == nil {
				return fmt.Errorf("%w: session belongs to an attached archive", ErrArchiveSourceConflict)
			}
			if stored.Source != p.Source || stored.ManifestID != p.ManifestID || stored.NativeSessionID != p.NativeSessionID {
				return fmt.Errorf("%w: session already belongs to another source", ErrArchiveSourceConflict)
			}
		}
	}
	if p == nil {
		var owned bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_session_provenance WHERE session_id=?)`, write.Session.ID).Scan(&owned)
		if err != nil {
			return err
		}
		if owned {
			return fmt.Errorf("%w: session belongs to an attached archive", ErrArchiveSourceConflict)
		}
		return nil
	}
	if err := archiveReceiverTx(ctx, tx); err != nil {
		return err
	}
	if err := validateArchiveDeviceTx(ctx, tx, p.Source.DeviceID); err != nil {
		return err
	}
	if p.NativeSessionID == "" || strings.Contains(p.NativeSessionID, "~") || write.Session.ID != p.Source.DeviceID+"~"+p.NativeSessionID || write.Session.Machine != p.Source.DeviceID {
		return fmt.Errorf("%w: session does not match its source identity", ErrArchiveSourceConflict)
	}
	if err := checkArchiveSourceBindingTx(ctx, tx, p.ArchiveSourceProvenance); err != nil {
		return err
	}
	var member bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_attachment_sources WHERE `+archiveMembershipWhere+`)`, archiveMembershipArgs(p.ArchiveSourceProvenance)...).Scan(&member); err != nil {
		return err
	}
	if !member {
		return fmt.Errorf("%w: source is not registered in this attachment", ErrArchiveSourceConflict)
	}
	return checkExistingArchiveProvenanceTx(ctx, tx, write.Session.ID, *p)
}

func checkExistingArchiveProvenanceTx(ctx context.Context, tx *sql.Tx, sessionID string, provenance ArchiveSessionProvenance) error {
	var stored ArchiveSessionProvenance
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, sessionID).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	s := &stored.Source
	err := tx.QueryRowContext(ctx, `SELECT archive_id,device_id,root_id,source_key,manifest_id,native_session_id FROM archive_session_provenance WHERE session_id=?`, sessionID).
		Scan(&s.ArchiveID, &s.DeviceID, &s.RootID, &s.SourceKey, &stored.ManifestID, &stored.NativeSessionID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: session already belongs to another route", ErrArchiveSourceConflict)
	}
	if err != nil {
		return err
	}
	if stored.Source != provenance.Source || stored.ManifestID != provenance.ManifestID || stored.NativeSessionID != provenance.NativeSessionID {
		return fmt.Errorf("%w: session already belongs to another source", ErrArchiveSourceConflict)
	}
	return nil
}

func recordArchiveSessionProvenanceTx(ctx context.Context, tx *sql.Tx, write SessionBatchWrite) error {
	p := write.ArchiveProvenance
	if p == nil {
		return nil
	}
	s := p.Source
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_session_provenance(session_id,archive_id,device_id,root_id,source_key,manifest_id,native_session_id)
 VALUES(?,?,?,?,?,?,?) ON CONFLICT(session_id) DO NOTHING`, write.Session.ID, s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey, p.ManifestID, p.NativeSessionID)
	return err
}

// CheckArchiveSessionProvenance rejects collisions before parser side effects.
// Publication repeats this check inside its write transaction.
func (d *DB) CheckArchiveSessionProvenance(ctx context.Context, session Session, provenance ArchiveSessionProvenance) error {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return d.checkArchiveSessionProvenanceTx(ctx, tx, SessionBatchWrite{Session: session, ArchiveProvenance: &provenance})
}

func (d *DB) ArchiveSourceSessionIDs(ctx context.Context, source ArchiveSourceIdentity) ([]string, error) {
	rows, err := d.getReader().Query(ctx, `SELECT p.session_id FROM archive_session_provenance p
 JOIN sessions s ON s.id=p.session_id WHERE p.archive_id=? AND p.device_id=? AND p.root_id=? AND p.source_key=? AND s.deleted_at IS NULL`,
		source.ArchiveID, source.DeviceID, source.RootID, source.SourceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CheckExistingArchiveProvenance compares the original database during a rebuild;
// the replacement database validates the selected snapshot membership separately.
func (d *DB) CheckExistingArchiveProvenance(ctx context.Context, sessionID string, provenance ArchiveSessionProvenance) error {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return checkExistingArchiveProvenanceTx(ctx, tx, sessionID, provenance)
}
