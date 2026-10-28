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
	"strings"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
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
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", errSourceUnavailable, err)
		}
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
	scratch, release, err := newBootstrapScratch(cache)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	// The archive owns attachment identity. Catalog changes register new sources
	// under the same attachment instead of reparsing completed ones.
	info := docbank.BackupReaderInfo{RepositoryID: "archive:" + archiveID, SnapshotID: "directory:" + archiveID}
	output := filepath.Join(scratch, "index.db")
	if err := writeCompactIndex(ctx, uri, output, info); err != nil {
		return nil, err
	}
	path, catalog, err := publishDirectoryIndex(ctx, output, cache, archiveID, func() error {
		return validateSavedDatabase(ctx, uri, func(ctx context.Context, tenant string, ref rawsync.ObjectRef) error {
			_, err := objects.StatObject(ctx, tenant, ref)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	index, err := openCompactIndex(ctx, path, info)
	if err != nil {
		return nil, err
	}
	index.objects, index.closeSource, index.catalog = objects, closeSource, catalog
	return index, nil
}

// Hash only the compact acceptance catalog, never the multi-GiB transcript DB.
// A catalog already published under this hash was validated when it was
// written, so unchanged passes skip checking every stored object. Older
// catalogs of the same archive are removed once the new one is in place.
func publishDirectoryIndex(ctx context.Context, output, cache, archiveID string, validate func() error) (string, string, error) {
	file, err := os.Open(output)
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, ctxio.Reader{Context: ctx, Reader: file})
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", "", err
	}
	catalog := hex.EncodeToString(hash.Sum(nil))
	prefix := fmt.Sprintf("directory-%x-", sha256.Sum256([]byte(archiveID)))
	path := filepath.Join(cache, prefix+catalog+".db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := validate(); err != nil {
			return "", "", err
		}
		if err := publishIndex(output, path); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return "", "", err
	}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, prefix) && name != filepath.Base(path) {
			// Best effort: another process may still hold an old catalog open.
			_ = os.Remove(filepath.Join(cache, name))
		}
	}
	return path, catalog, nil
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
