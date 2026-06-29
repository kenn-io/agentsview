package remotesync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	tmpDir, err := hs.downloadAndExtract(ctx, client, targets)
	if err != nil {
		return SyncStats{}, err
	}
	defer os.RemoveAll(tmpDir)
	stats, err := Importer{
		Host:                    hs.Host,
		Full:                    hs.Full,
		DB:                      hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress,
	}.ImportExtracted(ctx, targets, tmpDir)
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

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("remote sync %s: %s", resp.Status, msg)
}
