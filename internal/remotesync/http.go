package remotesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// RemoteSyncLifecycle observes the preparation and atomic rebuild work for one
// remote host. Callbacks run synchronously around the operation they describe.
type RemoteSyncLifecycle struct {
	PrepareStarted  func()
	PrepareFinished func(error)
	RebuildStarted  func()
	RebuildFinished func(syncpkg.SyncStats, error)
}

type RemoteSync struct {
	Host string
	// Source fetches the remote half of the sync. Required.
	Source                  Source
	Full                    bool
	FullReason              FullImportReason
	DataDir                 string
	DB                      *db.DB
	BlockedResultCategories []string
	Progress                syncpkg.ProgressFunc
	Lifecycle               *RemoteSyncLifecycle
	runPrepare              func(context.Context) (*PreparedRemote, error)
	removeArchiveSpool      func(string) error
	acquireMirrorLock       func(context.Context, string) (*MirrorLockHandle, error)
}

// forceParseRequested distinguishes an operator-requested full sync from a
// complete source scan required by an automatic data rebuild. A blank reason
// retains the historical meaning of Full for direct callers.
func (hs RemoteSync) forceParseRequested() bool {
	return hs.Full && (hs.FullReason == "" || hs.FullReason == FullImportExplicit)
}

func (hs RemoteSync) forceFullParseAfterCacheRequested() bool {
	return hs.Full && !hs.forceParseRequested()
}

// PreparedCleanupError reports an operation failure while retaining a prepared
// HTTP source whose cleanup still needs to be retried. RetryCleanup is safe to
// call more than once. Error matching traverses the original operation and
// cleanup causes through Unwrap.
type PreparedCleanupError struct {
	mu       sync.Mutex
	cause    error
	prepared *PreparedRemote
}

func (e *PreparedCleanupError) Error() string { return e.cause.Error() }

func (e *PreparedCleanupError) Unwrap() error { return e.cause }

// RetryCleanup retries release of the source retained by this error.
func (e *PreparedCleanupError) RetryCleanup() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.prepared == nil {
		return nil
	}
	if err := e.prepared.Close(); err != nil {
		return err
	}
	e.prepared = nil
	return nil
}

func (hs RemoteSync) Run(ctx context.Context) (stats SyncStats, err error) {
	prepare := hs.Prepare
	if hs.runPrepare != nil {
		prepare = hs.runPrepare
	}
	prepared, prepareErr := prepare(ctx)
	if prepareErr != nil {
		if prepared == nil {
			return SyncStats{}, prepareErr
		}
		cleanupErr := prepared.Close()
		if cleanupErr == nil {
			return SyncStats{}, prepareErr
		}
		return SyncStats{}, &PreparedCleanupError{
			cause: errors.Join(prepareErr, cleanupErr), prepared: prepared,
		}
	}
	defer func() {
		if cleanupErr := prepared.Close(); cleanupErr != nil {
			err = &PreparedCleanupError{
				cause: errors.Join(err, cleanupErr), prepared: prepared,
			}
		}
	}()
	return prepared.ImportActive(ctx)
}

func (hs RemoteSync) importRoot(
	ctx context.Context, targets TargetSet, root string,
) (SyncStats, error) {
	stats, err := Importer{
		Host:                    hs.Host,
		Full:                    hs.Full,
		RequireComplete:         true,
		completeSourceMirror:    true,
		DB:                      hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress,
	}.ImportExtracted(ctx, targets, root)
	stats.FullReason = hs.FullReason
	if stats.FullReason == "" {
		stats.FullReason = FullImportLegacy
	}
	if err != nil {
		return stats, err
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf(
			"Synced %d sessions from %s (%d unchanged)",
			stats.SessionsSynced, hs.Host, stats.Skipped,
		),
	})
	return stats, nil
}

func (hs RemoteSync) downloadIntoMirror(
	ctx context.Context,
	targets TargetSet,
	fetch []string,
	full bool,
	mirrorRoot string,
) (err error) {
	request := ArchiveRequest{TargetSet: targets}
	downloadLabel := fmt.Sprintf(
		"Downloading %d changed files from %s", len(fetch), hs.Host,
	)
	extractLabel := fmt.Sprintf(
		"Extracting %d changed files from %s", len(fetch), hs.Host,
	)
	if full {
		downloadLabel = "Downloading session archive from " + hs.Host
		extractLabel = "Extracting session archive from " + hs.Host
	} else {
		request.DeltaFiles = fetch
	}
	downloadStarted := time.Now()
	stream, err := hs.Source.Archive(ctx, request)
	if err != nil {
		return err
	}
	archive, err := hs.downloadArchive(
		ctx, stream, downloadLabel, filepath.Dir(mirrorRoot),
	)
	if err != nil {
		return err
	}
	downloadDuration := time.Since(downloadStarted)
	defer func() {
		if cleanupErr := archive.Close(); cleanupErr != nil {
			err = retainDownloadedArchiveCleanup(
				errors.Join(err, cleanupErr), archive, "",
			)
		}
	}()
	if err := RemoveMirrorFetchFiles(mirrorRoot, fetch); err != nil {
		return err
	}
	extractStarted := time.Now()
	var extractErr error
	if full && fetch != nil {
		selected := make(map[string]struct{}, len(fetch))
		for _, remotePath := range fetch {
			name, nameErr := safeRemotePathArchiveName(remotePath)
			if nameErr != nil {
				return nameErr
			}
			selected[filepath.ToSlash(filepath.Clean(name))] = struct{}{}
		}
		extractErr = archive.extractSelected(
			ctx, mirrorRoot, selected, hs.Progress, extractLabel,
		)
	} else {
		extractErr = archive.extract(ctx, mirrorRoot, hs.Progress, extractLabel)
	}
	hs.logArchiveTransfer(archive, downloadDuration, extractStarted)
	if extractErr != nil {
		return fmt.Errorf("extract archive into mirror: %w", extractErr)
	}
	return nil
}

func (hs RemoteSync) downloadAndExtract(
	ctx context.Context,
	targets TargetSet,
) (root string, err error) {
	downloadStarted := time.Now()
	stream, err := hs.Source.Archive(ctx, ArchiveRequest{TargetSet: targets})
	if err != nil {
		return "", err
	}
	downloadLabel := "Downloading session archive from " + hs.Host
	archive, err := hs.downloadArchive(ctx, stream, downloadLabel, os.TempDir())
	if err != nil {
		return "", err
	}
	downloadDuration := time.Since(downloadStarted)
	var tmpDir string
	defer func() {
		cleanupErr := archive.Close()
		if err == nil && cleanupErr == nil {
			return
		}
		var rootErr error
		if tmpDir != "" {
			rootErr = os.RemoveAll(tmpDir)
		}
		if cleanupErr != nil || rootErr != nil {
			var retainedArchive *downloadedArchive
			if cleanupErr != nil {
				retainedArchive = archive
			}
			var retainedRoot string
			if rootErr != nil {
				retainedRoot = tmpDir
			}
			err = retainDownloadedArchiveCleanup(
				errors.Join(err, cleanupErr, rootErr),
				retainedArchive, retainedRoot,
			)
		}
		root = ""
	}()
	tmpDir, err = os.MkdirTemp("", "agentsview-http-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	extractLabel := "Extracting session archive from " + hs.Host
	extractStarted := time.Now()
	extractErr := archive.extract(ctx, tmpDir, hs.Progress, extractLabel)
	hs.logArchiveTransfer(archive, downloadDuration, extractStarted)
	if extractErr != nil {
		return "", extractErr
	}
	return tmpDir, nil
}

// logArchiveTransfer records how one archive's download and extraction
// split their time, so slow syncs show which side to look at.
func (hs RemoteSync) logArchiveTransfer(
	archive *downloadedArchive, download time.Duration, extractStarted time.Time,
) {
	log.Printf("remote sync archive: host=%s encoding=%q wire_bytes=%d "+
		"download_duration=%s extract_duration=%s",
		hs.Host, archive.encoding, archive.bytes, download.Round(time.Millisecond),
		time.Since(extractStarted).Round(time.Millisecond))
}

func (hs RemoteSync) report(progress syncpkg.Progress) {
	if hs.Progress != nil {
		hs.Progress(progress)
	}
}

func (hs RemoteSync) reportProgressDetail(detail string) {
	hs.report(syncpkg.Progress{Detail: detail})
}

type progressReader struct {
	r      io.Reader
	done   int64
	total  int64
	report func(done, total int64)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.done += int64(n)
		r.report(r.done, r.total)
	}
	return n, err
}
