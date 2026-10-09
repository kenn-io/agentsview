package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/importer"
)

func syncClaudeAIChrome(ctx context.Context) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return err
	}
	transport, err := detectTransportContext(ctx, cfg.DataDir, cfg.AuthToken, startupWaitTimeout)
	if err != nil {
		return err
	}
	if transport.Mode != transportHTTP {
		return errors.New("Claude.ai Sync requires a running server")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(transport.URL, "/")+"/api/v1/import/claude-ai/sync?browser=chrome", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Origin", daemonOriginURL(transport.URL))
	if cfg.AuthToken != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	stats, err := readChromeSync(response)
	printImportSummary(stats)
	if err != nil {
		return err
	}
	if stats.Errors > 0 {
		return fmt.Errorf("sync completed with %d errors", stats.Errors)
	}
	return nil
}

func readChromeSync(response *http.Response) (importer.ImportStats, error) {
	if response.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if err != nil {
			return importer.ImportStats{}, err
		}
		var failure struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Code == "claude_ai_chrome_host_required" {
			return importer.ImportStats{}, errors.New(failure.Error)
		}
		return importer.ImportStats{}, fmt.Errorf("Claude.ai Sync: HTTP %d: %s", response.StatusCode, body)
	}
	stream := runtime.NewEventStream[[]byte](response)
	defer stream.Close()
	var stats importer.ImportStats
	for stream.Next() {
		frame := stream.Event()
		switch frame.Type {
		case "progress", "done":
			var next importer.ImportStats
			if err := json.Unmarshal(frame.Data, &next); err != nil {
				return stats, err
			}
			stats = next
			if frame.Type == "done" {
				return stats, nil
			}
		case "error":
			var failure struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(frame.Data, &failure); err != nil {
				return stats, err
			}
			return stats, errors.New(failure.Error)
		case "fetch":
			return stats, errors.New("Chrome Sync unexpectedly requested a page relay")
		}
	}
	if err := stream.Err(); err != nil {
		return stats, err
	}
	return stats, errors.New("Claude.ai Sync stream ended without a result")
}
