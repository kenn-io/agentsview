package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const simpleFTSDirEnv = "AGENTSVIEW_SIMPLE_DIR"

const (
	chineseFTSFingerprintStatsKey = "messages_chinese_fts_fingerprint_v1"
	chineseFTSSchemaVersion       = "messages-chinese-fts-v3"
)

var (
	simpleFTSRuntimeConfig, simpleFTSRuntimeErr = discoverSimpleFTSRuntime()
)

const schemaChineseFTSPendingSessions = `
CREATE TABLE IF NOT EXISTS messages_chinese_fts_pending_sessions (
    session_id TEXT PRIMARY KEY,
    generation INTEGER NOT NULL CHECK (generation > 0)
);

DROP TRIGGER IF EXISTS sessions_chinese_pending_bi;
DROP TRIGGER IF EXISTS sessions_chinese_pending_bu;
DROP TRIGGER IF EXISTS sessions_chinese_pending_bd;

CREATE TRIGGER sessions_chinese_pending_bi
BEFORE INSERT ON sessions BEGIN
    INSERT INTO messages_chinese_fts_pending_sessions(session_id, generation)
        VALUES(new.id, 1)
    ON CONFLICT(session_id) DO UPDATE SET
        generation = messages_chinese_fts_pending_sessions.generation + 1;
END;

CREATE TRIGGER sessions_chinese_pending_bu
BEFORE UPDATE OF transcript_revision ON sessions
WHEN old.transcript_revision IS NOT new.transcript_revision BEGIN
    INSERT INTO messages_chinese_fts_pending_sessions(session_id, generation)
        VALUES(new.id, 1)
    ON CONFLICT(session_id) DO UPDATE SET
        generation = messages_chinese_fts_pending_sessions.generation + 1;
END;

CREATE TRIGGER sessions_chinese_pending_bd
BEFORE DELETE ON sessions BEGIN
    INSERT INTO messages_chinese_fts_pending_sessions(session_id, generation)
        VALUES(old.id, 1)
    ON CONFLICT(session_id) DO UPDATE SET
        generation = messages_chinese_fts_pending_sessions.generation + 1;
END;
`

type simpleFTSRuntime struct {
	libraryPath    string
	dictionaryPath string
	fingerprint    string
}

func (c simpleFTSRuntime) available() bool {
	return c.libraryPath != "" && c.dictionaryPath != "" && c.fingerprint != ""
}

func checkSimpleFTSRuntimeConfig() error {
	return simpleFTSRuntimeErr
}

func discoverSimpleFTSRuntime() (simpleFTSRuntime, error) {
	executablePath, err := os.Executable()
	if err != nil {
		executablePath = ""
	}
	return discoverSimpleFTSRuntimeFrom(executablePath, os.Getenv(simpleFTSDirEnv), runtime.GOOS)
}

func discoverSimpleFTSRuntimeFrom(
	executablePath, explicitDir, goos string,
) (simpleFTSRuntime, error) {
	libraryName, err := simpleFTSLibraryName(goos)
	if err != nil {
		if explicitDir != "" {
			return simpleFTSRuntime{}, err
		}
		// The sidecar is optional. Unsupported platforms retain the standard
		// FTS path unless the user explicitly requested a sidecar directory.
		return simpleFTSRuntime{}, nil
	}

	if explicitDir != "" {
		config, err := validateSimpleFTSRuntime(explicitDir, libraryName)
		if err != nil {
			return simpleFTSRuntime{}, fmt.Errorf(
				"invalid %s: %w", simpleFTSDirEnv, err,
			)
		}
		return config, nil
	}

	var candidates []string
	if executablePath != "" {
		executableDir := filepath.Dir(executablePath)
		candidates = append(candidates,
			filepath.Join(executableDir, "agentsview-simple"),
			filepath.Clean(filepath.Join(
				executableDir, "..", "lib", "agentsview", "simple",
			)),
		)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return simpleFTSRuntime{}, fmt.Errorf(
				"checking bundled simple FTS directory %s: %w", candidate, err,
			)
		}
		if !info.IsDir() {
			return simpleFTSRuntime{}, fmt.Errorf(
				"bundled simple FTS path is not a directory: %s", candidate,
			)
		}
		config, err := validateSimpleFTSRuntime(candidate, libraryName)
		if err != nil {
			return simpleFTSRuntime{}, fmt.Errorf(
				"invalid bundled simple FTS directory %s: %w", candidate, err,
			)
		}
		return config, nil
	}
	return simpleFTSRuntime{}, nil
}

func validateSimpleFTSRuntime(
	dir, libraryName string,
) (simpleFTSRuntime, error) {
	libraryPath := filepath.Join(dir, libraryName)
	if err := requireRegularFile(libraryPath); err != nil {
		return simpleFTSRuntime{}, fmt.Errorf("simple extension: %w", err)
	}
	dictionaryPath := filepath.Join(dir, "dict")
	for _, name := range simpleFTSDictionaryFiles {
		if err := requireRegularFile(filepath.Join(dictionaryPath, name)); err != nil {
			return simpleFTSRuntime{}, fmt.Errorf("cppjieba dictionary: %w", err)
		}
	}
	fingerprint, err := fingerprintSimpleFTSRuntime(libraryPath, dictionaryPath)
	if err != nil {
		return simpleFTSRuntime{}, err
	}
	return simpleFTSRuntime{
		libraryPath:    libraryPath,
		dictionaryPath: dictionaryPath,
		fingerprint:    fingerprint,
	}, nil
}

var simpleFTSDictionaryFiles = []string{
	"hmm_model.utf8",
	"idf.utf8",
	"jieba.dict.utf8",
	"stop_words.utf8",
	"user.dict.utf8",
}

func fingerprintSimpleFTSRuntime(
	libraryPath, dictionaryPath string,
) (string, error) {
	h := sha256.New()
	_, _ = io.WriteString(h, chineseFTSSchemaVersion+"\n")

	type fingerprintFile struct {
		name string
		path string
	}
	files := []fingerprintFile{{
		name: filepath.Base(libraryPath),
		path: libraryPath,
	}}
	for _, name := range simpleFTSDictionaryFiles {
		files = append(files, fingerprintFile{
			name: name,
			path: filepath.Join(dictionaryPath, name),
		})
	}
	for _, item := range files {
		_, _ = io.WriteString(h, item.name+"\x00")
		file, err := os.Open(item.path)
		if err != nil {
			return "", fmt.Errorf(
				"opening simple FTS fingerprint input %s: %w", item.path, err,
			)
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf(
				"hashing simple FTS fingerprint input %s: %w", item.path, copyErr,
			)
		}
		if closeErr != nil {
			return "", fmt.Errorf(
				"closing simple FTS fingerprint input %s: %w", item.path, closeErr,
			)
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return fmt.Sprintf("%s:%x", chineseFTSSchemaVersion, h.Sum(nil)), nil
}

func simpleFTSLibraryName(goos string) (string, error) {
	switch goos {
	case "linux":
		return "libsimple.so", nil
	case "darwin":
		return "libsimple.dylib", nil
	case "windows":
		return "simple.dll", nil
	default:
		return "", fmt.Errorf("simple FTS is unsupported on %s", goos)
	}
}

func requireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

type chineseFTSTransactor interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// ensureChineseFTS atomically reconciles the derived Chinese index with the
// loaded extension and dictionaries. The table, complete backfill, fingerprint,
// and connection-local maintenance triggers become visible together.
func ensureChineseFTS(
	ctx context.Context, conn chineseFTSTransactor, forceRebuild bool,
) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning Chinese FTS transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, trigger := range []string{
		"messages_chinese_ai",
		"messages_chinese_ad",
		"messages_chinese_au",
		"sessions_chinese_pending_ai",
		"sessions_chinese_pending_au",
		"sessions_chinese_pending_ad",
	} {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
			return fmt.Errorf("dropping Chinese FTS trigger %s: %w", trigger, err)
		}
	}

	var tableExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = 'messages_chinese_fts'
		)`).Scan(&tableExists); err != nil {
		return fmt.Errorf("checking Chinese FTS table: %w", err)
	}

	var pendingTableExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = 'table'
			  AND name = 'messages_chinese_fts_pending_sessions'
		)`).Scan(&pendingTableExists); err != nil {
		return fmt.Errorf("checking Chinese FTS freshness ledger table: %w", err)
	}

	trackFreshness := simpleFTSRuntimeConfig.available() ||
		tableExists || pendingTableExists
	if !trackFreshness {
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM stats WHERE key = ?", chineseFTSFingerprintStatsKey,
		); err != nil {
			return fmt.Errorf("clearing orphaned Chinese FTS fingerprint: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing disabled Chinese FTS state: %w", err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, schemaChineseFTSPendingSessions); err != nil {
		return fmt.Errorf("installing Chinese FTS freshness ledger: %w", err)
	}
	var pendingSessions int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pendingSessions); err != nil {
		return fmt.Errorf("checking Chinese FTS freshness ledger: %w", err)
	}

	if !simpleFTSRuntimeConfig.available() {
		if tableExists {
			if _, err := tx.ExecContext(ctx, "DROP TABLE messages_chinese_fts"); err != nil {
				return fmt.Errorf("dropping unavailable Chinese FTS: %w", err)
			}
		}
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM stats WHERE key = ?", chineseFTSFingerprintStatsKey,
		); err != nil {
			return fmt.Errorf("clearing Chinese FTS fingerprint: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing Chinese FTS removal: %w", err)
		}
		return nil
	}

	var storedFingerprint string
	fingerprintErr := tx.QueryRowContext(ctx,
		"SELECT CAST(value AS TEXT) FROM stats WHERE key = ?",
		chineseFTSFingerprintStatsKey,
	).Scan(&storedFingerprint)
	if fingerprintErr != nil && !errors.Is(fingerprintErr, sql.ErrNoRows) {
		return fmt.Errorf("reading Chinese FTS fingerprint: %w", fingerprintErr)
	}
	current := tableExists && fingerprintErr == nil && pendingSessions == 0 &&
		storedFingerprint == simpleFTSRuntimeConfig.fingerprint

	if forceRebuild || !current {
		if tableExists {
			if _, err := tx.ExecContext(ctx, "DROP TABLE messages_chinese_fts"); err != nil {
				return fmt.Errorf("dropping stale Chinese FTS: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, schemaChineseFTS); err != nil {
			return fmt.Errorf("creating Chinese FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages_chinese_fts(messages_chinese_fts) VALUES('rebuild')",
		); err != nil {
			return fmt.Errorf("backfilling Chinese FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stats (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			chineseFTSFingerprintStatsKey,
			simpleFTSRuntimeConfig.fingerprint,
		); err != nil {
			return fmt.Errorf("storing Chinese FTS fingerprint: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM messages_chinese_fts_pending_sessions",
		); err != nil {
			return fmt.Errorf("clearing Chinese FTS freshness ledger: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, schemaChineseFTSTriggers); err != nil {
		return fmt.Errorf("installing Chinese FTS triggers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing Chinese FTS transaction: %w", err)
	}
	return nil
}

func installChineseFTSTriggers(conn *sql.DB) error {
	return ensureChineseFTS(context.Background(), conn, false)
}
