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
	return Importer{
		Host:                    hs.Host,
		Full:                    hs.Full,
		DB:                      hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress,
	}.ImportExtracted(ctx, targets, tmpDir)
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
	if _, err := ExtractTarStream(ctx, resp.Body, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("extract tar: %w", err)
	}
	return tmpDir, nil
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
