package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

var ErrArchiveSourceConflict = errors.New("archive source identity or target conflicts")

type (
	ArchiveAttachmentKey    struct{ RepositoryID, SnapshotID string }
	ArchiveSourceIdentity   struct{ ArchiveID, DeviceID, RootID, SourceKey string }
	ArchiveSourceProvenance struct {
		Attachment ArchiveAttachmentKey
		Source     ArchiveSourceIdentity
		ManifestID string
	}
)

type ArchiveSessionProvenance struct {
	ArchiveSourceProvenance
	NativeSessionID string
}
type ArchiveSourceTarget struct {
	Provenance   ArchiveSourceProvenance
	Target       string
	LastError    string
	MachineLabel string
}
type ArchiveAttachmentStatus struct {
	ArchiveID string
	Target    string
	Pending   int64
}

func (d *DB) RegisterArchiveAttachment(ctx context.Context, key ArchiveAttachmentKey, archiveID string) error {
	if err := rawArchiveFields(key.RepositoryID, key.SnapshotID, archiveID); err != nil {
		return err
	}
	return d.Update(ctx, func(tx *sql.Tx) error {
		if err := archiveReceiverTx(ctx, tx); err != nil {
			return err
		}
		var replaced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_attachments WHERE snapshot_id=? AND repository_id!=?)`, key.SnapshotID, key.RepositoryID).Scan(&replaced); err != nil {
			return err
		}
		if replaced {
			return fmt.Errorf("%w: snapshot repository identity changed", ErrArchiveSourceConflict)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO archive_attachments(repository_id,snapshot_id,archive_id)
			VALUES(?,?,?) ON CONFLICT DO NOTHING`, key.RepositoryID, key.SnapshotID, archiveID)
		if err != nil {
			return err
		}
		var saved string
		if err := tx.QueryRowContext(ctx, `SELECT archive_id FROM archive_attachments WHERE repository_id=? AND snapshot_id=?`,
			key.RepositoryID, key.SnapshotID).Scan(&saved); err != nil {
			return err
		}
		if saved != archiveID {
			return ErrArchiveSourceConflict
		}
		return nil
	})
}

func archiveReceiverTx(ctx context.Context, tx *sql.Tx) error {
	var archiveOnly bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='archive_only' AND value='1')`).Scan(&archiveOnly)
	if err != nil {
		return err
	}
	if archiveOnly {
		return ErrArchiveOnly
	}
	return nil
}

// validateArchiveDeviceTx runs again inside each content publication, so later
// identity adoption cannot silently turn attached history into local exports.
func validateArchiveDeviceTx(ctx context.Context, tx *sql.Tx, device string) error {
	if err := config.ValidateInstallationID(device); err != nil {
		return err
	}
	var local bool
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE related(machine) AS (
		SELECT ? UNION SELECT p.value FROM pg_sync_state p JOIN related r ON p.key=?||r.machine
	) SELECT EXISTS(SELECT 1 FROM related WHERE machine='local' OR machine IN (
		SELECT value FROM pg_sync_state WHERE key IN ('artifact_local_installation_id','artifact_local_machine_name')))`,
		device, MachineAliasKeyPrefix).Scan(&local)
	if err != nil {
		return err
	}
	if local {
		return fmt.Errorf("%w: source belongs to the receiving installation", ErrArchiveSourceConflict)
	}
	return nil
}

// SetArchiveAttachmentTargets registers the complete selected source set and
// updates completion requirements after a parser or content-policy change.
func (d *DB) SetArchiveAttachmentTargets(ctx context.Context, key ArchiveAttachmentKey, target string, sources []ArchiveSourceTarget) error {
	if err := rawArchiveFields(key.RepositoryID, key.SnapshotID, target); err != nil {
		return err
	}
	return d.Update(ctx, func(tx *sql.Tx) error {
		if err := archiveReceiverTx(ctx, tx); err != nil {
			return err
		}
		var archiveID string
		if err := tx.QueryRowContext(ctx, `SELECT archive_id FROM archive_attachments WHERE repository_id=? AND snapshot_id=?`,
			key.RepositoryID, key.SnapshotID).Scan(&archiveID); err != nil {
			return err
		}
		seen := make(map[ArchiveSourceIdentity]bool, len(sources))
		for _, source := range sources {
			p := source.Provenance
			if p.Attachment != key || p.Source.ArchiveID != archiveID || seen[p.Source] {
				return ErrArchiveSourceConflict
			}
			seen[p.Source] = true
			if err := setArchiveSourceTargetTx(ctx, tx, source); err != nil {
				return err
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM archive_attachment_sources WHERE repository_id=? AND snapshot_id=?`,
			key.RepositoryID, key.SnapshotID).Scan(&count); err != nil {
			return err
		}
		if count != len(sources) {
			return fmt.Errorf("%w: registered source set differs", ErrArchiveSourceConflict)
		}
		_, err := tx.ExecContext(ctx, `UPDATE archive_attachments SET target=?,pending_count=(
			SELECT count(*) FROM archive_attachment_sources WHERE repository_id=? AND snapshot_id=? AND completed_target!=target)
			WHERE repository_id=? AND snapshot_id=?`, target, key.RepositoryID, key.SnapshotID, key.RepositoryID, key.SnapshotID)
		return err
	})
}

func setArchiveSourceTargetTx(ctx context.Context, tx *sql.Tx, source ArchiveSourceTarget) error {
	p, s := source.Provenance, source.Provenance.Source
	if err := rawArchiveFields(s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey, p.ManifestID, source.Target); err != nil {
		return err
	}
	if err := validateArchiveDeviceTx(ctx, tx, s.DeviceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_source_bindings(archive_id,device_id,root_id,source_key,manifest_id)
		VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey, p.ManifestID)
	if err != nil {
		return err
	}
	if err := checkArchiveSourceBindingTx(ctx, tx, p); err != nil {
		return err
	}
	if source.MachineLabel != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pg_sync_state(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, MachineLabelKeyPrefix+s.DeviceID, source.MachineLabel); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_attachment_sources(repository_id,snapshot_id,archive_id,device_id,root_id,source_key,target)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(repository_id,snapshot_id,archive_id,device_id,root_id,source_key)
		DO UPDATE SET target=excluded.target`, p.Attachment.RepositoryID, p.Attachment.SnapshotID,
		s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey, source.Target)
	return err
}

func checkArchiveSourceBindingTx(ctx context.Context, tx *sql.Tx, p ArchiveSourceProvenance) error {
	s := p.Source
	var manifestID string
	if err := tx.QueryRowContext(ctx, `SELECT manifest_id FROM archive_source_bindings
		WHERE archive_id=? AND device_id=? AND root_id=? AND source_key=?`,
		s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey).Scan(&manifestID); err != nil {
		return err
	}
	if manifestID != p.ManifestID {
		return ErrArchiveSourceConflict
	}
	return nil
}

func (d *DB) ArchiveAttachmentStatus(ctx context.Context, key ArchiveAttachmentKey) (ArchiveAttachmentStatus, error) {
	var result ArchiveAttachmentStatus
	err := d.getReader().QueryRowContext(ctx, `SELECT archive_id,target,pending_count FROM archive_attachments
		WHERE repository_id=? AND snapshot_id=?`, key.RepositoryID, key.SnapshotID).
		Scan(&result.ArchiveID, &result.Target, &result.Pending)
	return result, err
}

func (d *DB) PendingArchiveSources(ctx context.Context, key ArchiveAttachmentKey, after ArchiveSourceIdentity, limit int) ([]ArchiveSourceTarget, error) {
	if limit <= 0 || limit > 10000 {
		return nil, errors.New("invalid pending archive source page size")
	}
	rows, err := d.getReader().QueryContext(ctx, `SELECT s.archive_id,s.device_id,s.root_id,s.source_key,b.manifest_id,s.target,s.last_error
		FROM archive_attachment_sources s JOIN archive_source_bindings b USING(archive_id,device_id,root_id,source_key)
		WHERE s.repository_id=? AND s.snapshot_id=? AND s.completed_target!=s.target AND (s.root_id,s.source_key)>(?,?) ORDER BY s.root_id,s.source_key LIMIT ?`,
		key.RepositoryID, key.SnapshotID, after.RootID, after.SourceKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ArchiveSourceTarget
	for rows.Next() {
		item := ArchiveSourceTarget{Provenance: ArchiveSourceProvenance{Attachment: key}}
		s := &item.Provenance.Source
		if err := rows.Scan(&s.ArchiveID, &s.DeviceID, &s.RootID, &s.SourceKey, &item.Provenance.ManifestID, &item.Target, &item.LastError); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

const archiveMembershipWhere = `repository_id=? AND snapshot_id=? AND archive_id=? AND device_id=? AND root_id=? AND source_key=?`

func archiveMembershipArgs(p ArchiveSourceProvenance) []any {
	s := p.Source
	return []any{p.Attachment.RepositoryID, p.Attachment.SnapshotID, s.ArchiveID, s.DeviceID, s.RootID, s.SourceKey}
}

// ArchiveSourcePending checks the target again after acquiring the sync lock.
// A rebuild can finish a source after the drainer selected its pending page.
func (d *DB) ArchiveSourcePending(ctx context.Context, p ArchiveSourceProvenance, target string) (bool, error) {
	var wanted, completed string
	err := d.getReader().QueryRowContext(ctx, `SELECT target,completed_target FROM archive_attachment_sources WHERE `+archiveMembershipWhere,
		archiveMembershipArgs(p)...).Scan(&wanted, &completed)
	if err != nil {
		return false, err
	}
	if target == "" || wanted != target {
		return false, ErrArchiveSourceConflict
	}
	return completed != target, nil
}

func (d *DB) CompleteArchiveSource(ctx context.Context, p ArchiveSourceProvenance, target string) error {
	return d.completeArchiveSource(ctx, p, target, nil)
}

// ArchiveSourceCompletion reads the completion marker before a source is
// parsed. Restoring a member replaces the marker during that parse.
func (d *DB) ArchiveSourceCompletion(ctx context.Context, p ArchiveSourceProvenance) (string, error) {
	var completed string
	err := d.getReader().QueryRowContext(ctx, `SELECT completed_target FROM archive_attachment_sources WHERE `+archiveMembershipWhere,
		archiveMembershipArgs(p)...).Scan(&completed)
	return completed, err
}

// CompleteArchiveSourceFrom completes a source only if its marker still equals
// observed. A member restored during the parse keeps the source pending, so
// the next drain applies the upgrade the trashed member skipped.
func (d *DB) CompleteArchiveSourceFrom(ctx context.Context, p ArchiveSourceProvenance, target, observed string) error {
	return d.completeArchiveSource(ctx, p, target, &observed)
}

func (d *DB) completeArchiveSource(ctx context.Context, p ArchiveSourceProvenance, target string, observed *string) error {
	return d.Update(ctx, func(tx *sql.Tx) error {
		if err := checkArchiveSourceBindingTx(ctx, tx, p); err != nil {
			return err
		}
		var wanted, completed string
		if err := tx.QueryRowContext(ctx, `SELECT target,completed_target FROM archive_attachment_sources WHERE `+archiveMembershipWhere,
			archiveMembershipArgs(p)...).Scan(&wanted, &completed); err != nil {
			return err
		}
		if target == "" || target != wanted {
			return ErrArchiveSourceConflict
		}
		if completed == target || (observed != nil && completed != *observed) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE archive_attachment_sources SET completed_target=target,last_error=''
			WHERE `+archiveMembershipWhere, archiveMembershipArgs(p)...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE archive_attachments SET pending_count=pending_count-1
			WHERE repository_id=? AND snapshot_id=?`, p.Attachment.RepositoryID, p.Attachment.SnapshotID)
		return err
	})
}

// Restoring a member requires another parse even if its source finished while
// the member was trashed, or finishes while the restore commits. Several snapshots can share that source. Count only
// newly pending memberships so restoring another member cannot double-count it.
func invalidateRestoredArchiveSourceTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE archive_attachments SET pending_count=pending_count+1
		WHERE (repository_id,snapshot_id) IN (
			SELECT s.repository_id,s.snapshot_id FROM archive_attachment_sources s
			JOIN archive_session_provenance p USING(archive_id,device_id,root_id,source_key)
			WHERE p.session_id=? AND s.completed_target=s.target
		)`, sessionID); err != nil {
		return err
	}
	// A fresh marker also fences completions already in flight.
	_, err := tx.ExecContext(ctx, `UPDATE archive_attachment_sources SET completed_target='restored:'||lower(hex(randomblob(8)))
		WHERE (archive_id,device_id,root_id,source_key) IN (
			SELECT archive_id,device_id,root_id,source_key FROM archive_session_provenance WHERE session_id=?
		)`, sessionID)
	return err
}

func (d *DB) RecordArchiveSourceError(ctx context.Context, p ArchiveSourceProvenance, message string) error {
	if strings.TrimSpace(message) == "" || len(message) > 64<<10 {
		return errors.New("invalid archive source error")
	}
	return d.Update(ctx, func(tx *sql.Tx) error {
		if err := checkArchiveSourceBindingTx(ctx, tx, p); err != nil {
			return err
		}
		args := append([]any{message}, archiveMembershipArgs(p)...)
		result, err := tx.ExecContext(ctx, `UPDATE archive_attachment_sources SET last_error=? WHERE `+archiveMembershipWhere, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n != 1 {
			return sql.ErrNoRows
		}
		return err
	})
}
