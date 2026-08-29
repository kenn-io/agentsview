package rawcapture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

const (
	sqliteBackupTimeout    = 5 * time.Minute
	sqliteBackupRetryDelay = 25 * time.Millisecond
)

func (c *Capturer) snapshotSQLitePlan(
	ctx context.Context,
	plan parser.RawCapturePlan,
	source *sql.Conn,
	maxBytes int64,
) (parser.RawCapturePlan, func() error, error) {
	if len(plan.Entries) != 1 {
		return parser.RawCapturePlan{}, nil, ErrUnsupportedSnapshot
	}
	temporary, err := os.MkdirTemp(c.store.CaptureTempDir(), "sqlite-")
	if err != nil {
		return parser.RawCapturePlan{}, nil, fmt.Errorf("rawcapture: create SQLite snapshot directory: %w", err)
	}
	cleanup := func() error {
		if err := c.files.removeAll(temporary); err != nil {
			return fmt.Errorf("rawcapture: remove SQLite snapshot: %w", err)
		}
		return nil
	}
	destination := filepath.Join(temporary, "snapshot.db")
	backupCtx, cancel := context.WithTimeout(ctx, sqliteBackupTimeout)
	defer cancel()
	if err := c.sqliteBackup(
		backupCtx, source, destination, maxBytes,
	); err != nil {
		return parser.RawCapturePlan{}, nil, errors.Join(err, cleanup())
	}
	return parser.RawCapturePlan{
		ConfiguredRoot: temporary,
		CaptureRoot:    temporary,
		SourceKey:      plan.SourceKey,
		Entries: []parser.RawCaptureEntry{{
			Path: plan.Entries[0].Path, LocalPath: destination,
		}},
	}, cleanup, nil
}

type sqliteSnapshotSource struct {
	database         *sql.DB
	connection       *sql.Conn
	pathPin          *os.File
	path             string
	expectedInfo     os.FileInfo
	expectedIdentity string
}

func openSQLiteSnapshotSource(
	ctx context.Context,
	sourcePath string,
	expected os.FileInfo,
) (*sqliteSnapshotSource, error) {
	pathPin, err := pinSQLiteSnapshotPath(sourcePath, expected)
	if err != nil {
		if errors.Is(err, ErrSourceChanged) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrSourceChanged
		}
		return nil, fmt.Errorf("rawcapture: pin SQLite source: %w", err)
	}
	pinInfo, err := pathPin.Stat()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("rawcapture: inspect pinned SQLite source: %w", err),
			pathPin.Close(),
		)
	}
	expectedIdentity := stableFileIdentity(pathPin, pinInfo)
	if expectedIdentity == "" {
		return nil, errors.Join(ErrSourceChanged, pathPin.Close())
	}
	pathInfo, err := os.Stat(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(ErrSourceChanged, pathPin.Close())
		}
		return nil, errors.Join(
			fmt.Errorf("rawcapture: inspect SQLite source: %w", err),
			pathPin.Close(),
		)
	}
	if expected == nil || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(expected, pathInfo) {
		return nil, errors.Join(ErrSourceChanged, pathPin.Close())
	}
	database, err := sql.Open(
		sqliteSnapshotDriverName, sqliteSnapshotDSN(sourcePath, true),
	)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("rawcapture: open SQLite source: %w", err), pathPin.Close(),
		)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("rawcapture: connect SQLite source: %w", err),
			database.Close(), pathPin.Close(),
		)
	}
	source := &sqliteSnapshotSource{
		database: database, connection: connection, pathPin: pathPin,
		path: sourcePath, expectedInfo: expected, expectedIdentity: expectedIdentity,
	}
	actualPath, err := sqliteSnapshotMainPath(ctx, connection)
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	if !sameSQLiteSnapshotPath(actualPath, sourcePath) {
		return nil, errors.Join(ErrSourceChanged, source.Close())
	}
	if err := source.verifyCurrent(); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

func (s *sqliteSnapshotSource) verifyCurrent() error {
	pathInfo, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSourceChanged
		}
		return fmt.Errorf("rawcapture: inspect SQLite source: %w", err)
	}
	if !pathInfo.Mode().IsRegular() ||
		!os.SameFile(s.expectedInfo, pathInfo) {
		return ErrSourceChanged
	}
	identity, err := sqliteSnapshotConnectionIdentity(s.connection)
	if err != nil {
		return err
	}
	if identity == "" || identity != s.expectedIdentity {
		return ErrSourceChanged
	}
	return nil
}

func (s *sqliteSnapshotSource) Close() error {
	if s == nil {
		return nil
	}
	return errors.Join(s.connection.Close(), s.database.Close(), s.pathPin.Close())
}

func pinSQLiteSnapshotPath(path string, expected os.FileInfo) (*os.File, error) {
	// Keep an independently opened path handle until the SQLite connection
	// closes. In particular, Windows opens this handle without delete sharing,
	// so the name cannot be replaced in the interval before SQLite opens it.
	pin, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := pin.Stat()
	if err != nil {
		return nil, errors.Join(err, pin.Close())
	}
	if expected == nil || !info.Mode().IsRegular() ||
		!os.SameFile(expected, info) {
		return nil, errors.Join(ErrSourceChanged, pin.Close())
	}
	return pin, nil
}

func sqliteSnapshotMainPath(ctx context.Context, connection *sql.Conn) (string, error) {
	rows, err := connection.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return "", fmt.Errorf("rawcapture: inspect SQLite source path: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, path string
		if err := rows.Scan(&sequence, &name, &path); err != nil {
			return "", fmt.Errorf("rawcapture: inspect SQLite source path: %w", err)
		}
		if name == "main" && path != "" {
			return path, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("rawcapture: inspect SQLite source path: %w", err)
	}
	return "", ErrSourceChanged
}

func sameSQLiteSnapshotPath(actual, expected string) bool {
	actual, err := filepath.EvalSymlinks(actual)
	if err != nil {
		return false
	}
	expected, err = filepath.EvalSymlinks(expected)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(expected), filepath.Clean(actual))
	return err == nil && relative == "."
}

func sqliteOnlineBackupSize(ctx context.Context, source *sql.Conn) (int64, error) {
	var pages, pageBytes int64
	if err := source.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, fmt.Errorf("rawcapture: read SQLite page count: %w", err)
	}
	if err := source.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageBytes); err != nil {
		return 0, fmt.Errorf("rawcapture: read SQLite page size: %w", err)
	}
	if pages < 0 || pageBytes < 0 || (pages != 0 && pageBytes > math.MaxInt64/pages) {
		return math.MaxInt64, nil
	}
	return pages * pageBytes, nil
}

func runSQLiteBackup(
	ctx context.Context,
	pageBytes int64,
	maxBytes int64,
	step func() (bool, error),
	remaining func() int,
	pageCount func() int,
) error {
	previousRemaining := -1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := step()
		if err != nil {
			return err
		}
		if err := sqliteBackupSizeError(int64(pageCount()), pageBytes, maxBytes); err != nil {
			return err
		}
		if done {
			return nil
		}
		currentRemaining := remaining()
		if currentRemaining == previousRemaining {
			timer := time.NewTimer(sqliteBackupRetryDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		previousRemaining = currentRemaining
	}
}

func sqliteBackupSizeError(pages, pageBytes, maxBytes int64) error {
	if pages < 0 || pageBytes < 0 || pages != 0 && pageBytes > math.MaxInt64/pages ||
		pages*pageBytes > maxBytes {
		return rawcheckpoint.ErrOutboxFull
	}
	return nil
}

func sqliteSnapshotReservationBytes(snapshotBytes int64) int64 {
	metadata := rawcheckpoint.CaptureMetadataCharge(1, 1)
	if snapshotBytes > math.MaxInt64-metadata {
		return math.MaxInt64
	}
	return snapshotBytes + metadata
}
