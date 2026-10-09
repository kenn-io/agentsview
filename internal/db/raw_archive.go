package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
)

type (
	RawArchiveRoot struct{ ID, DeviceID, Machine, Provider, OriginalPath, ConfiguredRootID string }
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
	if err := rawArchiveFields(root.ID, root.DeviceID, root.Machine, root.Provider, root.OriginalPath, root.ConfiguredRootID); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_roots(id,device_id,machine,provider,original_path,configured_root_id) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET machine=excluded.machine`, root.ID, root.DeviceID, root.Machine, root.Provider, root.OriginalPath, root.ConfiguredRootID)
	if err != nil {
		return err
	}
	var got RawArchiveRoot
	err = tx.QueryRowContext(ctx, `SELECT id,device_id,machine,provider,original_path,configured_root_id FROM raw_archive_roots WHERE id=?`, root.ID).Scan(&got.ID, &got.DeviceID, &got.Machine, &got.Provider, &got.OriginalPath, &got.ConfiguredRootID)
	if err != nil {
		return err
	}
	if got != root {
		return errors.New("raw archive root identity conflict")
	}
	return tx.Commit()
}

func (d *DB) ListRawArchiveRoots(ctx context.Context) ([]RawArchiveRoot, error) {
	rows, err := d.getReader().QueryContext(ctx, `SELECT id,device_id,machine,provider,original_path,configured_root_id FROM raw_archive_roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawArchiveRoot
	for rows.Next() {
		var r RawArchiveRoot
		if err = rows.Scan(&r.ID, &r.DeviceID, &r.Machine, &r.Provider, &r.OriginalPath, &r.ConfiguredRootID); err != nil {
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
	return readRawArchiveFiles(rows, err)
}

// HasConflictingRawArchiveFiles reports whether flat extraction would need to
// write different retained versions to the same path.
func (d *DB) HasConflictingRawArchiveFiles(ctx context.Context) (bool, error) {
	var conflict bool
	err := d.getReader().QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM raw_archive_files GROUP BY root_id,path HAVING count(*) > 1
	)`).Scan(&conflict)
	return conflict, err
}

// RawArchiveCaptureEvidence finds the retained descriptor and inventory even
// when no source from that capture was accepted. Three rows expose ambiguity
// without reading every retained version.
func (d *DB) RawArchiveCaptureEvidence(ctx context.Context, captureID string) ([]RawArchiveFile, error) {
	rows, err := d.getReader().QueryContext(ctx, `SELECT id,root_id,path,sha256,size,mod_time_ns,covered FROM raw_archive_files
		WHERE root_id IN (SELECT id FROM raw_archive_roots WHERE provider='files' AND configured_root_id='capture-evidence' AND original_path='capture-evidence')
		AND path IN (?,?) LIMIT 3`, captureID+"/capture.json", captureID+"/inventory.json")
	return readRawArchiveFiles(rows, err)
}

func readRawArchiveFiles(rows *sql.Rows, err error) ([]RawArchiveFile, error) {
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

var ErrRawArchiveDeletionConflict = errors.New("source deletion state conflicts with its first accepted capture")

// RawArchiveSuppression retains the source database's trash/permanent-delete
// distinction. Empty Provider follows excluded_sessions' global native-ID scope.
type RawArchiveSuppression struct {
	ParserID string `json:"parser_id"`
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind"`
}

// RegisterRawArchiveDevice freezes the source's deletion policy. A changed
// capture cannot silently alter an already accepted projection.
func (d *DB) RegisterRawArchiveDevice(ctx context.Context, device, label string, suppressions []RawArchiveSuppression) error {
	data, err := json.Marshal(suppressions)
	if err != nil {
		return err
	}
	return d.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO raw_archive_devices(device_id,suppressions) VALUES(?,?) ON CONFLICT(device_id) DO NOTHING`, device, data); err != nil {
			return err
		}
		var existing []byte
		if err := tx.QueryRowContext(ctx, `SELECT suppressions FROM raw_archive_devices WHERE device_id=?`, device).Scan(&existing); err != nil {
			return err
		}
		if !bytes.Equal(existing, data) {
			return ErrRawArchiveDeletionConflict
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pg_sync_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, MachineLabelKeyPrefix+device, label)
		return err
	})
}

func (d *DB) RawArchiveSuppressions(ctx context.Context, device string) ([]RawArchiveSuppression, error) {
	var data []byte
	if err := d.getReader().QueryRowContext(ctx, `SELECT suppressions FROM raw_archive_devices WHERE device_id=?`, device).Scan(&data); err != nil {
		return nil, err
	}
	var out []RawArchiveSuppression
	err := json.Unmarshal(data, &out)
	return out, err
}

// BindRawArchiveSession records both the provider-qualified parser identity
// and its raw source alias. The caller publishes this with content via the
// checked scratch-database swap; no session row is required for a suppression.
func (d *DB) BindRawArchiveSession(ctx context.Context, root RawArchiveRoot, sourceKey, parserID, sessionID string) (known bool, err error) {
	err = d.Update(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO raw_archive_sessions(device_id,provider,parser_id,session_id,root_id,source_key) VALUES(?,?,?,?,?,?) ON CONFLICT(device_id,provider,parser_id) DO NOTHING`, root.DeviceID, root.Provider, parserID, sessionID, root.ID, sourceKey)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		known = n == 0
		var storedID, storedRoot, storedSource string
		if err := tx.QueryRowContext(ctx, `SELECT session_id,root_id,source_key FROM raw_archive_sessions WHERE device_id=? AND provider=? AND parser_id=?`, root.DeviceID, root.Provider, parserID).Scan(&storedID, &storedRoot, &storedSource); err != nil {
			return err
		}
		if storedID != sessionID || storedRoot != root.ID || storedSource != sourceKey {
			return errors.New("session identity conflicts with another selected source or recorded raw alias")
		}
		return nil
	})
	return known, err
}

// TrashedSessionsByFilePath maps each trashed session stored for one of the
// given source paths to its machine key. Sync leaves these rows in trash
// without parsing them again, so archive reparse counts them as suppressed.
func (d *DB) TrashedSessionsByFilePath(ctx context.Context, agent string, paths []string) (map[string]string, error) {
	out := make(map[string]string)
	for _, path := range paths {
		rows, err := d.getReader().QueryContext(ctx, `SELECT id, machine FROM sessions INDEXED BY idx_sessions_file_path
			WHERE file_path = ? AND agent = ? AND deleted_at IS NOT NULL`, path, agent)
		if err != nil {
			return nil, fmt.Errorf("listing trashed sessions for a source: %w", err)
		}
		for rows.Next() {
			var id, machine string
			if err := rows.Scan(&id, &machine); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = machine
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
