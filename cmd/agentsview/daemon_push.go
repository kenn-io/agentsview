package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type daemonPushRequest struct {
	Full                   bool                 `json:"full"`
	Projects               []string             `json:"projects,omitempty"`
	ExcludeProjects        []string             `json:"exclude_projects,omitempty"`
	PG                     *config.PGConfig     `json:"pg,omitempty"`
	DuckDB                 *config.DuckDBConfig `json:"duckdb,omitempty"`
	SyncStateTarget        string               `json:"sync_state_target,omitempty"`
	MigrateLegacySyncState bool                 `json:"migrate_legacy_sync_state,omitempty"`
	// NoVectors mirrors the CLI --no-vectors flag into the daemon: it has no
	// per-invocation flag of its own, so the gate must travel in the request.
	NoVectors bool `json:"no_vectors,omitempty"`
	// ScopeVectorsToChangedSessions is set by change-triggered watch
	// pushes so the daemon's vector phase reads state only for the
	// changed relational sessions (see postgres.PushOptions).
	ScopeVectorsToChangedSessions bool `json:"scope_vectors_to_changed_sessions,omitempty"`
	// LastReconciledVectorGeneration travels with a scoped push so the
	// daemon's fresh Sync can promote to generation-wide when the active
	// generation id has changed (see postgres.PushOptions).
	LastReconciledVectorGeneration int64 `json:"last_reconciled_vector_generation,omitempty"`
	// Automatic is set by the watch-mode DuckDB pushes so the daemon
	// defers instead of rebuilding when a live serve process holds the
	// mirror and skips archive-scale diagnostics (see
	// duckdbsync.SyncOptions.Automatic).
	Automatic     bool                        `json:"automatic,omitempty"`
	WatchBatch    *syncpkg.WatchBatch         `json:"watch_batch,omitempty"`
	WatchRecovery *syncpkg.WatchRecoveryScope `json:"watch_recovery,omitempty"`
}

// postDaemonPush delegates a push to the local daemon. It negotiates an SSE
// response so the daemon can stream per-phase progress while the push runs;
// each progress event is decoded as P and handed to onProgress (which may be
// nil). A plain JSON response — the daemon streams only when it can flush —
// is decoded directly as the result T.
func postDaemonPush[T, P any](
	ctx context.Context,
	tr transport,
	authToken string,
	path string,
	body daemonPushRequest,
	onProgress func(P),
) (T, error) {
	var zero T
	body = daemonPushRequestForCapabilities(tr, body)
	fallbackAttempted := false
	for {
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
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Origin", tr.URL)
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return zero, err
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !fallbackAttempted && body.WatchBatch != nil &&
				daemonRejectsWatchScope(resp.StatusCode, msg) {
				body.WatchBatch = nil
				body.WatchRecovery = nil
				fallbackAttempted = true
				continue
			}
			return zero, daemonPushError(resp.StatusCode, msg)
		}
		defer resp.Body.Close()
		if strings.HasPrefix(
			resp.Header.Get("Content-Type"), "text/event-stream",
		) {
			return parseDaemonPushSSE[T](resp.Body, onProgress)
		}
		var out T
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return zero, err
		}
		return out, nil
	}
}

func daemonPushRequestForCapabilities(
	tr transport, body daemonPushRequest,
) daemonPushRequest {
	if body.WatchBatch != nil && tr.Runtime != nil && tr.Runtime.API > 0 &&
		tr.Runtime.API < server.ScopedWatchPushAPIVersion {
		body.WatchBatch = nil
		body.WatchRecovery = nil
	}
	return body
}

func daemonRejectsWatchScope(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	message := strings.ToLower(string(body))
	if !strings.Contains(message, "watch_batch") &&
		!strings.Contains(message, "watch_recovery") {
		return false
	}
	return strings.Contains(message, "unexpected") ||
		strings.Contains(message, "unknown") ||
		strings.Contains(message, "additional") ||
		strings.Contains(message, "not allowed")
}

// daemonPushError renders a non-200 daemon response, preferring the API's
// {"error": ...} body over the raw payload.
func daemonPushError(status int, body []byte) error {
	var apiErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error != "" {
		return errors.New(apiErr.Error)
	}
	return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(body)))
}

// parseDaemonPushSSE consumes the daemon push event stream: "progress" events
// decode as P and feed onProgress, a "done" event decodes as the result T,
// and an "error" event (an {"error": ...} body) fails the push. A stream that
// ends without a done event is an error — the daemon died mid-push.
func parseDaemonPushSSE[T, P any](
	r io.Reader, onProgress func(P),
) (T, error) {
	var zero T
	reader := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder
	var done bool
	var result T
	var pushErr error
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		switch event {
		case "done", "report":
			if err := json.Unmarshal([]byte(data.String()), &result); err != nil {
				return fmt.Errorf("decoding daemon push result: %w", err)
			}
			done = true
		case "progress":
			if onProgress == nil {
				return nil
			}
			var p P
			if err := json.Unmarshal([]byte(data.String()), &p); err != nil {
				return fmt.Errorf("decoding daemon push progress: %w", err)
			}
			onProgress(p)
		default:
			var apiErr struct {
				Error string `json:"error"`
			}
			raw := data.String()
			if err := json.Unmarshal([]byte(raw), &apiErr); err == nil &&
				apiErr.Error != "" {
				pushErr = errors.New(apiErr.Error)
			} else {
				pushErr = fmt.Errorf("daemon push error: %s", raw)
			}
		}
		return nil
	}
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return zero, err
			}
			event = ""
			data.Reset()
		} else if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		} else if value, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return zero, readErr
			}
			break
		}
	}
	if err := dispatch(); err != nil {
		return zero, err
	}
	if pushErr != nil {
		return zero, pushErr
	}
	if !done {
		return zero, errors.New("daemon push response missing done event")
	}
	return result, nil
}
