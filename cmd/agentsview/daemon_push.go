package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

type daemonPushRequest struct {
	Full                   bool                 `json:"full"`
	Projects               []string             `json:"projects,omitempty"`
	ExcludeProjects        []string             `json:"exclude_projects,omitempty"`
	PG                     *config.PGConfig     `json:"pg,omitempty"`
	DuckDB                 *config.DuckDBConfig `json:"duckdb,omitempty"`
	SyncStateTarget        string               `json:"sync_state_target,omitempty"`
	MigrateLegacySyncState bool                 `json:"migrate_legacy_sync_state,omitempty"`
}

func postDaemonPush[T any](
	ctx context.Context,
	tr transport,
	authToken string,
	path string,
	body daemonPushRequest,
) (T, error) {
	var zero T
	data, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimSuffix(tr.URL, "/")+path,
		bytes.NewReader(data),
	)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", tr.URL)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		var apiErr struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(msg, &apiErr); err == nil &&
			apiErr.Error != "" {
			return zero, errors.New(apiErr.Error)
		}
		return zero, fmt.Errorf(
			"HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)),
		)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}
