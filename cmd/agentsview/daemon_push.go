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
	// LastReconciledVectorFingerprint travels with a scoped push so the
	// daemon's fresh Sync can promote to generation-wide when the active
	// generation fingerprint has changed (see postgres.PushOptions).
	LastReconciledVectorFingerprint string `json:"last_reconciled_vector_fingerprint,omitempty"`
	// Automatic is set by the watch-mode DuckDB pushes so the daemon
	// defers instead of rebuilding when a live serve process holds the
	// mirror and skips archive-scale diagnostics (see
	// duckdbsync.SyncOptions.Automatic).
	Automatic bool `json:"automatic,omitempty"`
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return zero, daemonPushError(resp.StatusCode, msg)
	}
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
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
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
		case "done":
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
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return zero, err
			}
			event = ""
			data.Reset()
			continue
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return zero, err
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
