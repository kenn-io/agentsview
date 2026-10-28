package archivesource

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

// Saved databases are inspected for capabilities, never opened by migrating code.
var requiredColumns = map[string]map[string]string{
	"archive_metadata": {"key": "TEXT", "value": "TEXT"},
	"raw_archive_roots": {
		"id": "TEXT", "device_id": "TEXT", "machine": "TEXT", "provider": "TEXT",
		"original_path": "TEXT", "configured_root_id": "TEXT",
	},
	"raw_archive_sources": {
		"manifest_id": "TEXT", "root_id": "TEXT", "source_key": "TEXT", "original_path": "TEXT",
		"canonical_json": "BLOB", "parse_error": "TEXT", "processing_version": "TEXT",
	},
	"raw_archive_files": {
		"id": "INTEGER", "root_id": "TEXT", "path": "TEXT", "sha256": "TEXT", "size": "INTEGER",
		"mod_time_ns": "INTEGER", "covered": "INTEGER",
	},
	"raw_archive_devices":      {"device_id": "TEXT", "suppressions": "BLOB"},
	"raw_archive_root_aliases": {"root_id": "TEXT", "alias": "TEXT"},
	"raw_archive_sessions": {
		"device_id": "TEXT", "provider": "TEXT", "parser_id": "TEXT", "session_id": "TEXT", "root_id": "TEXT", "source_key": "TEXT",
	},
}

func checkSavedSchema(ctx context.Context, source *sql.DB) (string, error) {
	for table, required := range requiredColumns {
		if err := checkColumns(ctx, source, table, required); err != nil {
			return "", err
		}
	}
	var version int
	if err := source.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return "", err
	}
	_, archiveOnly := db.DecodeUserVersion(version)
	var marker, archiveID string
	if err := source.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='archive_only'`).Scan(&marker); err != nil {
		return "", fmt.Errorf("reading archive-only marker: %w", err)
	}
	if !archiveOnly || marker != "1" {
		return "", errors.New("saved database must have consistent archive-only markers")
	}
	if err := source.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='archive_id'`).Scan(&archiveID); err != nil {
		return "", err
	}
	if strings.TrimSpace(archiveID) == "" {
		return "", errors.New("saved archive identity is empty")
	}
	return archiveID, nil
}

func checkColumns(ctx context.Context, source *sql.DB, table string, required map[string]string) error {
	rows, err := source.QueryContext(ctx, `SELECT name,type FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := make(map[string]string)
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return err
		}
		found[name] = strings.ToUpper(kind)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, kind := range required {
		if found[name] != kind {
			return fmt.Errorf("unsupported source schema: %s.%s must have type %s", table, name, kind)
		}
	}
	return nil
}

func buildCompactIndex(ctx context.Context, savedURI, output string, info docbank.BackupReaderInfo, checkObject func(context.Context, string, rawsync.ObjectRef) error) error {
	if err := validateSavedDatabase(ctx, savedURI, checkObject); err != nil {
		return err
	}
	return writeCompactIndex(ctx, savedURI, output, info)
}

func validateSavedDatabase(ctx context.Context, savedURI string, checkObject func(context.Context, string, rawsync.ObjectRef) error) (retErr error) {
	source, err := sql.Open("sqlite3", savedURI)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	archiveID, err := checkSavedSchema(ctx, source)
	if err != nil {
		return err
	}
	return validateSavedRecords(ctx, source, archiveID, checkObject)
}

// writeCompactIndex copies acceptance records without validating them; callers
// validate the saved database first or reuse a catalog validated earlier.
func writeCompactIndex(ctx context.Context, savedURI, output string, info docbank.BackupReaderInfo) (retErr error) {
	source, err := sql.Open("sqlite3", savedURI)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	archiveID, err := checkSavedSchema(ctx, source)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	index, err := sql.Open("sqlite3", output)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, index.Close()) }()
	index.SetMaxOpenConns(1)
	if _, err := index.ExecContext(ctx, `ATTACH DATABASE ? AS saved`, savedURI); err != nil {
		return err
	}
	tx, err := index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := copyIndexTables(ctx, tx, archiveID, info); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = index.ExecContext(ctx, `DETACH DATABASE saved`)
	return err
}

func copyIndexTables(ctx context.Context, tx *sql.Tx, archiveID string, info docbank.BackupReaderInfo) error {
	statements := []string{
		`CREATE TABLE metadata(version INTEGER NOT NULL,archive_id TEXT NOT NULL,repository_id TEXT NOT NULL,snapshot_id TEXT NOT NULL)`,
		`CREATE TABLE roots AS SELECT id,device_id,machine,provider,original_path,configured_root_id FROM saved.raw_archive_roots`,
		`CREATE UNIQUE INDEX roots_id ON roots(id)`,
		`CREATE TABLE sources AS SELECT manifest_id,root_id,source_key,original_path,canonical_json FROM saved.raw_archive_sources`,
		`CREATE UNIQUE INDEX sources_id ON sources(manifest_id)`,
		`CREATE TABLE devices AS SELECT device_id,suppressions FROM saved.raw_archive_devices`,
		`CREATE UNIQUE INDEX devices_id ON devices(device_id)`,
		`CREATE TABLE root_aliases AS SELECT root_id,alias FROM saved.raw_archive_root_aliases`,
		`CREATE TABLE sessions AS SELECT device_id,provider,parser_id,session_id,root_id,source_key FROM saved.raw_archive_sessions`,
		`CREATE TABLE coverage AS SELECT (SELECT count(*) FROM sources) AS supported_sources,
			(SELECT count(*) FROM saved.raw_archive_files WHERE covered=0) AS supplemental_files`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO metadata(version,archive_id,repository_id,snapshot_id) VALUES(?,?,?,?)`,
		indexVersion, archiveID, info.RepositoryID, info.SnapshotID)
	return err
}

func validateSavedRecords(ctx context.Context, source *sql.DB, archiveID string, checkObject func(context.Context, string, rawsync.ObjectRef) error) error {
	roots, err := readRoots(ctx, source)
	if err != nil {
		return err
	}
	if err := validateDevices(ctx, source, roots); err != nil {
		return err
	}
	if err := validateSources(ctx, source, roots, archiveID, checkObject); err != nil {
		return err
	}
	for _, query := range []string{
		`SELECT EXISTS(SELECT 1 FROM raw_archive_files f WHERE size<0 OR covered NOT IN (0,1)
		 OR NOT EXISTS(SELECT 1 FROM raw_archive_roots r WHERE r.id=f.root_id))`,
		`SELECT EXISTS(SELECT 1 FROM raw_archive_root_aliases a WHERE trim(alias)=''
		 OR NOT EXISTS(SELECT 1 FROM raw_archive_roots r WHERE r.id=a.root_id))`,
		`SELECT EXISTS(SELECT 1 FROM raw_archive_sessions a WHERE trim(parser_id)='' OR instr(parser_id,'~')>0 OR trim(session_id)=''
		 OR NOT EXISTS(SELECT 1 FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id
		 WHERE s.root_id=a.root_id AND s.source_key=a.source_key AND r.device_id=a.device_id AND r.provider=a.provider))`,
	} {
		var invalid bool
		if err := source.QueryRowContext(ctx, query).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return errors.New("inconsistent saved archive references")
		}
	}
	return nil
}

func readRoots(ctx context.Context, source *sql.DB) (map[string]db.RawArchiveRoot, error) {
	rows, err := source.QueryContext(ctx, `SELECT id,device_id,machine,provider,original_path,configured_root_id FROM raw_archive_roots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]db.RawArchiveRoot)
	for rows.Next() {
		var root db.RawArchiveRoot
		if err := rows.Scan(&root.ID, &root.DeviceID, &root.Machine, &root.Provider, &root.OriginalPath, &root.ConfiguredRootID); err != nil {
			return nil, err
		}
		if err := config.ValidateInstallationID(root.DeviceID); err != nil {
			return nil, errors.New("invalid source device identity")
		}
		for _, field := range []string{root.ID, root.Machine, root.Provider, root.OriginalPath, root.ConfiguredRootID} {
			if strings.TrimSpace(field) == "" || strings.ContainsRune(field, '\x00') || len(field) > 4096 {
				return nil, errors.New("invalid source root field")
			}
		}
		if _, duplicate := result[root.ID]; duplicate {
			return nil, errors.New("duplicate source root")
		}
		result[root.ID] = root
	}
	return result, rows.Err()
}

func validateDevices(ctx context.Context, source *sql.DB, roots map[string]db.RawArchiveRoot) error {
	rows, err := source.QueryContext(ctx, `SELECT device_id,suppressions FROM raw_archive_devices`)
	if err != nil {
		return err
	}
	defer rows.Close()
	devices := make(map[string]bool)
	for rows.Next() {
		var device string
		var data []byte
		if err := rows.Scan(&device, &data); err != nil {
			return err
		}
		if err := config.ValidateInstallationID(device); err != nil || devices[device] {
			return errors.New("invalid or repeated archive device")
		}
		var suppressions []db.RawArchiveSuppression
		if err := json.Unmarshal(data, &suppressions); err != nil {
			return fmt.Errorf("reading source suppression policy: %w", err)
		}
		seen := make(map[string]bool)
		for _, item := range suppressions {
			if item.ParserID == "" || strings.Contains(item.ParserID, "~") || seen[item.ParserID] ||
				(item.Kind != "trashed" && item.Kind != "deleted") || (item.Kind == "trashed" && item.Provider == "") {
				return errors.New("invalid source suppression policy")
			}
			seen[item.ParserID] = true
		}
		devices[device] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, root := range roots {
		if !devices[root.DeviceID] {
			return errors.New("archive root has no device suppression policy")
		}
	}
	return nil
}

func validateSources(ctx context.Context, source *sql.DB, roots map[string]db.RawArchiveRoot, archiveID string, checkObject func(context.Context, string, rawsync.ObjectRef) error) error {
	rows, err := source.QueryContext(ctx, `SELECT manifest_id,root_id,source_key,canonical_json FROM raw_archive_sources`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, rootID, key string
		var canonical []byte
		if err := rows.Scan(&id, &rootID, &key, &canonical); err != nil {
			return err
		}
		root, ok := roots[rootID]
		if !ok || (root.Provider != "claude" && root.Provider != "codex") {
			return errors.New("accepted source has no supported provider root")
		}
		identity, err := rawsync.NewAuthIdentity(archiveID, root.DeviceID)
		if err != nil {
			return err
		}
		manifest, err := rawsync.ParseCanonicalManifest(identity, id, canonical, rawsync.DefaultManifestLimits())
		if err != nil {
			return err
		}
		if manifest.Manifest.ConfiguredRootID != root.ConfiguredRootID || string(manifest.Manifest.Provider) != root.Provider || manifest.Manifest.SourceKey != key {
			return errors.New("accepted source differs from its recorded root")
		}
		for _, ref := range manifest.Objects {
			if err := checkObject(ctx, archiveID, ref); err != nil {
				return fmt.Errorf("source object %s: %w", ref.SHA256, err)
			}
		}
	}
	return rows.Err()
}
