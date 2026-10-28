// Package archivesource reads retained provider history without restoring a vault.
package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/kit/atomicfile"
)

const indexVersion = 1

// Only missing initial layout files mean an attachment is unavailable. Missing
// snapshots or content after opening the repository are errors in that archive.
var errSourceUnavailable = errors.New("archive source layout unavailable")

// cacheMu serializes this process's bootstraps, which can run outside the sync
// lock, so they do not copy the same snapshot twice. Scratch locks protect
// trees across processes.
var cacheMu sync.Mutex

// Index is disposable source metadata. Session ownership lives in the receiver.
type Index struct {
	Info        docbank.BackupReaderInfo
	ArchiveID   string
	database    *sql.DB
	objects     rawderive.ObjectStore
	closeSource func() error
	machineName string
	// catalog fingerprints a directory's accepted sources; backups are fixed.
	catalog string
}

type Source struct {
	db.RawArchiveSource
	Root db.RawArchiveRoot
}

type Coverage struct {
	SupportedSources  int64
	SupplementalFiles int64
	DeletionRules     int64
}

// OpenIndex requires the caller to own the receiving data directory. cacheDir
// belongs exclusively to this feature; abandoned bootstrap trees are removed.
func OpenIndex(ctx context.Context, source config.ArchiveSource, cacheDir string, progress func(string)) (index *Index, retErr error) {
	defer func() {
		if index != nil {
			index.machineName = source.MachineName
		}
	}()
	if err := (config.Config{ArchiveSources: []config.ArchiveSource{source}}).ValidateArchiveSources(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if source.Directory != "" {
		return openDirectoryIndex(ctx, source, cacheDir)
	}
	// Reopen the repository too: its constructor caches the repository UUID.
	repository, err := docbank.OpenBackupRepository(source.Repository)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errSourceUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	reader, err := repository.OpenSnapshot(ctx, source.Snapshot)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	info := reader.Info()
	if err := prepareCache(cacheDir); err != nil {
		return nil, err
	}
	key := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s", indexVersion, info.RepositoryID, info.SnapshotID))
	path := filepath.Join(cacheDir, fmt.Sprintf("%x.db", key))
	if _, err := os.Stat(path); err == nil {
		if err := reader.Close(); err != nil {
			return nil, err
		}
		return openCompactIndex(ctx, path, info)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := bootstrapIndex(ctx, reader, path, progress); err != nil {
		return nil, fmt.Errorf("indexing archive source %q: %w", source.Name, err)
	}
	return openCompactIndex(ctx, path, info)
}

func prepareCache(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return withCacheLock(directory, func() error { return removeAbandonedBootstraps(directory) })
}

// withCacheLock serializes cleanup with scratch creation in every process. A
// new tree is visible before its owner locks it, so cleanup must not run then.
// The lock sits beside the cache so the cache holds only indexes and scratch.
func withCacheLock(cache string, work func() error) error {
	lock := flock.New(filepath.Clean(cache) + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	err := work()
	return errors.Join(err, lock.Close())
}

func removeAbandonedBootstraps(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".bootstrap-") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		lock := flock.New(filepath.Join(path, bootstrapLockFile))
		locked, err := lock.TryLock()
		if errors.Is(err, fs.ErrNotExist) {
			continue // Its owner removed it meanwhile.
		}
		if err != nil {
			return err
		}
		if !locked {
			continue // Another bootstrap, possibly in a rebuild worker, owns it.
		}
		if err := errors.Join(lock.Close(), os.RemoveAll(path)); err != nil {
			return fmt.Errorf("removing abandoned archive bootstrap: %w", err)
		}
	}
	return nil
}

const bootstrapLockFile = ".active"

// Test seam: invoked after a scratch tree exists and before its owner locks it.
var bootstrapTestHookBeforeLock func(scratch string)

// newBootstrapScratch holds a lock on its tree until release, so cache cleanup
// in any process removes only trees whose bootstrap ended.
func newBootstrapScratch(cache string) (string, func() error, error) {
	var scratch string
	var lock *flock.Flock
	err := withCacheLock(cache, func() error {
		var err error
		if scratch, err = os.MkdirTemp(cache, ".bootstrap-"); err != nil {
			return err
		}
		if bootstrapTestHookBeforeLock != nil {
			bootstrapTestHookBeforeLock(scratch)
		}
		lock = flock.New(filepath.Join(scratch, bootstrapLockFile))
		locked, err := lock.TryLock()
		if err == nil && !locked {
			err = errors.New("archive bootstrap scratch is already in use")
		}
		if err != nil {
			return errors.Join(err, lock.Close(), os.RemoveAll(scratch))
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	// Unlock first: Windows cannot remove a directory holding an open file.
	return scratch, func() error { return errors.Join(lock.Close(), os.RemoveAll(scratch)) }, nil
}

// publishIndex installs a finished index. Another process may have published
// the same snapshot or catalog first; its complete copy is equivalent.
func publishIndex(output, target string) error {
	if err := atomicfile.RenameNoReplace(output, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			return nil
		}
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(target))
}

func immutableURI(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&immutable=1"
}

func openCompactIndex(ctx context.Context, path string, info docbank.BackupReaderInfo) (_ *Index, retErr error) {
	conn, err := sql.Open("sqlite3", immutableURI(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, conn.Close())
		}
	}()
	var version int
	var archiveID, repositoryID, snapshotID string
	err = conn.QueryRowContext(ctx, `SELECT version,archive_id,repository_id,snapshot_id FROM metadata`).
		Scan(&version, &archiveID, &repositoryID, &snapshotID)
	if err != nil {
		return nil, err
	}
	if version != indexVersion || repositoryID != info.RepositoryID || snapshotID != info.SnapshotID || archiveID == "" {
		return nil, errors.New("archive source index identity or version differs")
	}
	return &Index{Info: info, ArchiveID: archiveID, database: conn}, nil
}

func (i *Index) Close() error {
	err := i.database.Close()
	if i.closeSource != nil {
		err = errors.Join(err, i.closeSource())
		i.closeSource = nil
	}
	return err
}

func (i *Index) Sources(ctx context.Context, after string, limit int) ([]Source, error) {
	if limit <= 0 || limit > 10000 {
		return nil, errors.New("invalid archive source page size")
	}
	rows, err := i.database.QueryContext(ctx, `SELECT s.manifest_id,s.root_id,s.source_key,s.original_path,s.canonical_json,
		r.id,r.device_id,r.machine,r.provider,r.original_path,r.configured_root_id
		FROM sources s JOIN roots r ON r.id=s.root_id WHERE s.manifest_id>? ORDER BY s.manifest_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ManifestID, &s.RootID, &s.SourceKey, &s.OriginalPath, &s.CanonicalJSON,
			&s.Root.ID, &s.Root.DeviceID, &s.Root.Machine, &s.Root.Provider, &s.Root.OriginalPath, &s.Root.ConfiguredRootID); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (i *Index) Coverage(ctx context.Context) (Coverage, error) {
	var result Coverage
	err := i.database.QueryRowContext(ctx, `SELECT supported_sources,supplemental_files,(SELECT coalesce(sum(json_array_length(CAST(suppressions AS TEXT))),0) FROM devices) FROM coverage`).
		Scan(&result.SupportedSources, &result.SupplementalFiles, &result.DeletionRules)
	return result, err
}

func (i *Index) Suppressions(ctx context.Context, deviceID string) ([]db.RawArchiveSuppression, error) {
	var data []byte
	if err := i.database.QueryRowContext(ctx, `SELECT suppressions FROM devices WHERE device_id=?`, deviceID).Scan(&data); err != nil {
		return nil, err
	}
	var result []db.RawArchiveSuppression
	err := json.Unmarshal(data, &result)
	return result, err
}

func bootstrapIndex(ctx context.Context, reader *docbank.BackupReader, target string, progress func(string)) (retErr error) {
	scratch, release, err := newBootstrapScratch(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	if err := readRecoveryMetadata(ctx, reader); err != nil {
		return err
	}
	object, err := reader.Extra("application/sessions.db")
	if err != nil {
		return err
	}
	if progress != nil {
		progress(fmt.Sprintf("Reading archive source index: %d temporary bytes required", object.Bytes))
	}
	saved := filepath.Join(scratch, "sessions.db")
	if err := copyDatabase(ctx, reader, saved, object.Bytes, progress); err != nil {
		return err
	}
	// SQLite inspection and index construction must not hold the repository lease.
	if err := reader.Close(); err != nil {
		return err
	}
	output := filepath.Join(scratch, "index.db")
	if err := buildCompactIndex(ctx, immutableURI(saved), output, reader.Info(), snapshotObjects{reader}.checkObject); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishIndex(output, target)
}

func copyDatabase(ctx context.Context, reader *docbank.BackupReader, path string, size int64, progress func(string)) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := &copyProgress{file: file, total: size, report: progress}
	_, copyErr := reader.CopyExtra(ctx, "application/sessions.db", writer)
	return errors.Join(copyErr, file.Close())
}

type copyProgress struct {
	file          *os.File
	total, copied int64
	next          int64
	report        func(string)
}

func (w *copyProgress) Write(data []byte) (int, error) {
	n, err := w.file.Write(data)
	w.copied += int64(n)
	if w.report != nil && (w.copied >= w.next || w.copied == w.total) {
		w.report(fmt.Sprintf("Reading archive source index: %d of %d bytes", w.copied, w.total))
		w.next = w.copied + 64<<20
	}
	return n, err
}
