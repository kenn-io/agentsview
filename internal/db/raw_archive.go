package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

type (
	RawArchiveRoot struct{ ID, DeviceID, Machine, Provider, OriginalPath string }
	RawArchiveFile struct {
		ID                   int64
		RootID, Path, SHA256 string
		Size, ModTimeNS      int64
		Covered              bool
	}
)

type RawArchiveSource struct {
	ManifestID, RootID, SourceKey, OriginalPath           string
	CanonicalJSON                                         []byte
	ParentReceipt, Receipt, ParseError, ProcessingVersion string
}

func rawArchiveFields(fields ...string) error {
	for _, s := range fields {
		if strings.TrimSpace(s) == "" || len(s) > 4096 || strings.ContainsRune(s, '\x00') {
			return errors.New("invalid raw archive field")
		}
	}
	return nil
}

func (d *DB) RegisterRawArchiveRoot(ctx context.Context, root RawArchiveRoot) error {
	if err := rawArchiveFields(root.ID, root.DeviceID, root.Machine, root.Provider, root.OriginalPath); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_roots WHERE device_id != ?`, root.DeviceID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("raw archive supports one original device")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_roots(id,device_id,machine,provider,original_path) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, root.ID, root.DeviceID, root.Machine, root.Provider, root.OriginalPath)
	if err != nil {
		return err
	}
	var got RawArchiveRoot
	err = tx.QueryRowContext(ctx, `SELECT id,device_id,machine,provider,original_path FROM raw_archive_roots WHERE id=?`, root.ID).Scan(&got.ID, &got.DeviceID, &got.Machine, &got.Provider, &got.OriginalPath)
	if err != nil {
		return err
	}
	if got != root {
		return errors.New("raw archive root identity conflict")
	}
	return tx.Commit()
}

func (d *DB) ListRawArchiveRoots(ctx context.Context) ([]RawArchiveRoot, error) {
	rows, err := d.getReader().QueryContext(ctx, `SELECT id,device_id,machine,provider,original_path FROM raw_archive_roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawArchiveRoot
	for rows.Next() {
		var r RawArchiveRoot
		if err = rows.Scan(&r.ID, &r.DeviceID, &r.Machine, &r.Provider, &r.OriginalPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) RecordRawArchiveFile(ctx context.Context, f RawArchiveFile) error {
	if err := rawArchiveFields(f.RootID, f.Path, f.SHA256); err != nil {
		return err
	}
	_, hashErr := hex.DecodeString(f.SHA256)
	if f.Size < 0 || len(f.SHA256) != 64 || hashErr != nil {
		return errors.New("invalid raw archive file")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	result, err := d.getWriter().ExecContext(ctx, `INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns,covered) VALUES(?,?,?,?,?,?) ON CONFLICT(root_id,path,sha256) DO UPDATE SET covered=max(covered,excluded.covered) WHERE size=excluded.size`, f.RootID, f.Path, f.SHA256, f.Size, f.ModTimeNS, f.Covered)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errors.New("raw archive file identity conflict")
	}
	return err
}

func (d *DB) ListRawArchiveFiles(ctx context.Context, afterID int64, limit int) ([]RawArchiveFile, error) {
	if limit <= 0 || limit > 10000 {
		return nil, errors.New("invalid raw archive page size")
	}
	rows, err := d.getReader().QueryContext(ctx, `SELECT id,root_id,path,sha256,size,mod_time_ns,covered FROM raw_archive_files WHERE id>? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawArchiveFile
	for rows.Next() {
		var f RawArchiveFile
		if err = rows.Scan(&f.ID, &f.RootID, &f.Path, &f.SHA256, &f.Size, &f.ModTimeNS, &f.Covered); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

const rawSourceColumns = `manifest_id,root_id,source_key,original_path,canonical_json,parent_receipt,receipt,parse_error,processing_version`

func scanRawSource(row interface{ Scan(...any) error }) (RawArchiveSource, error) {
	var s RawArchiveSource
	err := row.Scan(&s.ManifestID, &s.RootID, &s.SourceKey, &s.OriginalPath, &s.CanonicalJSON, &s.ParentReceipt, &s.Receipt, &s.ParseError, &s.ProcessingVersion)
	return s, err
}

func (d *DB) GetRawArchiveSource(ctx context.Context, id string) (RawArchiveSource, error) {
	return scanRawSource(d.getReader().QueryRowContext(ctx, `SELECT `+rawSourceColumns+` FROM raw_archive_sources WHERE manifest_id=?`, id))
}

func (d *DB) RawArchiveHead(ctx context.Context, root, key string) (*RawArchiveSource, error) {
	s, err := scanRawSource(d.getReader().QueryRowContext(ctx, `SELECT `+rawSourceColumns+` FROM raw_archive_sources WHERE manifest_id=(SELECT manifest_id FROM raw_archive_heads WHERE root_id=? AND source_key=?)`, root, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) AcceptRawArchiveSource(ctx context.Context, s RawArchiveSource) (RawArchiveSource, error) {
	if err := rawArchiveFields(s.ManifestID, s.RootID, s.SourceKey, s.OriginalPath); err != nil {
		return s, err
	}
	if len(s.CanonicalJSON) == 0 || len(s.CanonicalJSON) > 1<<20 || len(s.ParentReceipt) > 4096 {
		return s, errors.New("invalid raw archive source")
	}
	if s.Receipt != "" && s.Receipt != s.ManifestID {
		return s, errors.New("invalid raw archive receipt")
	}
	s.Receipt = s.ManifestID
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return s, err
	}
	defer func() { _ = tx.Rollback() }()
	got, err := scanRawSource(tx.QueryRowContext(ctx, `SELECT `+rawSourceColumns+` FROM raw_archive_sources WHERE manifest_id=?`, s.ManifestID))
	if err == nil {
		if got.RootID != s.RootID || got.SourceKey != s.SourceKey || got.OriginalPath != s.OriginalPath || got.ParentReceipt != s.ParentReceipt || !bytes.Equal(got.CanonicalJSON, s.CanonicalJSON) {
			return s, errors.New("raw archive manifest identity conflict")
		}
		return got, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	var receipt string
	err = tx.QueryRowContext(ctx, `SELECT manifest_id FROM raw_archive_heads WHERE root_id=? AND source_key=?`, s.RootID, s.SourceKey).Scan(&receipt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	if receipt != s.ParentReceipt {
		return s, errors.New("raw archive parent receipt conflict")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_sources(manifest_id,root_id,source_key,original_path,canonical_json,parent_receipt,receipt) VALUES(?,?,?,?,?,?,?)`, s.ManifestID, s.RootID, s.SourceKey, s.OriginalPath, s.CanonicalJSON, s.ParentReceipt, s.Receipt)
	if err != nil {
		return s, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_heads(root_id,source_key,manifest_id) VALUES(?,?,?) ON CONFLICT(root_id,source_key) DO UPDATE SET manifest_id=excluded.manifest_id`, s.RootID, s.SourceKey, s.ManifestID)
	if err != nil {
		return s, err
	}
	s.ParseError = ""
	s.ProcessingVersion = ""
	return s, tx.Commit()
}

func (d *DB) ListRawArchiveSources(ctx context.Context, after string, limit int) ([]RawArchiveSource, error) {
	if limit <= 0 || limit > 10000 {
		return nil, errors.New("invalid raw archive page size")
	}
	rows, err := d.getReader().QueryContext(ctx, `SELECT `+rawSourceColumns+` FROM raw_archive_sources WHERE manifest_id>? ORDER BY manifest_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawArchiveSource
	for rows.Next() {
		s, err := scanRawSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) RecordRawArchiveParse(ctx context.Context, id, version, parseError string) error {
	if err := rawArchiveFields(id, version); err != nil {
		return err
	}
	if len(parseError) > 64<<10 {
		return errors.New("raw archive parse error too large")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	result, err := d.getWriter().ExecContext(ctx, `UPDATE raw_archive_sources SET processing_version=?,parse_error=? WHERE manifest_id=?`, version, parseError, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

// SnapshotTo writes a consistent full archive into a newly reserved file.
func (d *DB) SnapshotTo(ctx context.Context, path string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	_, err = d.getWriter().ExecContext(ctx, `VACUUM INTO ?`, path)
	if err != nil {
		return fmt.Errorf("snapshot archive: %w", err)
	}
	return nil
}
