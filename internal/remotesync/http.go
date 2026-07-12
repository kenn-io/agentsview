package remotesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type HTTPSync struct {
	Host                    string
	URL                     string
	Token                   string
	Full                    bool
	DataDir                 string
	DB                      *db.DB
	BlockedResultCategories []string
	Progress                syncpkg.ProgressFunc
	Client                  *http.Client
	runPrepare              func(context.Context) (*PreparedHTTP, error)
	removeArchiveSpool      func(string) error
}

// PreparedCleanupError reports an operation failure while retaining a prepared
// HTTP source whose cleanup still needs to be retried. RetryCleanup is safe to
// call more than once. Error matching traverses the original operation and
// cleanup causes through Unwrap.
type PreparedCleanupError struct {
	mu       sync.Mutex
	cause    error
	prepared *PreparedHTTP
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

func (hs HTTPSync) Run(ctx context.Context) (stats SyncStats, err error) {
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

func (hs HTTPSync) importRoot(
	ctx context.Context, targets TargetSet, root string,
) (SyncStats, error) {
	stats, err := Importer{
		Host:                    hs.Host,
		Full:                    hs.Full,
		DB:                      hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress,
	}.ImportExtracted(ctx, targets, root)
	if err != nil {
		return SyncStats{}, err
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf(
			"Synced %d sessions from %s (%d unchanged)",
			stats.SessionsSynced, hs.Host, stats.Skipped,
		),
	})
	return stats, nil
}

func (hs HTTPSync) fetchManifest(
	ctx context.Context, client *http.Client, targets TargetSet,
) (Manifest, bool, error) {
	body, err := json.Marshal(targets)
	if err != nil {
		return Manifest{}, false, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, hs.endpoint("/api/v1/remote-sync/manifest"),
		bytes.NewReader(body),
	)
	if err != nil {
		return Manifest{}, false, err
	}
	hs.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		// Old daemon without the manifest endpoint; also gates delta
		// archive usage (an old server would ignore the files field
		// and return the full corpus).
		return Manifest{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Manifest{}, false, httpStatusError(resp)
	}
	// A real old daemon has no manifest route at all: the POST falls
	// through to the SPA catch-all, which serves index.html with a 200
	// and Content-Type text/html rather than a 404. Treat any 2xx
	// response that isn't JSON as manifest-unsupported instead of
	// failing the decode below. A malformed Content-Type is treated the
	// same way; only a genuine JSON response goes through the decoder,
	// so truncated/corrupt JSON (e.g. bad gzip) still surfaces as a hard
	// error rather than silently degrading to full syncs forever.
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Manifest{}, false, nil
	}
	reader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Manifest{}, false, fmt.Errorf("decode manifest gzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	var manifest Manifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return Manifest{}, false, fmt.Errorf("decode remote manifest: %w", err)
	}
	return manifest, true, nil
}

func (hs HTTPSync) downloadIntoMirror(
	ctx context.Context,
	client *http.Client,
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
		downloadLabel = fmt.Sprintf("Downloading session archive from %s", hs.Host)
		extractLabel = fmt.Sprintf("Extracting session archive from %s", hs.Host)
	} else {
		request.DeltaFiles = fetch
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, hs.endpoint("/api/v1/remote-sync/archive"),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	hs.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	archive, err := hs.downloadArchive(
		ctx, resp, downloadLabel, filepath.Dir(mirrorRoot),
	)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := archive.Close(); cleanupErr != nil {
			err = retainDownloadedArchiveCleanup(
				errors.Join(err, cleanupErr), archive, "",
			)
		}
	}()
	if err := archive.extract(ctx, mirrorRoot, hs.Progress, extractLabel); err != nil {
		return fmt.Errorf("extract archive into mirror: %w", err)
	}
	return nil
}

func (hs HTTPSync) fetchTargets(
	ctx context.Context,
	client *http.Client,
) (TargetSet, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, hs.endpoint("/api/v1/remote-sync/targets"), nil,
	)
	if err != nil {
		return TargetSet{}, err
	}
	hs.authorize(req)
	resp, err := client.Do(req)
	if err != nil {
		return TargetSet{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TargetSet{}, httpStatusError(resp)
	}
	var targets TargetSet
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return TargetSet{}, fmt.Errorf("decode remote targets: %w", err)
	}
	return targets, nil
}

func (hs HTTPSync) downloadAndExtract(
	ctx context.Context,
	client *http.Client,
	targets TargetSet,
) (root string, err error) {
	body, err := json.Marshal(targets)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, hs.endpoint("/api/v1/remote-sync/archive"), bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	hs.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	downloadLabel := fmt.Sprintf("Downloading session archive from %s", hs.Host)
	archive, err := hs.downloadArchive(ctx, resp, downloadLabel, os.TempDir())
	if err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp("", "agentsview-http-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		cleanupErr := archive.Close()
		if err == nil && cleanupErr == nil {
			return
		}
		rootErr := os.RemoveAll(tmpDir)
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
	extractLabel := fmt.Sprintf("Extracting session archive from %s", hs.Host)
	if err := archive.extract(ctx, tmpDir, hs.Progress, extractLabel); err != nil {
		return "", err
	}
	return tmpDir, nil
}

func (hs HTTPSync) report(progress syncpkg.Progress) {
	if hs.Progress != nil {
		hs.Progress(progress)
	}
}

func (hs HTTPSync) reportProgressDetail(detail string) {
	hs.report(syncpkg.Progress{Detail: detail})
}

func positiveContentLength(n int64) int64 {
	if n > 0 {
		return n
	}
	return 0
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

func (hs HTTPSync) endpoint(path string) string {
	return strings.TrimRight(hs.URL, "/") + path
}

func (hs HTTPSync) authorize(req *http.Request) {
	if hs.Token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+hs.Token)
}

// StatusError reports a non-2xx response from a remote daemon's
// remote-sync endpoints. Detail carries the (untrusted) response
// body for local logs; user-facing summaries should rely on Code.
type StatusError struct {
	Code   int
	Status string
	Detail string
}

func (e *StatusError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Status
	}
	return fmt.Sprintf("remote sync %s: %s", e.Status, msg)
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &StatusError{
		Code:   resp.StatusCode,
		Status: resp.Status,
		Detail: strings.TrimSpace(string(body)),
	}
}
