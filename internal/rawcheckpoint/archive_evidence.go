package rawcheckpoint

import (
	"context"
	"database/sql"
	"errors"
)

// ArchiveEvidence contains only identity and acknowledged receipts. It omits
// configuration, credentials, pending uploads and source content.
type ArchiveEvidence struct {
	DeviceID string          `json:"device_id"`
	Roots    []ArchiveRoot   `json:"roots"`
	Sources  []ArchiveSource `json:"sources"`
}
type ArchiveRoot struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	LocalPath string `json:"local_path"`
}
type ArchiveSource struct {
	Provider   string `json:"provider"`
	RootID     string `json:"root_id"`
	SourceKey  string `json:"source_key"`
	ManifestID string `json:"manifest_id"`
	Receipt    string `json:"receipt"`
	Generation int64  `json:"generation"`
}

func (s *Store) ArchiveEvidence(ctx context.Context) (ArchiveEvidence, error) {
	var evidence ArchiveEvidence
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return evidence, err
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, "SELECT device_id FROM device_config WHERE id=1").Scan(&evidence.DeviceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return evidence, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, provider, local_root FROM configured_roots ORDER BY provider, local_root")
	if err != nil {
		return evidence, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ArchiveRoot
		if err := rows.Scan(&r.ID, &r.Provider, &r.LocalPath); err != nil {
			return evidence, err
		}
		evidence.Roots = append(evidence.Roots, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return evidence, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT provider, configured_root_id, source_key, head_manifest_id, head_receipt, head_generation FROM raw_sources ORDER BY provider, configured_root_id, source_key`)
	if err != nil {
		return evidence, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ArchiveSource
		if err := rows.Scan(&r.Provider, &r.RootID, &r.SourceKey, &r.ManifestID, &r.Receipt, &r.Generation); err != nil {
			return evidence, err
		}
		evidence.Sources = append(evidence.Sources, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return evidence, err
	}
	return evidence, tx.Commit()
}
