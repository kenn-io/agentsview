package rawcheckpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// RecoveryReport describes bounded startup repair of the private object spool.
type RecoveryReport struct {
	TemporaryFiles     int
	UnreferencedFiles  int
	Reservations       int
	InvalidGenerations int
	Garbage            GarbageCollectionReport
}

// Recover removes incomplete files and invalidates the complete local suffix
// behind any missing or wrong-sized referenced object. Acknowledged heads and
// their retained append metadata never advance during recovery.
func (s *Store) Recover(ctx context.Context) (RecoveryReport, error) {
	s.publicationMu.Lock()
	report, err := s.recoverObjectSpool(ctx)
	s.publicationMu.Unlock()
	if err != nil {
		return report, err
	}
	report.Garbage, err = s.CollectGarbage(ctx)
	return report, err
}

func (s *Store) recoverObjectSpool(ctx context.Context) (RecoveryReport, error) {
	s.objectMu.Lock()
	var report RecoveryReport
	entries, err := os.ReadDir(s.CaptureTempDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.objectMu.Unlock()
		return report, fmt.Errorf("rawcheckpoint: read temporary spool: %s",
			checkpointFilesystemError(err))
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(s.CaptureTempDir(), entry.Name())); err != nil {
			s.objectMu.Unlock()
			return report, fmt.Errorf("rawcheckpoint: remove temporary spool entry: %s",
				checkpointFilesystemError(err))
		}
		report.TemporaryFiles++
	}

	known := make(map[string]rawsync.ObjectRef)
	rows, err := s.db.QueryContext(ctx, `SELECT sha256, length, state FROM outbox_objects`)
	if err != nil {
		s.objectMu.Unlock()
		return report, fmt.Errorf("rawcheckpoint: list recovery objects: %w", err)
	}
	var broken []rawsync.ObjectRef
	for rows.Next() {
		var ref rawsync.ObjectRef
		var state string
		if err := rows.Scan(&ref.SHA256, &ref.Length, &state); err != nil {
			rows.Close()
			s.objectMu.Unlock()
			return report, fmt.Errorf("rawcheckpoint: list recovery objects: %w", err)
		}
		path := filepath.Clean(s.ObjectPath(ref))
		if state == "remote" {
			continue
		}
		known[path] = ref
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != ref.Length {
			broken = append(broken, ref)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.objectMu.Unlock()
		return report, fmt.Errorf("rawcheckpoint: list recovery objects: %w", err)
	}
	if err := rows.Close(); err != nil {
		s.objectMu.Unlock()
		return report, fmt.Errorf("rawcheckpoint: list recovery objects: %w", err)
	}
	objectsRoot := filepath.Join(s.spoolDir, "objects", "sha256")
	err = filepath.WalkDir(objectsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if _, ok := known[filepath.Clean(path)]; ok {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		report.UnreferencedFiles++
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.objectMu.Unlock()
		return report, fmt.Errorf("rawcheckpoint: sweep unreferenced objects: %s",
			checkpointFilesystemError(err))
	}
	err = s.withImmediateWrite(ctx, "recover object spool state", func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT provider, configured_root_id, source_key
			FROM outbox_reservations`)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: recover: list stale reservations: %w", err)
		}
		var interrupted []SourceIdentity
		for rows.Next() {
			var source SourceIdentity
			if err := rows.Scan(
				&source.Provider, &source.ConfiguredRootID, &source.SourceKey,
			); err != nil {
				rows.Close()
				return fmt.Errorf("rawcheckpoint: recover: list stale reservations: %w", err)
			}
			interrupted = append(interrupted, source)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("rawcheckpoint: recover: list stale reservations: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("rawcheckpoint: recover: list stale reservations: %w", err)
		}
		for _, source := range interrupted {
			if err := setSourceCoverageDegradedConn(
				ctx, conn, source, "capture_interrupted", s.now().UTC(),
			); err != nil {
				return err
			}
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM outbox_reservations`)
		if err != nil {
			return fmt.Errorf("rawcheckpoint: recover: clear stale reservations: %w", err)
		}
		cleared, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rawcheckpoint: recover: count stale reservations: %w", err)
		}
		report.Reservations = int(cleared)
		if len(broken) != 0 {
			invalid, err := invalidGenerationSuffixConn(ctx, conn, broken)
			if err != nil {
				return err
			}
			for _, captureID := range invalid {
				if err := releaseGenerationObjectsConn(ctx, conn, captureID); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx,
					`DELETE FROM outbox_entries WHERE capture_id = ?`, captureID); err != nil {
					return fmt.Errorf("rawcheckpoint: recover: release invalid entries: %w", err)
				}
			}
			report.InvalidGenerations = len(invalid)
			if err := resetInvalidSourcesConn(ctx, conn, invalid, s); err != nil {
				return err
			}
			for _, captureID := range invalid {
				if _, err := conn.ExecContext(ctx,
					`DELETE FROM outbox_generations WHERE capture_id = ?`, captureID); err != nil {
					return fmt.Errorf("rawcheckpoint: recover: compact invalid generation: %w", err)
				}
			}
			if _, err := conn.ExecContext(ctx, `UPDATE outbox_objects
				SET state = 'garbage_pending'
				WHERE ref_count = 0 AND state != 'remote'`); err != nil {
				return fmt.Errorf("rawcheckpoint: recover: mark garbage: %w", err)
			}
		}
		return nil
	})
	s.objectMu.Unlock()
	if err != nil {
		return report, err
	}
	return report, nil
}

// VerifyObject performs explicit full-digest verification for one retained
// local object without making startup scan every object byte.
func (s *Store) VerifyObject(ctx context.Context, ref rawsync.ObjectRef) error {
	if _, err := rawsync.NewObjectRef(ref.SHA256, ref.Length); err != nil {
		return err
	}
	s.objectMu.Lock()
	defer s.objectMu.Unlock()
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM outbox_objects
		WHERE sha256 = ? AND length = ?`, ref.SHA256, ref.Length).Scan(&present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rawsync.ErrNotFound
		}
		return fmt.Errorf("rawcheckpoint: verify object: %w", err)
	}
	digest, length, err := hashCheckpointObject(ctx, s.ObjectPath(ref))
	if err != nil || digest != ref.SHA256 || length != ref.Length {
		return fmt.Errorf("rawcheckpoint: verify object: %w", rawsync.ErrInvalid)
	}
	return nil
}

func invalidGenerationSuffixConn(
	ctx context.Context,
	conn *sql.Conn,
	broken []rawsync.ObjectRef,
) ([]string, error) {
	invalid := make(map[string]struct{})
	for _, ref := range broken {
		rows, err := conn.QueryContext(ctx, `WITH RECURSIVE suffix(capture_id) AS (
			SELECT entry_object.capture_id FROM outbox_entry_objects AS entry_object
			JOIN outbox_generations AS generation
				ON generation.capture_id = entry_object.capture_id
			WHERE entry_object.sha256 = ? AND entry_object.length = ?
				AND generation.state != 'acknowledged'
			UNION
			SELECT generation.capture_id FROM outbox_generations AS generation
			JOIN suffix ON generation.predecessor_capture_id = suffix.capture_id
			WHERE generation.state != 'acknowledged'
		)
		SELECT capture_id FROM suffix`, ref.SHA256, ref.Length)
		if err != nil {
			return nil, fmt.Errorf("rawcheckpoint: find broken generation suffix: %w", err)
		}
		for rows.Next() {
			var captureID string
			if err := rows.Scan(&captureID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("rawcheckpoint: find broken generation suffix: %w", err)
			}
			invalid[captureID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("rawcheckpoint: find broken generation suffix: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("rawcheckpoint: find broken generation suffix: %w", err)
		}
	}
	result := make([]string, 0, len(invalid))
	for captureID := range invalid {
		result = append(result, captureID)
	}
	sort.Strings(result)
	return result, nil
}

func resetInvalidSourcesConn(
	ctx context.Context,
	conn *sql.Conn,
	invalid []string,
	store *Store,
) error {
	seen := make(map[SourceIdentity]struct{})
	for _, captureID := range invalid {
		var source SourceIdentity
		if err := conn.QueryRowContext(ctx, `SELECT provider, configured_root_id,
			source_key FROM outbox_generations WHERE capture_id = ?`, captureID,
		).Scan(&source.Provider, &source.ConfiguredRootID, &source.SourceKey); err != nil {
			return fmt.Errorf("rawcheckpoint: recover: read invalid source: %w", err)
		}
		seen[source] = struct{}{}
	}
	for source := range seen {
		var headManifestID, headCaptureID string
		if err := conn.QueryRowContext(ctx, `SELECT head_manifest_id, head_capture_id FROM raw_sources
			WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			string(source.Provider), source.ConfiguredRootID, source.SourceKey,
		).Scan(&headManifestID, &headCaptureID); err != nil {
			return fmt.Errorf("rawcheckpoint: recover: read acknowledged head: %w", err)
		}
		latest := ""
		if headManifestID != "" {
			latest = headCaptureID
		}
		if _, err := conn.ExecContext(ctx, `UPDATE raw_sources SET latest_capture_id = ?,
			updated_at = ? WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
			latest, store.now().UTC().Format(time.RFC3339Nano),
			string(source.Provider), source.ConfiguredRootID, source.SourceKey); err != nil {
			return fmt.Errorf("rawcheckpoint: recover: reset capture base: %w", err)
		}
		if err := setSourceCoverageDegradedConn(ctx, conn, source,
			"missing_object", store.now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func hashCheckpointObject(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var length int64
	for {
		if err := ctx.Err(); err != nil {
			return "", length, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			written, writeErr := hash.Write(buffer[:read])
			length += int64(written)
			if writeErr != nil {
				return "", length, writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return hex.EncodeToString(hash.Sum(nil)), length, nil
			}
			return "", length, readErr
		}
	}
}
