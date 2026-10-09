package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
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
		return errors.New("claude.ai Sync requires a running server")
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	httpTransport.ResponseHeaderTimeout = 30 * time.Second
	defer httpTransport.CloseIdleConnections()
	api, err := apiclient.NewHTTPClient(transport.URL, cfg.AuthToken, &http.Client{Transport: httpTransport})
	if err != nil {
		return err
	}
	browser := apiclient.Chrome
	response, err := api.PostAPIV1ImportClaudeAiSyncStreamWithResponse(ctx, &apiclient.PostAPIV1ImportClaudeAiSyncRequestOptions{Query: &apiclient.PostAPIV1ImportClaudeAiSyncQuery{Browser: &browser}})
	if err != nil && (response == nil || response.StatusCode == http.StatusOK) {
		return err
	}
	defer response.HTTPResponse.Body.Close()
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

func readChromeSync(response *apiclient.PostAPIV1ImportClaudeAiSyncResp) (importer.ImportStats, error) {
	if response.StatusCode != http.StatusOK {
		body := response.Body
		var failure struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Code == "claude_ai_chrome_host_required" {
			return importer.ImportStats{}, errors.New(failure.Error)
		}
		return importer.ImportStats{}, fmt.Errorf("claude.ai Sync: HTTP %d: %s", response.StatusCode, body)
	}
	stream := response.Stream200
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
		}
	}
	if err := stream.Err(); err != nil {
		return stats, err
	}
	return stats, errors.New("claude.ai Sync stream ended without a result")
}
