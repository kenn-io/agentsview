package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const rawUploadSpoolDirectory = "raw-upload-spool"

const (
	rawUploadCleanupBatch    = 128
	rawUploadCleanupInterval = 15 * time.Minute
	rawUploadCleanupTimeout  = 30 * time.Second
)

const rawUploadSessionColumns = `
    upload_id, tenant_id, device_id, provider, sha256, size_bytes,
    offset_bytes, generation, state, created_at, expires_at`

// RawUploadStore combines PostgreSQL offset fencing with a restart-safe spool.
type RawUploadStore struct {
	db            *sql.DB
	root          *os.Root
	closeOnce     sync.Once
	closeErr      error
	syncDirectory func() error
	cleanupCancel context.CancelFunc
	cleanupWG     sync.WaitGroup
	scanMu        sync.Mutex
	scanDirectory *os.File
}

// NewRawUploadStore opens the private spool below one agentsview data directory.
func NewRawUploadStore(database *sql.DB, dataDir string) (*RawUploadStore, error) {
	if database == nil {
		return nil, fmt.Errorf("%w: PostgreSQL connection is required", rawsync.ErrInvalid)
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("%w: raw upload data directory is required", rawsync.ErrInvalid)
	}
	root, err := openRawUploadSpool(dataDir, syncRawUploadDirectory)
	if err != nil {
		return nil, err
	}
	store := &RawUploadStore{db: database, root: root}
	store.syncDirectory = store.syncSpoolDirectory
	startupCtx, cancelStartup := context.WithTimeout(
		context.Background(), rawUploadCleanupTimeout,
	)
	err = store.cleanupExpiredAndOrphaned(startupCtx, time.Now().UTC())
	cancelStartup()
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("cleaning raw upload spool at startup: %w", err)
	}
	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	store.cleanupCancel = cancelCleanup
	store.cleanupWG.Go(func() {
		ticker := time.NewTicker(rawUploadCleanupInterval)
		defer ticker.Stop()
		store.runCleanupLoop(cleanupCtx, ticker.C)
	})
	return store, nil
}

func openRawUploadSpool(
	dataDir string,
	syncParent func(string) error,
) (*os.Root, error) {
	spoolPath, err := filepath.Abs(filepath.Join(dataDir, rawUploadSpoolDirectory))
	if err != nil {
		return nil, fmt.Errorf("resolving raw upload spool: %w", err)
	}
	if err := os.MkdirAll(spoolPath, 0o700); err != nil {
		return nil, fmt.Errorf("creating raw upload spool: %w", err)
	}
	if err := os.Chmod(spoolPath, 0o700); err != nil {
		return nil, fmt.Errorf("restricting raw upload spool: %w", err)
	}
	if err := syncParent(filepath.Dir(spoolPath)); err != nil {
		return nil, fmt.Errorf("syncing raw upload spool parent: %w", err)
	}
	root, err := os.OpenRoot(spoolPath)
	if err != nil {
		return nil, fmt.Errorf("opening raw upload spool: %w", err)
	}
	return root, nil
}

func syncRawUploadDirectory(path string) (returnErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
	if err := directory.Sync(); err != nil &&
		!isRawUploadDirectorySyncUnsupported(err) {
		return err
	}
	return nil
}

// Close releases the retained spool root. It is safe to call more than once.
func (s *RawUploadStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.cleanupCancel != nil {
			s.cleanupCancel()
		}
		s.cleanupWG.Wait()
		s.scanMu.Lock()
		if s.scanDirectory != nil {
			s.closeErr = s.scanDirectory.Close()
			s.scanDirectory = nil
		}
		s.scanMu.Unlock()
		s.closeErr = errors.Join(s.closeErr, s.root.Close())
	})
	return s.closeErr
}

func (s *RawUploadStore) Create(
	ctx context.Context,
	record rawsync.UploadSession,
) (rawsync.UploadSession, bool, error) {
	if err := rawsync.ValidateUploadSession(record); err != nil ||
		record.Offset != 0 || record.Complete {
		return rawsync.UploadSession{}, false, fmt.Errorf(
			"%w: invalid raw upload session record", rawsync.ErrInvalid,
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.UploadSession{}, false, fmt.Errorf("beginning raw upload create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var expiredID string
	err = tx.QueryRowContext(ctx, `
		UPDATE raw_upload_sessions
		SET state = 'expired', updated_at = $1
		WHERE tenant_id = $2 AND device_id = $3 AND provider = $4
			AND sha256 = $5 AND size_bytes = $6
			AND state = 'open' AND expires_at <= $1
		RETURNING upload_id`,
		record.CreatedAt, record.Identity.TenantID, record.Identity.DeviceID,
		record.Provider, record.Object.SHA256, record.Object.Length,
	).Scan(&expiredID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rawsync.UploadSession{}, false, fmt.Errorf("expiring prior raw upload: %w", err)
	}

	row := tx.QueryRowContext(ctx, `
		INSERT INTO raw_upload_sessions (
			upload_id, tenant_id, device_id, provider, sha256, size_bytes,
			offset_bytes, state, created_at, updated_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, 0, 'open', $7, $7, $8)
		ON CONFLICT (
			tenant_id, device_id, provider, sha256, size_bytes
		) WHERE state = 'open'
		DO UPDATE SET updated_at = raw_upload_sessions.updated_at
		RETURNING `+rawUploadSessionColumns+`, upload_id = $1`,
		record.ID, record.Identity.TenantID, record.Identity.DeviceID,
		record.Provider, record.Object.SHA256, record.Object.Length,
		record.CreatedAt, record.ExpiresAt,
	)
	session, state, created, err := scanRawUploadSession(row, true)
	if err != nil {
		return rawsync.UploadSession{}, false, fmt.Errorf("storing raw upload session: %w", err)
	}
	if state != rawUploadStateOpen {
		return rawsync.UploadSession{}, false, fmt.Errorf(
			"raw upload create returned a non-open session: %w", rawsync.ErrConflict,
		)
	}
	if err := tx.Commit(); err != nil {
		return rawsync.UploadSession{}, false, fmt.Errorf("committing raw upload create: %w", err)
	}
	if expiredID != "" {
		_ = s.removeRawUploadStage(expiredID)
	}
	return session, created, nil
}

func (s *RawUploadStore) Status(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	now time.Time,
) (rawsync.UploadSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("beginning raw upload status: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	session, state, err := loadRawUploadSessionLocked(ctx, tx, identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	retired, err := retireExpiredRawUploadSession(
		ctx, tx, session, state, uploadID, now,
	)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	if retired {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing raw upload expiry: %w", err)
		}
		_ = s.removeRawUploadStage(uploadID)
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if state == rawUploadStateExpired {
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("committing raw upload status: %w", err)
	}
	return session, nil
}

func (s *RawUploadStore) Append(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	expectedOffset int64,
	chunk []byte,
	now time.Time,
) (rawsync.UploadSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("beginning raw upload append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	session, state, err := loadRawUploadSessionLocked(ctx, tx, identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	retired, err := retireExpiredRawUploadSession(
		ctx, tx, session, state, uploadID, now,
	)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	if retired {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing raw upload expiry: %w", err)
		}
		_ = s.removeRawUploadStage(uploadID)
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if state == rawUploadStateComplete {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing completed upload retry: %w", err)
		}
		return session, nil
	}
	if state != rawUploadStateOpen {
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if session.Offset != expectedOffset {
		return rawsync.UploadSession{}, &rawsync.UploadOffsetConflictError{
			CurrentOffset: session.Offset,
		}
	}
	if int64(len(chunk)) > session.Object.Length-session.Offset {
		return rawsync.UploadSession{}, fmt.Errorf(
			"%w: raw upload chunk exceeds declared object length", rawsync.ErrInvalid,
		)
	}
	if err := s.writeRawUploadChunk(session, chunk); err != nil {
		return rawsync.UploadSession{}, err
	}
	newOffset := session.Offset + int64(len(chunk))
	result, err := tx.ExecContext(ctx, `
		UPDATE raw_upload_sessions
		SET offset_bytes = $1, updated_at = $2
		WHERE upload_id = $3 AND state = 'open' AND offset_bytes = $4`,
		newOffset, now, uploadID, session.Offset,
	)
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("advancing raw upload offset: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("checking raw upload offset: %w", err)
	}
	if updated != 1 {
		return rawsync.UploadSession{}, fmt.Errorf(
			"raw upload offset changed while appending: %w", rawsync.ErrConflict,
		)
	}
	if err := tx.Commit(); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("committing raw upload append: %w", err)
	}
	session.Offset = newOffset
	return session, nil
}

func (s *RawUploadStore) Open(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	now time.Time,
) (rawsync.UploadSession, io.ReadCloser, error) {
	session, err := s.Status(ctx, identity, uploadID, now)
	if err != nil {
		return rawsync.UploadSession{}, nil, err
	}
	if session.Complete {
		return rawsync.UploadSession{}, nil, rawsync.ErrNotFound
	}
	file, err := s.root.Open(rawUploadStageName(uploadID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rawsync.UploadSession{}, nil, rawsync.ErrNotFound
		}
		return rawsync.UploadSession{}, nil, fmt.Errorf("opening raw upload spool file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return rawsync.UploadSession{}, nil, fmt.Errorf("stating raw upload spool file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != session.Offset {
		_ = file.Close()
		return rawsync.UploadSession{}, nil, fmt.Errorf(
			"raw upload spool does not match durable offset: %w", rawsync.ErrConflict,
		)
	}
	return session, file, nil
}

func (s *RawUploadStore) Reset(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	expectedGeneration int64,
	now time.Time,
) (rawsync.UploadSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("beginning raw upload reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	session, state, err := loadRawUploadSessionLocked(ctx, tx, identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	retired, err := retireExpiredRawUploadSession(
		ctx, tx, session, state, uploadID, now,
	)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	if retired {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing raw upload expiry: %w", err)
		}
		_ = s.removeRawUploadStage(uploadID)
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if state != rawUploadStateOpen {
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if session.Generation != expectedGeneration {
		return rawsync.UploadSession{}, fmt.Errorf(
			"raw upload generation changed while resetting: %w", rawsync.ErrConflict,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE raw_upload_sessions
		SET offset_bytes = 0, generation = generation + 1, updated_at = $1
		WHERE upload_id = $2 AND state = 'open'`, now, uploadID,
	); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("resetting raw upload offset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("committing raw upload reset: %w", err)
	}
	session.Offset = 0
	session.Generation++
	return session, nil
}

func (s *RawUploadStore) Complete(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	now time.Time,
) (rawsync.UploadSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("beginning raw upload completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	session, state, err := loadRawUploadSessionLocked(ctx, tx, identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	retired, err := retireExpiredRawUploadSession(
		ctx, tx, session, state, uploadID, now,
	)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	if retired {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing raw upload expiry: %w", err)
		}
		_ = s.removeRawUploadStage(uploadID)
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if state == rawUploadStateComplete {
		if err := tx.Commit(); err != nil {
			return rawsync.UploadSession{}, fmt.Errorf("committing completed upload retry: %w", err)
		}
		return session, nil
	}
	if state != rawUploadStateOpen {
		return rawsync.UploadSession{}, rawsync.ErrNotFound
	}
	if session.Offset != session.Object.Length {
		return rawsync.UploadSession{}, fmt.Errorf(
			"%w: raw upload is incomplete", rawsync.ErrConflict,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE raw_upload_sessions
		SET state = 'complete', completed_at = $1, updated_at = $1
		WHERE upload_id = $2 AND state = 'open'`, now, uploadID,
	); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("completing raw upload: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return rawsync.UploadSession{}, fmt.Errorf("committing raw upload completion: %w", err)
	}
	session.Complete = true
	_ = s.removeRawUploadStage(uploadID)
	return session, nil
}

func (s *RawUploadStore) runCleanupLoop(
	ctx context.Context,
	ticks <-chan time.Time,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, rawUploadCleanupTimeout)
			err := s.cleanupExpiredAndOrphaned(cleanupCtx, now.UTC())
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("raw upload cleanup failed: %v", err)
			}
		}
	}
}

func (s *RawUploadStore) cleanupExpiredAndOrphaned(
	ctx context.Context,
	now time.Time,
) error {
	rows, err := s.db.QueryContext(ctx, `
		WITH expired AS (
			SELECT upload_id
			FROM raw_upload_sessions
			WHERE state = 'open' AND expires_at <= $1
			ORDER BY expires_at, upload_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE raw_upload_sessions AS sessions
		SET state = 'expired', updated_at = $1
		FROM expired
		WHERE sessions.upload_id = expired.upload_id
		RETURNING sessions.upload_id`, now, rawUploadCleanupBatch)
	if err != nil {
		return fmt.Errorf("expiring abandoned raw uploads: %w", err)
	}
	expiredIDs := make([]string, 0, rawUploadCleanupBatch)
	for rows.Next() {
		var uploadID string
		if err := rows.Scan(&uploadID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reading expired raw upload: %w", err)
		}
		expiredIDs = append(expiredIDs, uploadID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("finishing expired raw upload query: %w", err)
	}
	rows, err = s.db.QueryContext(ctx, `
		WITH terminal AS (
			SELECT upload_id
			FROM raw_upload_sessions
			WHERE state IN ('complete', 'expired') AND expires_at <= $1
			ORDER BY expires_at, upload_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM raw_upload_sessions AS sessions
		USING terminal
		WHERE sessions.upload_id = terminal.upload_id
		RETURNING sessions.upload_id`, now, rawUploadCleanupBatch)
	if err != nil {
		return fmt.Errorf("deleting terminal raw uploads: %w", err)
	}
	terminalIDs := make([]string, 0, rawUploadCleanupBatch)
	for rows.Next() {
		var uploadID string
		if err := rows.Scan(&uploadID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reading deleted raw upload: %w", err)
		}
		terminalIDs = append(terminalIDs, uploadID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("finishing terminal raw upload query: %w", err)
	}
	var cleanupErr error
	for _, uploadID := range append(expiredIDs, terminalIDs...) {
		cleanupErr = errors.Join(cleanupErr, s.removeRawUploadStage(uploadID))
	}

	stageNames, err := s.nextRawUploadStageNames(rawUploadCleanupBatch)
	if err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("scanning raw upload spool: %w", err))
	}
	for _, stageName := range stageNames {
		cleanupErr = errors.Join(
			cleanupErr, s.reconcileRawUploadStage(ctx, stageName, now),
		)
	}
	return cleanupErr
}

func (s *RawUploadStore) nextRawUploadStageNames(limit int) ([]string, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanDirectory == nil {
		directory, err := s.root.Open(".")
		if err != nil {
			return nil, err
		}
		s.scanDirectory = directory
	}
	entries, err := s.scanDirectory.ReadDir(limit)
	if errors.Is(err, io.EOF) {
		err = nil
		closeErr := s.scanDirectory.Close()
		s.scanDirectory = nil
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err != nil {
		closeErr := s.scanDirectory.Close()
		s.scanDirectory = nil
		return nil, errors.Join(err, closeErr)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func (s *RawUploadStore) reconcileRawUploadStage(
	ctx context.Context,
	stageName string,
	now time.Time,
) error {
	uploadID, ok := strings.CutSuffix(stageName, ".part")
	if !ok {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning raw upload spool reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT state, expires_at
		FROM raw_upload_sessions
		WHERE upload_id = $1
		FOR UPDATE`, uploadID).Scan(&state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Rollback(); err != nil {
			return fmt.Errorf("releasing orphaned raw upload lookup: %w", err)
		}
		return s.removeRawUploadStageName(stageName)
	}
	if err != nil {
		return fmt.Errorf("loading raw upload spool owner: %w", err)
	}
	remove := state != rawUploadStateOpen
	if state == rawUploadStateOpen && !now.Before(expiresAt) {
		if err := expireRawUploadSession(ctx, tx, uploadID, now); err != nil {
			return err
		}
		remove = true
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing raw upload spool reconciliation: %w", err)
	}
	if remove {
		return s.removeRawUploadStageName(stageName)
	}
	return nil
}

func (s *RawUploadStore) removeRawUploadStage(uploadID string) error {
	return s.removeRawUploadStageName(rawUploadStageName(uploadID))
}

func (s *RawUploadStore) removeRawUploadStageName(stageName string) error {
	err := s.root.Remove(stageName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.syncDirectory()
}

func (s *RawUploadStore) writeRawUploadChunk(
	session rawsync.UploadSession,
	chunk []byte,
) (returnErr error) {
	file, created, err := s.openRawUploadStage(rawUploadStageName(session.ID))
	if err != nil {
		return fmt.Errorf("opening raw upload spool for append: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
		if returnErr != nil && created {
			returnErr = errors.Join(returnErr, s.removeRawUploadStage(session.ID))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stating raw upload spool for append: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < session.Offset {
		return fmt.Errorf(
			"raw upload spool is behind durable offset: %w", rawsync.ErrConflict,
		)
	}
	if info.Size() > session.Offset {
		if err := file.Truncate(session.Offset); err != nil {
			return fmt.Errorf("discarding uncommitted raw upload tail: %w", err)
		}
	}
	if len(chunk) != 0 {
		written, err := file.WriteAt(chunk, session.Offset)
		if err != nil {
			return fmt.Errorf("writing raw upload chunk: %w", err)
		}
		if written != len(chunk) {
			return fmt.Errorf("writing raw upload chunk: %w", io.ErrShortWrite)
		}
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("syncing raw upload chunk: %w", err)
	}
	if created {
		if err := s.syncDirectory(); err != nil {
			return fmt.Errorf("syncing raw upload spool directory: %w", err)
		}
	}
	return nil
}

func (s *RawUploadStore) openRawUploadStage(name string) (*os.File, bool, error) {
	file, err := s.root.OpenFile(name, os.O_RDWR, 0o600)
	if err == nil {
		return file, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	file, err = s.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		file, err = s.root.OpenFile(name, os.O_RDWR, 0o600)
		return file, false, err
	}
	return file, err == nil, err
}

func (s *RawUploadStore) syncSpoolDirectory() (returnErr error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
	if err := directory.Sync(); err != nil &&
		!isRawUploadDirectorySyncUnsupported(err) {
		return err
	}
	return nil
}

const (
	rawUploadStateOpen     = "open"
	rawUploadStateComplete = "complete"
	rawUploadStateExpired  = "expired"
)

type rawUploadRow interface {
	Scan(...any) error
}

func scanRawUploadSession(
	row rawUploadRow,
	withCreated bool,
) (rawsync.UploadSession, string, bool, error) {
	var session rawsync.UploadSession
	var provider string
	var state string
	created := false
	destinations := []any{
		&session.ID,
		&session.Identity.TenantID,
		&session.Identity.DeviceID,
		&provider,
		&session.Object.SHA256,
		&session.Object.Length,
		&session.Offset,
		&session.Generation,
		&state,
		&session.CreatedAt,
		&session.ExpiresAt,
	}
	if withCreated {
		destinations = append(destinations, &created)
	}
	if err := row.Scan(destinations...); err != nil {
		return rawsync.UploadSession{}, "", false, err
	}
	session.Provider = parser.AgentType(provider)
	session.CreatedAt = session.CreatedAt.UTC()
	session.ExpiresAt = session.ExpiresAt.UTC()
	session.Complete = state == rawUploadStateComplete
	if err := rawsync.ValidateUploadSession(session); err != nil {
		return rawsync.UploadSession{}, "", false, fmt.Errorf(
			"validating stored raw upload session: %w", err,
		)
	}
	return session, state, created, nil
}

func loadRawUploadSessionLocked(
	ctx context.Context,
	tx *sql.Tx,
	identity rawsync.AuthIdentity,
	uploadID string,
) (rawsync.UploadSession, string, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+rawUploadSessionColumns+`
		FROM raw_upload_sessions
		WHERE upload_id = $1 AND tenant_id = $2 AND device_id = $3
		FOR UPDATE`, uploadID, identity.TenantID, identity.DeviceID)
	session, state, _, err := scanRawUploadSession(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return rawsync.UploadSession{}, "", rawsync.ErrNotFound
	}
	if err != nil {
		return rawsync.UploadSession{}, "", fmt.Errorf("loading raw upload session: %w", err)
	}
	return session, state, nil
}

func expireRawUploadSession(
	ctx context.Context,
	tx *sql.Tx,
	uploadID string,
	now time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE raw_upload_sessions
		SET state = 'expired', updated_at = $1
		WHERE upload_id = $2 AND state = 'open'`, now, uploadID,
	); err != nil {
		return fmt.Errorf("expiring raw upload session: %w", err)
	}
	return nil
}

func retireExpiredRawUploadSession(
	ctx context.Context,
	tx *sql.Tx,
	session rawsync.UploadSession,
	state string,
	uploadID string,
	now time.Time,
) (bool, error) {
	if now.Before(session.ExpiresAt) {
		return false, nil
	}
	if state == rawUploadStateOpen {
		if err := expireRawUploadSession(ctx, tx, uploadID, now); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM raw_upload_sessions
		WHERE upload_id = $1 AND state IN ('complete', 'expired')`,
		uploadID,
	); err != nil {
		return false, fmt.Errorf("deleting expired raw upload: %w", err)
	}
	return true, nil
}

func rawUploadStageName(uploadID string) string {
	return uploadID + ".part"
}

var _ rawsync.UploadSessionStore = (*RawUploadStore)(nil)
