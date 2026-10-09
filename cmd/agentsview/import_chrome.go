package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	if cfg.AuthToken != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	stats, err := readChromeSync(response)
	if err != nil {
		return err
	}
	printImportSummary(stats)
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
		return importer.ImportStats{}, fmt.Errorf("Claude.ai Sync: HTTP %d: %s", response.StatusCode, body)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			switch event {
			case "done":
				var stats importer.ImportStats
				err := json.Unmarshal([]byte(data), &stats)
				return stats, err
			case "error":
				var failure struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(data), &failure); err != nil {
					return importer.ImportStats{}, err
				}
				return importer.ImportStats{}, errors.New(failure.Error)
			case "fetch":
				return importer.ImportStats{}, errors.New("Chrome Sync unexpectedly requested a page relay")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return importer.ImportStats{}, err
	}
	return importer.ImportStats{}, errors.New("Claude.ai Sync stream ended without a result")
}
