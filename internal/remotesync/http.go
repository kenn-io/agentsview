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
	"strings"

	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type HTTPSync struct {
	Host                    string
	URL                     string
	Token                   string
	Full                    bool
	RepairMirror            bool
	DataDir                 string
	DB                      *db.DB
	BlockedResultCategories []string
	Progress                syncpkg.ProgressFunc
	Client                  *http.Client
}

func (hs HTTPSync) Run(ctx context.Context) (SyncStats, error) {
	client := hs.Client
	if client == nil {
		client = http.DefaultClient
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf("Resolving agent directories on %s", hs.Host),
	})
	targets, err := hs.fetchTargets(ctx, client)
	if err != nil {
		return SyncStats{}, err
	}
	if err := validateTargetSetPaths(targets); err != nil {
		return SyncStats{}, err
	}
	if hs.DataDir != "" {
		stats, handled, err := hs.tryMirrorSync(ctx, client, targets)
		if handled || err != nil {
			return stats, err
		}
		hs.report(syncpkg.Progress{
			Detail: fmt.Sprintf(
				"Remote %s does not support incremental sync; downloading full archive",
				hs.Host,
			),
		})
	}
	return hs.runLegacy(ctx, client, targets)
}

// tryMirrorSync runs the incremental mirror flow, reporting
// handled=false when the remote lacks manifest support and the caller
// should fall back to the legacy full-archive path. The mirror lock is
// acquired BEFORE the manifest fetch: concurrent syncs of the same
// host would otherwise fetch manifests in one order and apply them in
// another, and the stale manifest's deletion pass would remove files
// the newer sync just mirrored.
func (hs HTTPSync) tryMirrorSync(
	ctx context.Context, client *http.Client, targets TargetSet,
) (SyncStats, bool, error) {
	// The manifest covers only dir-scoped agents; file-scoped agents
	// (Windsurf) stream curated, sanitized exports the manifest cannot
	// model, so they are fetched as a separate small full archive on
	// every mirror sync instead of dragging the whole host onto the
	// full-archive path.
	dirScoped, fileScoped := targets.SplitFileScoped()
	mirrorRoot := MirrorDir(hs.DataDir, hs.Host)
	lock, err := AcquireMirrorLock(ctx, mirrorRoot)
	if err != nil {
		return SyncStats{}, true, err
	}
	defer func() { _ = lock.Close() }()
	manifest, supported, err := hs.fetchManifest(ctx, client, dirScoped)
	if err != nil {
		return SyncStats{}, true, err
	}
	if !supported {
		return SyncStats{}, false, nil
	}
	split := splitTargets{
		all:        targets,
		dirScoped:  dirScoped,
		fileScoped: fileScoped,
	}
	stats, err := hs.runMirror(ctx, client, split, manifest, mirrorRoot)
	return stats, true, err
}

// runLegacy is the pre-manifest flow: download the full tree into a
// throwaway temp dir and import it. It remains the path for old
// daemons (no manifest endpoint) and for callers without a DataDir.
func (hs HTTPSync) runLegacy(
	ctx context.Context, client *http.Client, targets TargetSet,
) (SyncStats, error) {
	tmpDir, err := hs.downloadAndExtract(ctx, client, targets)
	if err != nil {
		return SyncStats{}, err
	}
	defer os.RemoveAll(tmpDir)
	return hs.importRoot(ctx, targets, tmpDir)
}

func (hs HTTPSync) importRoot(
	ctx context.Context, targets TargetSet, root string,
) (SyncStats, error) {
	stats, err := Importer{
		Host:                    hs.Host,
		Full:                    hs.Full || hs.RepairMirror,
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

// splitTargets carries a target set alongside its SplitFileScoped
// partition so the mirror flow addresses each half without
// re-deriving it.
type splitTargets struct {
	all        TargetSet
	dirScoped  TargetSet
	fileScoped TargetSet
}

// runMirror syncs incrementally: diff the manifest against the
// persistent mirror, download only changed files, and import over the
// complete mirror tree so parser sibling reads keep working. The
// caller holds the mirror lock from before the manifest fetch through
// import, because extraction truncates files in place and the engine
// reads the mirror during SyncAll.
func (hs HTTPSync) runMirror(
	ctx context.Context,
	client *http.Client,
	targets splitTargets,
	manifest Manifest,
	mirrorRoot string,
) (SyncStats, error) {
	delta, err := MirrorDiff(mirrorRoot, manifest)
	if err != nil {
		return SyncStats{}, err
	}
	// Deletions run BEFORE extraction so a remote path that changed
	// type does not wedge the mirror: a stale file would block
	// creating a directory of the same name and vice versa.
	// ApplyMirrorDeletions prunes emptied directories for the same
	// reason. The pass also clears all file-scoped mirror content
	// (never in the manifest); the file-scoped archive re-populates it
	// below, so exports removed on the remote disappear locally too.
	if err := ApplyMirrorDeletions(mirrorRoot, delta.Deletions); err != nil {
		return SyncStats{}, err
	}
	// Directories stranded at fetch paths (crashed extraction) are
	// invisible to the deletion pass and would block writing the file.
	if err := RemoveMirrorTypeConflicts(mirrorRoot, delta.Fetch); err != nil {
		return SyncStats{}, err
	}
	if len(delta.Fetch) > 0 || hs.RepairMirror {
		// Bootstrap heuristic: past half the corpus a full archive is
		// cheaper than uploading a huge file list, and it doubles as
		// the empty-mirror bootstrap (fetch == total). Repair bypasses
		// the stat diff for stale or corrupt mirror bytes, which size and
		// mtime comparison cannot detect.
		full := hs.RepairMirror || len(delta.Fetch)*2 >= delta.Total
		err := hs.downloadIntoMirror(ctx, client, targets.dirScoped, delta.Fetch, full, mirrorRoot)
		var statusErr *StatusError
		if err != nil && !full && errors.As(err, &statusErr) {
			// Mid-rollout oddity: manifest worked but the delta
			// request was refused. Retry once as a full archive.
			err = hs.downloadIntoMirror(ctx, client, targets.dirScoped, delta.Fetch, true, mirrorRoot)
		}
		if err != nil {
			return SyncStats{}, err
		}
	}
	if !targets.fileScoped.IsEmpty() {
		// File-scoped exports (sanitized Windsurf state DBs) are
		// regenerated per archive and carry no stat identity the
		// manifest could diff, so they are re-fetched as a small full
		// archive on every sync. Extraction preserves the stamped
		// mtime, so downstream freshness checks still skip unchanged
		// sessions.
		err := hs.downloadIntoMirror(ctx, client, targets.fileScoped, nil, true, mirrorRoot)
		if err != nil {
			return SyncStats{}, err
		}
	}
	return hs.importRoot(ctx, targets.all, mirrorRoot)
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
) error {
	request := ArchiveRequest{TargetSet: targets}
	label := fmt.Sprintf("Downloading %d changed files from %s", len(fetch), hs.Host)
	if full {
		label = fmt.Sprintf("Downloading session archive from %s", hs.Host)
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError(resp)
	}
	hs.report(syncpkg.Progress{
		Detail:     label,
		BytesTotal: positiveContentLength(resp.ContentLength),
	})
	// Progress counts compressed wire bytes so totals stay meaningful.
	progress := &progressReader{
		r:     resp.Body,
		total: positiveContentLength(resp.ContentLength),
		report: func(done, total int64) {
			hs.report(syncpkg.Progress{
				Detail:     label,
				BytesDone:  done,
				BytesTotal: total,
			})
		},
	}
	stream := io.Reader(progress)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(progress)
		if err != nil {
			return fmt.Errorf("decode archive gzip: %w", err)
		}
		defer gz.Close()
		stream = gz
	}
	if err := os.MkdirAll(mirrorRoot, 0o755); err != nil {
		return fmt.Errorf("create mirror dir: %w", err)
	}
	hs.report(syncpkg.Progress{Detail: fmt.Sprintf("Extracting session archive from %s", hs.Host)})
	if _, err := ExtractTarStream(ctx, stream, mirrorRoot); err != nil {
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
) (string, error) {
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
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", httpStatusError(resp)
	}
	tmpDir, err := os.MkdirTemp("", "agentsview-http-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	archivePath := tmpDir + "/remote-sync.tar"
	archive, err := os.Create(archivePath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("create archive temp file: %w", err)
	}
	downloadLabel := fmt.Sprintf("Downloading session archive from %s", hs.Host)
	hs.report(syncpkg.Progress{
		Detail:     downloadLabel,
		BytesTotal: positiveContentLength(resp.ContentLength),
	})
	reader := &progressReader{
		r:     resp.Body,
		total: positiveContentLength(resp.ContentLength),
		report: func(done, total int64) {
			hs.report(syncpkg.Progress{
				Detail:     downloadLabel,
				BytesDone:  done,
				BytesTotal: total,
			})
		},
	}
	if _, err := io.Copy(archive, reader); err != nil {
		_ = archive.Close()
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("download archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("close archive temp file: %w", err)
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf("Extracting session archive from %s", hs.Host),
	})
	archive, err = os.Open(archivePath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("open archive temp file: %w", err)
	}
	if _, err := ExtractTarStream(ctx, archive, tmpDir); err != nil {
		_ = archive.Close()
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("extract tar: %w", err)
	}
	if err := archive.Close(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("close archive temp file: %w", err)
	}
	if err := os.Remove(archivePath); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("remove archive temp file: %w", err)
	}
	return tmpDir, nil
}

func (hs HTTPSync) report(progress syncpkg.Progress) {
	if hs.Progress != nil {
		hs.Progress(progress)
	}
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
