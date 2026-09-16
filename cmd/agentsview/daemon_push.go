package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
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
		var stream *runtime.Stream[[]byte]
		switch target {
		case daemonPushPG:
			response, requestErr := api.PostAPIV1PushPgStreamWithResponse(ctx, &apiclient.PostAPIV1PushPgRequestOptions{Body: &body})
			if response == nil {
				return zero, requestErr
			}
			resp, payload, stream = response.HTTPResponse, response.Body, response.Stream200
		case daemonPushDuckDB:
			response, requestErr := api.PostAPIV1PushDuckdbStreamWithResponse(ctx, &apiclient.PostAPIV1PushDuckdbRequestOptions{Body: &body})
			if response == nil {
				return zero, requestErr
			}
			resp, payload, stream = response.HTTPResponse, response.Body, response.Stream200
		case daemonStartupSync:
			response, requestErr := api.PostAPIV1SyncStreamWithResponse(ctx, &apiclient.PostAPIV1SyncRequestOptions{Query: &apiclient.PostAPIV1SyncQuery{Wait: new(true), StartupOnly: new(true)}})
			if response == nil {
				return zero, requestErr
			}
			resp, payload, stream = response.HTTPResponse, response.Body, response.Stream200
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
			return consumeDaemonPushEvents[T](stream, onProgress)
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

// consumeDaemonPushEvents applies daemon progress and terminal events decoded
// by the generated client's stream.
func consumeDaemonPushEvents[T, P any](stream *runtime.Stream[[]byte], onProgress func(P)) (T, error) {
	var result, zero T
	var done bool
	var pushErr error
	defer stream.Close()
	for stream.Next() {
		frame := stream.Event()
		if len(frame.Data) == 0 {
			continue
		}
		switch frame.Type {
		case "done", "report":
			if err := json.Unmarshal(frame.Data, &result); err != nil {
				return zero, fmt.Errorf("decoding daemon push result: %w", err)
			}
			done = true
		case "progress":
			if onProgress != nil {
				var progress P
				if err := json.Unmarshal(frame.Data, &progress); err != nil {
					return zero, fmt.Errorf("decoding daemon push progress: %w", err)
				}
				onProgress(progress)
			}
		default:
			var apiErr apiclient.APIErrorResponse
			if err := json.Unmarshal(frame.Data, &apiErr); err == nil && apiErr.ErrorData != "" {
				pushErr = errors.New(apiErr.ErrorData)
			} else {
				pushErr = fmt.Errorf("daemon push error: %s", frame.Data)
			}
		}
	}
	if err := stream.Err(); err != nil {
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
