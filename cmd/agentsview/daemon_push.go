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

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/server"
)

type daemonPushTarget int

const (
	daemonPushPG daemonPushTarget = iota
	daemonPushDuckDB
	daemonStartupSync
)

// postDaemonPush delegates a push to the local daemon. It negotiates an SSE
// response so the daemon can stream per-phase progress while the push runs;
// each progress event is decoded as P and handed to onProgress (which may be
// nil). A plain JSON response — the daemon streams only when it can flush —
// is decoded directly as the result T.
func postDaemonPush[T, P any](
	ctx context.Context,
	tr transport,
	authToken string,
	target daemonPushTarget,
	body apiclient.DaemonPushRequest,
	onProgress func(P),
) (T, error) {
	var zero T
	body = daemonPushRequestForCapabilities(tr, body)
	fallbackAttempted := false
	for {
		api, err := apiclient.NewHTTPClient(tr.URL, authToken, http.DefaultClient)
		if err != nil {
			return zero, err
		}
		var resp *http.Response
		var payload []byte
		switch target {
		case daemonPushPG:
			response, requestErr := api.PostAPIV1PushPgStreamWithResponse(ctx, &apiclient.PostAPIV1PushPgRequestOptions{Body: &body})
			if response == nil {
				return zero, requestErr
			}
			resp, payload = response.HTTPResponse, response.Body
		case daemonPushDuckDB:
			response, requestErr := api.PostAPIV1PushDuckdbStreamWithResponse(ctx, &apiclient.PostAPIV1PushDuckdbRequestOptions{Body: &body})
			if response == nil {
				return zero, requestErr
			}
			resp, payload = response.HTTPResponse, response.Body
		case daemonStartupSync:
			response, requestErr := api.PostAPIV1SyncStreamWithResponse(ctx, &apiclient.PostAPIV1SyncRequestOptions{Query: &apiclient.PostAPIV1SyncQuery{Wait: new(true), StartupOnly: new(true)}})
			if response == nil {
				return zero, requestErr
			}
			resp, payload = response.HTTPResponse, response.Body
		default:
			return zero, fmt.Errorf("unknown daemon push target: %d", target)
		}
		if resp.StatusCode != http.StatusOK {
			msg := payload
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
		if err := json.Unmarshal(payload, &out); err != nil {
			return zero, err
		}
		return out, nil
	}
}

func daemonPushRequestForCapabilities(
	tr transport, body apiclient.DaemonPushRequest,
) apiclient.DaemonPushRequest {
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
