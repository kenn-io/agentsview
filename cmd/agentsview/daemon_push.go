package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/server"
)

// daemonPushOperation invokes one generated streaming daemon operation and
// returns its raw response parts so postDaemonPush can decode them
// uniformly. Every daemon push route has one; the generated client has no
// path parameter for the backend name, so each registered replica maps its
// Name() to its generated operation in replicaPushOperations.
type daemonPushOperation func(
	ctx context.Context, api *apiclient.Client, body *apiclient.DaemonPushRequest,
) (*http.Response, []byte, *runtime.Stream[[]byte], error)

// replicaPushOperations maps a replica's Name() to its generated daemon push
// operation. Adding a replica means regenerating the API client and adding
// its entry here.
var replicaPushOperations = map[string]daemonPushOperation{
	"pg": func(
		ctx context.Context, api *apiclient.Client, body *apiclient.DaemonPushRequest,
	) (*http.Response, []byte, *runtime.Stream[[]byte], error) {
		response, err := api.PostAPIV1PushPgStreamWithResponse(
			ctx, &apiclient.PostAPIV1PushPgRequestOptions{Body: body},
		)
		if response == nil {
			return nil, nil, nil, err
		}
		return response.HTTPResponse, response.Body, response.Stream200, nil
	},
	"clickhouse": func(
		ctx context.Context, api *apiclient.Client, body *apiclient.DaemonPushRequest,
	) (*http.Response, []byte, *runtime.Stream[[]byte], error) {
		response, err := api.PostAPIV1PushClickhouseStreamWithResponse(
			ctx, &apiclient.PostAPIV1PushClickhouseRequestOptions{Body: body},
		)
		if response == nil {
			return nil, nil, nil, err
		}
		return response.HTTPResponse, response.Body, response.Stream200, nil
	},
}

func replicaPushOperation(name string) (daemonPushOperation, error) {
	operation, ok := replicaPushOperations[name]
	if !ok {
		return nil, fmt.Errorf(
			"replica backend %q has no daemon push operation; "+
				"regenerate the API client and register it in replicaPushOperations",
			name,
		)
	}
	return operation, nil
}

func mirrorPushOperation(
	ctx context.Context, api *apiclient.Client, body *apiclient.DaemonPushRequest,
) (*http.Response, []byte, *runtime.Stream[[]byte], error) {
	response, err := api.PostAPIV1PushDuckdbStreamWithResponse(
		ctx, &apiclient.PostAPIV1PushDuckdbRequestOptions{Body: body},
	)
	if response == nil {
		return nil, nil, nil, err
	}
	return response.HTTPResponse, response.Body, response.Stream200, nil
}

func startupSyncOperation(
	ctx context.Context, api *apiclient.Client, _ *apiclient.DaemonPushRequest,
) (*http.Response, []byte, *runtime.Stream[[]byte], error) {
	response, err := api.PostAPIV1SyncStreamWithResponse(
		ctx, &apiclient.PostAPIV1SyncRequestOptions{
			Query: &apiclient.PostAPIV1SyncQuery{Wait: new(true), StartupOnly: new(true)},
		},
	)
	if response == nil {
		return nil, nil, nil, err
	}
	return response.HTTPResponse, response.Body, response.Stream200, nil
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
	operation daemonPushOperation,
	body apiclient.DaemonPushRequest,
	onProgress func(P),
) (T, error) {
	var zero T
	body = daemonPushRequestForCapabilities(tr, body)
	fallbackAttempted := false
	for {
		api, err := apiclient.NewHTTPClient(tr.URL, authToken, &http.Client{Timeout: 0})
		if err != nil {
			return zero, err
		}
		resp, payload, stream, err := operation(ctx, api, &body)
		if resp == nil {
			return zero, err
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
			return zero, errors.New(daemonErrorMessage(resp.StatusCode, msg))
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
