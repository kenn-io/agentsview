package remotesync

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// downloadedArchive owns an isolated directory containing a raw HTTP
// response-body spool until Close. Keeping the wire representation separate
// from extraction makes transfer progress independent from tar processing and
// preserves meaningful Content-Length totals for compressed responses.
type downloadedArchive struct {
	dir        string
	path       string
	compressed bool
	open       func() (io.ReadCloser, error)
	remove     func(string) error
}

// downloadedArchiveCleanupError retains a spool whose removal failed so the
// process-level cleanup registry can retry it before another HTTP sync starts.
type downloadedArchiveCleanupError struct {
	mu      sync.Mutex
	cause   error
	archive *downloadedArchive
	root    string
	remove  func(string) error
}

func (e *downloadedArchiveCleanupError) Error() string { return e.cause.Error() }
func (e *downloadedArchiveCleanupError) Unwrap() error { return e.cause }
func (e *downloadedArchiveCleanupError) RetryCleanup() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var cleanupErr error
	if e.archive != nil {
		if err := e.archive.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			e.archive = nil
		}
	}
	if e.root != "" {
		remove := e.remove
		if remove == nil {
			remove = os.RemoveAll
		}
		if err := remove(e.root); err != nil {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("remove extracted archive root: %w", err))
		} else {
			e.root = ""
		}
	}
	return cleanupErr
}

func retainDownloadedArchiveCleanup(
	cause error, archive *downloadedArchive, root string,
) error {
	return &downloadedArchiveCleanupError{
		cause: cause, archive: archive, root: root,
	}
}

func (hs HTTPSync) downloadArchive(
	ctx context.Context,
	resp *http.Response,
	label string,
	spoolDir string,
) (result *downloadedArchive, err error) {
	var owned *downloadedArchive
	defer func() {
		err = errors.Join(err, resp.Body.Close())
		if err != nil && owned != nil {
			if cleanupErr := owned.Close(); cleanupErr != nil {
				err = &downloadedArchiveCleanupError{
					cause: errors.Join(err, cleanupErr), archive: owned,
				}
			}
			result = nil
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpStatusError(resp)
	}
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("create archive spool dir: %w", err)
	}
	tempDir, err := os.MkdirTemp(spoolDir, "agentsview-http-archive-*")
	if err != nil {
		return nil, fmt.Errorf("create archive spool dir: %w", err)
	}
	owned = &downloadedArchive{
		dir:        tempDir,
		path:       filepath.Join(tempDir, "archive"),
		compressed: resp.Header.Get("Content-Encoding") == "gzip",
		remove:     hs.removeArchiveSpool,
	}
	spool, err := os.Create(owned.path)
	if err != nil {
		return nil, fmt.Errorf("create archive spool: %w", err)
	}

	total := positiveContentLength(resp.ContentLength)
	hs.report(syncpkg.Progress{Detail: label, BytesTotal: total})
	reader := &progressReader{
		r:     &contextReader{ctx: ctx, r: resp.Body},
		total: total,
		report: func(done, total int64) {
			hs.report(syncpkg.Progress{
				Detail:     label,
				BytesDone:  done,
				BytesTotal: total,
			})
		},
	}
	_, copyErr := io.Copy(spool, reader)
	closeErr := spool.Close()
	if copyErr != nil {
		return nil, errors.Join(
			fmt.Errorf("download archive: %w", copyErr), closeErr,
		)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close archive spool: %w", closeErr)
	}
	return owned, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

func (a *downloadedArchive) extract(
	ctx context.Context,
	dst string,
	report syncpkg.ProgressFunc,
	label string,
) (err error) {
	if report != nil {
		report(syncpkg.Progress{Detail: label})
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create extraction dir: %w", err)
	}
	open := a.open
	if open == nil {
		open = func() (io.ReadCloser, error) { return os.Open(a.path) }
	}
	archive, err := open()
	if err != nil {
		return fmt.Errorf("open archive spool: %w", err)
	}
	defer func() { err = errors.Join(err, archive.Close()) }()

	var stream io.Reader = archive
	if a.compressed {
		gz, gzipErr := gzip.NewReader(archive)
		if gzipErr != nil {
			return fmt.Errorf("decode archive gzip: %w", gzipErr)
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		stream = gz
	}
	stream = &contextReader{ctx: ctx, r: stream}
	if _, err := ExtractTarStream(ctx, stream, dst); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	return nil
}

func (a *downloadedArchive) Close() error {
	if a == nil || a.dir == "" {
		return nil
	}
	remove := a.remove
	if remove == nil {
		remove = os.RemoveAll
	}
	if err := remove(a.dir); err != nil {
		return fmt.Errorf("remove archive spool dir: %w", err)
	}
	a.dir = ""
	a.path = ""
	return nil
}
