package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/importer"
)

// syncChrome runs platform's Sync in the server through the Chrome extension.
func syncChrome(ctx context.Context, platform importer.BrowserPlatform) error {
	cfg, err := config.LoadMinimalFor(config.ScopeLocal)
	if err != nil {
		return err
	}
	transport, err := detectClientTransportContext(ctx, &cfg, startupWaitTimeout)
	if err != nil {
		return err
	}
	if transport.Mode != transportHTTP {
		return fmt.Errorf("%s Sync requires a running server", platform.Label)
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.ResponseHeaderTimeout = 30 * time.Second
	defer httpTransport.CloseIdleConnections()
	api, err := apiclient.NewHTTPClient(transport.URL, cfg.AuthToken, &http.Client{Transport: httpTransport})
	if err != nil {
		return err
	}
	browser := apiclient.Chrome
	response, err := api.PostAPIV1ImportPlatformSyncStreamWithResponse(ctx, &apiclient.PostAPIV1ImportPlatformSyncRequestOptions{
		PathParams: &apiclient.PostAPIV1ImportPlatformSyncPath{Platform: string(platform.Agent)},
		Query:      &apiclient.PostAPIV1ImportPlatformSyncQuery{Browser: &browser},
	})
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

func readChromeSync(response *apiclient.PostAPIV1ImportPlatformSyncResp) (importer.ImportStats, error) {
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

// browserSyncNames lists the --type values --sync accepts.
func browserSyncNames() string {
	var names []string
	for _, platform := range importer.BrowserPlatforms() {
		names = append(names, string(platform.Agent))
	}
	return strings.Join(names, ", ")
}
