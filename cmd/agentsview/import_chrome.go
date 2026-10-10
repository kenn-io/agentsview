package main

import (
	"context"
	"errors"
	"fmt"
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
		return importer.ImportStats{}, errors.New(daemonErrorMessage(response.StatusCode, response.Body))
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
