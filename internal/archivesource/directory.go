package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/kit/atomicfile"
)

var errDirectoryBusy = errors.New("archive directory is in use; stop its AgentsView process before attaching it")

// A directory source holds both the application writer lock and Docbank owner
// until Close. Its acceptance records cannot change during copying or parsing.
func openDirectoryIndex(ctx context.Context, source config.ArchiveSource, cache string) (_ *Index, retErr error) {
	application, vault, err := directoryLayout(source.Directory)
	if err != nil {
		return nil, err
	}
	saved := filepath.Join(application, "sessions.db")
	for _, path := range []string{saved, filepath.Join(vault, "docbank.db")} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("archive directory requires sessions.db and its raw Docbank vault")
		}
	}
	lock := flock.New(filepath.Join(application, "db.write.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.Join(errDirectoryBusy, lock.Close())
	}
	var repository *artifact.Repository
	closeSource := func() error {
		var err error
		if repository != nil {
			err = repository.Close()
		}
		return errors.Join(err, lock.Close())
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, closeSource())
		}
	}()
	uri := "file:" + (&url.URL{Path: saved}).EscapedPath() + "?mode=ro"
	database, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, err
	}
	archiveID, schemaErr := checkSavedSchema(ctx, database)
	if err := errors.Join(schemaErr, database.Close()); err != nil {
		return nil, err
	}
	repository, err = artifact.OpenExistingRepository(ctx, vault)
	if err != nil {
		return nil, err
	}
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	if err != nil {
		return nil, err
	}
	if err := prepareCache(cache); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(cache, ".bootstrap-")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	info := docbank.BackupReaderInfo{RepositoryID: "archive:" + archiveID, SnapshotID: "directory"}
	output := filepath.Join(scratch, "index.db")
	if err := buildCompactIndex(ctx, uri, output, info, func(ctx context.Context, tenant string, ref rawsync.ObjectRef) error {
		_, err := objects.StatObject(ctx, tenant, ref)
		return err
	}); err != nil {
		return nil, err
	}
	path, info, err := publishDirectoryIndex(ctx, output, cache, info)
	if err != nil {
		return nil, err
	}
	index, err := openCompactIndex(ctx, path, info)
	if err != nil {
		return nil, err
	}
	index.objects, index.closeSource = objects, closeSource
	return index, nil
}

// Hash only the compact acceptance catalog, never the multi-GiB transcript DB.
// Including its identity makes the same directory movable without merging it
// with a different archive that happens to contain the same provider files.
func publishDirectoryIndex(ctx context.Context, output, cache string, info docbank.BackupReaderInfo) (string, docbank.BackupReaderInfo, error) {
	file, err := os.Open(output)
	if err != nil {
		return "", info, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, ctxio.Reader{Context: ctx, Reader: file})
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", info, err
	}
	info.SnapshotID = "directory:" + hex.EncodeToString(hash.Sum(nil))
	path := filepath.Join(cache, fmt.Sprintf("directory-%x.db", hash.Sum(nil)))
	if _, err := os.Stat(path); err == nil {
		return path, info, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", info, err
	}
	index, err := sql.Open("sqlite3", output)
	if err != nil {
		return "", info, err
	}
	_, updateErr := index.ExecContext(ctx, `UPDATE metadata SET snapshot_id=?`, info.SnapshotID)
	if err := errors.Join(updateErr, index.Close()); err != nil {
		return "", info, err
	}
	if err := atomicfile.RenameNoReplace(output, path); err != nil {
		return "", info, err
	}
	return path, info, atomicfile.SyncDir(cache)
}

// Both restore commands retain the acceptance database beside the raw vault.
func directoryLayout(directory string) (application, vault string, err error) {
	if _, err := os.Stat(filepath.Join(directory, "sessions.db")); err == nil {
		return directory, filepath.Join(directory, rawarchive.Directory, "artifacts"), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	return filepath.Join(directory, "application"), directory, nil
}
