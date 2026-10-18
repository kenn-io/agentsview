package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/importer"
)

func syncChrome(ctx context.Context, importType string) error {
	label := "claude.ai"
	if importType == "chatgpt" {
		label = "ChatGPT"
	}
	cfg, err := config.LoadMinimalFor(config.ScopeLocal)
	if err != nil {
		return err
	}
	transport, err := detectClientTransportContext(ctx, &cfg, startupWaitTimeout)
	if err != nil {
		return err
	}
	if transport.Mode != transportHTTP {
		return fmt.Errorf("%s Sync requires a running server", label)
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.ResponseHeaderTimeout = 30 * time.Second
	defer httpTransport.CloseIdleConnections()
	api, err := apiclient.NewHTTPClient(transport.URL, cfg.AuthToken, &http.Client{Transport: httpTransport})
	if err != nil {
		return err
	}
	var status int
	var body []byte
	var stream *runtime.Stream[[]byte]
	var httpResponse *http.Response
	if importType == "chatgpt" {
		response, streamErr := api.PostAPIV1ImportChatgptSyncStreamWithResponse(ctx)
		err = streamErr
		if response != nil {
			status, body, stream, httpResponse = response.StatusCode, response.Body, response.Stream200, response.HTTPResponse
		}
	} else {
		browser := apiclient.Chrome
		response, streamErr := api.PostAPIV1ImportClaudeAiSyncStreamWithResponse(ctx, &apiclient.PostAPIV1ImportClaudeAiSyncRequestOptions{Query: &apiclient.PostAPIV1ImportClaudeAiSyncQuery{Browser: &browser}})
		err = streamErr
		if response != nil {
			status, body, stream, httpResponse = response.StatusCode, response.Body, response.Stream200, response.HTTPResponse
		}
	}
	if err != nil && (httpResponse == nil || status == http.StatusOK) {
		return err
	}
	defer httpResponse.Body.Close()
	stats, err := readBrowserChromeSync(status, body, stream)
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
	return readBrowserChromeSync(response.StatusCode, response.Body, response.Stream200)
}

func readBrowserChromeSync(status int, body []byte, stream *runtime.Stream[[]byte]) (importer.ImportStats, error) {
	if status != http.StatusOK {
		return importer.ImportStats{}, errors.New(daemonErrorMessage(status, body))
	}
	var stats importer.ImportStats
	result, err := consumeDaemonPushEvents[importer.ImportStats](stream, func(progress importer.ImportStats) {
		stats = progress
	})
	if err != nil {
		return stats, err
	}
	return result, nil
}
