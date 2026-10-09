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
		if json.Unmarshal(body, &failure) == nil {
			return importer.ImportStats{}, errors.New(failure.Error)
		}
		return importer.ImportStats{}, fmt.Errorf("claude.ai Sync: HTTP %d: %s", response.StatusCode, body)
	}
	var stats importer.ImportStats
	result, err := consumeDaemonPushEvents[importer.ImportStats](response.Stream200, func(progress importer.ImportStats) {
		stats = progress
	})
	if err != nil {
		return stats, err
	}
	return result, nil
}
