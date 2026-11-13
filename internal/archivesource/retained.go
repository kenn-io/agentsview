package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

type codexReadCallback func(string, []string, []string, func(context.Context, string, int64) ([]byte, error)) error

// HasRetainedSession is a routing hint; Retained rechecks the binding inside its transaction.
func HasRetainedSession(ctx context.Context, cfg config.Config, database *db.DB, session *db.Session, provider, native string) (bool, error) {
	if database == nil {
		return false, nil
	}
	var found bool
	err := database.Reader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM raw_archive_sessions WHERE session_id=?)`, session.ID).Scan(&found)
	if err != nil || found {
		return found, err
	}
	var stored string
	if session.FilePath != nil {
		stored = *session.FilePath
	}
	_, found, err = unboundRetainedSource(ctx, database.Reader(), cfg.InstallationID, session.Machine, provider, native, stored)
	return found, err
}

// Retained reconstructs a local retained source or a configured directory attachment.
// The reader and transaction are closed before the independent scratch tree is returned.
func Retained(ctx context.Context, cfg config.Config, database *db.DB, sessionID, device, native, provider, stored string, owners []db.ArchiveSessionProvenance, limit int64, use codexReadCallback) (tree *rawderive.Materialization, primary, source string, retErr error) {
	defer func() {
		if retErr != nil && tree != nil {
			retErr = errors.Join(retErr, tree.Cleanup())
			tree = nil
		}
	}()
	var reader *docbank.ContentReader
	var tx *sql.Tx
	var connection *sql.DB
	var lock *flock.Flock
	var owner db.ArchiveSessionProvenance
	var archiveID string
	var err error
	if len(owners) != 0 {
		reader, connection, tx, lock, owner, archiveID, err = openRetainedDirectory(ctx, cfg.ArchiveSources, owners)
		if err != nil {
			return nil, "", "", err
		}
		source = "directory:" + archiveID
	} else {
		if database == nil {
			return nil, "", "", errors.New("retained recovery requires the local archive database")
		}
		vault := filepath.Join(cfg.DataDir, rawarchive.Directory, "artifacts")
		reader, err = docbank.OpenContentReader(ctx, docbank.ContentReaderConfig{Root: vault})
		if err != nil {
			return nil, "", "", vaultReadError(err)
		}
		tx, err = database.BeginReadTx(ctx)
		if err != nil {
			return nil, "", "", errors.Join(err, reader.Close())
		}
	}
	defer func() {
		if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, err)
		}
		if connection != nil {
			retErr = errors.Join(retErr, connection.Close())
		}
		retErr = errors.Join(retErr, reader.Close())
		if lock != nil {
			retErr = errors.Join(retErr, lock.Close())
		}
	}()
	if len(owners) == 0 {
		archiveID, err = checkSavedSchema(ctx, tx)
		if err != nil {
			return nil, "", "", err
		}
		owner.Source.ArchiveID = archiveID
		owner.NativeSessionID = native
		err = tx.QueryRowContext(ctx, `SELECT r.device_id,r.id,s.source_key,s.manifest_id
			FROM raw_archive_sessions b JOIN raw_archive_roots r ON r.id=b.root_id
			JOIN raw_archive_sources s ON s.root_id=b.root_id AND s.source_key=b.source_key
			WHERE b.session_id=? AND b.device_id=? AND b.provider=? AND b.parser_id=? AND r.device_id=b.device_id AND r.provider=b.provider`, sessionID, device, provider, native).Scan(&owner.Source.DeviceID, &owner.Source.RootID, &owner.Source.SourceKey, &owner.ManifestID)
		if errors.Is(err, sql.ErrNoRows) {
			var bound bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM raw_archive_sessions WHERE session_id=?)`, sessionID).Scan(&bound)
			if err != nil {
				return nil, "", "", err
			}
			if !bound {
				var found bool
				owner, found, err = unboundRetainedSource(ctx, tx, cfg.InstallationID, device, provider, native, stored)
				if err != nil {
					return nil, "", "", err
				}
				if found {
					owner.Source.ArchiveID = archiveID
				} else {
					return nil, "", "", errors.New("original transcript or owning source unavailable")
				}
			} else {
				return nil, "", "", errors.New("original transcript or owning source unavailable")
			}
		}
		if err != nil {
			return nil, "", "", fmt.Errorf("retained session ownership: %w", err)
		}
		source = "retained:" + archiveID
	}
	if len(owners) != 0 && (owner.NativeSessionID != native || owner.Source.DeviceID != device) {
		return nil, "", "", errors.New("archive session identity differs")
	}
	if rest, ok := strings.CutPrefix(stored, "archive://"); ok {
		root, encoded, found := strings.Cut(rest, "/")
		key, err := url.PathUnescape(encoded)
		if !found || err != nil || root != owner.Source.RootID || key != owner.Source.SourceKey {
			return nil, "", "", errors.New("archive source binding differs")
		}
	}
	objects := rawsync.NewArtifactObjectReader(artifact.ContentReaderOpener{Reader: reader})
	return materializeCatalog(ctx, tx, archiveID, owner, provider, limit, use, objects, nil, source)
}

// openRetainedDirectory checks each configured directory under its own lease.
func openRetainedDirectory(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance) (*docbank.ContentReader, *sql.DB, *sql.Tx, *flock.Flock, db.ArchiveSessionProvenance, string, error) {
	var candidateErr error
	for _, candidate := range sources {
		if candidate.Directory == "" {
			continue
		}
		reader, connection, tx, lock, archiveID, err := openDirectoryCandidate(ctx, candidate.Directory)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, nil, nil, db.ArchiveSessionProvenance{}, "", errors.Join(ctx.Err(), err)
			}
			candidateErr = errors.Join(candidateErr, fmt.Errorf("directory %s: %w", candidate.Directory, err))
			continue
		}
		for _, owner := range owners {
			if owner.Attachment.RepositoryID == "archive:"+archiveID && owner.Attachment.SnapshotID == "directory:"+archiveID {
				return reader, connection, tx, lock, owner, archiveID, nil
			}
		}
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		if err := errors.Join(ctx.Err(), rollbackErr, connection.Close(), reader.Close(), lock.Close()); err != nil {
			return nil, nil, nil, nil, db.ArchiveSessionProvenance{}, "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, db.ArchiveSessionProvenance{}, "", err
	}
	return nil, nil, nil, nil, db.ArchiveSessionProvenance{}, "", errors.Join(errors.New("owning directory is not configured or unavailable"), candidateErr)
}

func openDirectoryCandidate(ctx context.Context, directory string) (_ *docbank.ContentReader, _ *sql.DB, _ *sql.Tx, _ *flock.Flock, _ string, retErr error) {
	application, vault, err := directoryLayout(directory)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	lockPath := filepath.Join(application, "db.write.lock")
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil, nil, nil, nil, "", errors.New("archive directory requires an existing regular db.write.lock; attach or start its owner, then retry")
	}
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	lock := flock.New(lockPath, flock.SetFlag(os.O_RDONLY))
	locked, err := lock.TryRLock()
	if err != nil || !locked {
		if err == nil {
			err = errors.New("archive is busy; stop its owner and retry")
		}
		return nil, nil, nil, nil, "", errors.Join(err, lock.Close())
	}
	var reader *docbank.ContentReader
	var connection *sql.DB
	var tx *sql.Tx
	defer func() {
		if retErr == nil {
			return
		}
		if tx != nil {
			rollbackErr := tx.Rollback()
			if !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
		if connection != nil {
			retErr = errors.Join(retErr, connection.Close())
		}
		if reader != nil {
			retErr = errors.Join(retErr, reader.Close())
		}
		retErr = errors.Join(retErr, lock.Close())
	}()
	reader, err = docbank.OpenContentReader(ctx, docbank.ContentReaderConfig{Root: vault})
	if err != nil {
		return nil, nil, nil, nil, "", vaultReadError(err)
	}
	saved := filepath.Join(application, "sessions.db")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, statErr := os.Lstat(saved + suffix); statErr == nil {
			return nil, nil, nil, nil, "", errors.New("close the archive writer and retry")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, nil, nil, nil, "", statErr
		}
	}
	pending, err := db.CompactRecoveryPending(saved)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	if pending {
		return nil, nil, nil, nil, "", errors.New("archive compaction recovery is pending; start the owner and retry")
	}
	connection, err = sql.Open("sqlite3", immutableURI(saved))
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	tx, err = connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	archiveID, err := checkSavedSchema(ctx, tx)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	return reader, connection, tx, lock, archiveID, nil
}

func vaultReadError(err error) error {
	if errors.Is(err, docbank.ErrVaultLocked) {
		return fmt.Errorf("archive is busy; stop its owner and retry: %w", err)
	}
	return err
}
