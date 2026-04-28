// ABOUTME: httpBackend implements SessionService by proxying HTTP
// ABOUTME: calls to a running agentsview daemon.
package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

// errHTTPNotFound is returned by getJSON for 404 responses so callers
// can distinguish "no such resource" from other transport errors
// without string-matching the status code. Kept unexported since
// only Get currently consumes it; other paths map status codes
// explicitly below.
var errHTTPNotFound = errors.New("http: not found")

type httpBackend struct {
	baseURL  string
	client   *http.Client
	readOnly bool
	token    string
}

// NewHTTPBackend constructs a SessionService that proxies to a
// running agentsview daemon at baseURL. When readOnly is true,
// Sync returns a clear error without making the HTTP round-trip.
// token, when non-empty, is attached as `Authorization: Bearer ...`
// on every request so the backend works against daemons running
// with require_auth=true.
func NewHTTPBackend(baseURL, token string, readOnly bool) SessionService {
	return &httpBackend{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		client:   &http.Client{Timeout: 30 * time.Second},
		readOnly: readOnly,
		token:    token,
	}
}

func (b *httpBackend) Get(
	ctx context.Context, id string,
) (*SessionDetail, error) {
	var out SessionDetail
	path := "/api/v1/sessions/" + url.PathEscape(id)
	err := b.getJSON(ctx, path, &out)
	if errors.Is(err, errHTTPNotFound) {
		// Match directBackend.Get: absent session returns (nil, nil)
		// so transport swaps stay neutral.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) List(
	ctx context.Context, f ListFilter,
) (*SessionList, error) {
	q := filterToQuery(f)
	var out SessionList
	if err := b.getJSON(ctx, "/api/v1/sessions?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// filterToQuery converts a ListFilter into the URL query params
// expected by handleListSessions. Field mapping mirrors the
// server-side parser in internal/server/sessions.go.
func filterToQuery(f ListFilter) url.Values {
	q := url.Values{}
	setIfNotEmpty := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	setIfNotEmpty("project", f.Project)
	setIfNotEmpty("exclude_project", f.ExcludeProject)
	setIfNotEmpty("machine", f.Machine)
	setIfNotEmpty("agent", f.Agent)
	setIfNotEmpty("date", f.Date)
	setIfNotEmpty("date_from", f.DateFrom)
	setIfNotEmpty("date_to", f.DateTo)
	setIfNotEmpty("active_since", f.ActiveSince)
	if f.MinMessages > 0 {
		q.Set("min_messages", strconv.Itoa(f.MinMessages))
	}
	if f.MaxMessages > 0 {
		q.Set("max_messages", strconv.Itoa(f.MaxMessages))
	}
	if f.MinUserMessages > 0 {
		q.Set("min_user_messages", strconv.Itoa(f.MinUserMessages))
	}
	if f.IncludeOneShot {
		q.Set("include_one_shot", "true")
	}
	if f.IncludeAutomated {
		q.Set("include_automated", "true")
	}
	if f.IncludeChildren {
		q.Set("include_children", "true")
	}
	setIfNotEmpty("outcome", f.Outcome)
	setIfNotEmpty("health_grade", f.HealthGrade)
	setIfNotEmpty("termination", f.Termination)
	if f.MinToolFailures != nil {
		q.Set("min_tool_failures", strconv.Itoa(*f.MinToolFailures))
	}
	setIfNotEmpty("cursor", f.Cursor)
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

func (b *httpBackend) Messages(
	ctx context.Context, id string, f MessageFilter,
) (*MessageList, error) {
	q := url.Values{}
	if f.From != nil {
		q.Set("from", strconv.Itoa(*f.From))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Direction != "" {
		q.Set("direction", f.Direction)
	}
	path := "/api/v1/sessions/" + url.PathEscape(id) +
		"/messages?" + q.Encode()
	var out MessageList
	if err := b.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) ToolCalls(
	ctx context.Context, id string,
) (*ToolCallList, error) {
	var out ToolCallList
	path := "/api/v1/sessions/" + url.PathEscape(id) + "/tool-calls"
	if err := b.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *httpBackend) Sync(
	ctx context.Context, in SyncInput,
) (*SessionDetail, error) {
	if b.readOnly {
		// Return the shared sentinel so callers can
		// errors.Is(err, db.ErrReadOnly) regardless of
		// transport.
		return nil, fmt.Errorf(
			"sync: daemon at %s is read-only: %w",
			b.baseURL, db.ErrReadOnly,
		)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		b.baseURL+"/api/v1/sessions/sync",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The daemon's CSRF guard rejects mutating requests whose Origin
	// is not in the allowlist. Setting Origin to the daemon's own
	// baseURL satisfies that check for the CLI, which has no real
	// browser origin.
	req.Header.Set("Origin", b.baseURL)
	b.addAuth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		// Daemon is read-only (pg serve). Surface as the shared
		// sentinel so CLI callers can errors.Is it.
		return nil, fmt.Errorf(
			"sync: daemon at %s: %w", b.baseURL, db.ErrReadOnly,
		)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"sync: HTTP %d: %s", resp.StatusCode, msg,
		)
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

func (b *httpBackend) Watch(
	ctx context.Context, id string,
) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		b.baseURL+"/api/v1/sessions/"+url.PathEscape(id)+"/watch",
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	b.addAuth(req)
	// Use a separate no-timeout client so long-lived streams do not
	// hit the 30s default on b.client.
	streamingClient := &http.Client{Timeout: 0}
	resp, err := streamingClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("watch: session not found: %s", id)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("watch: HTTP %d", resp.StatusCode)
	}

	out := make(chan Event)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		parseSSE(resp.Body, func(ev Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return out, nil
}

// Stats is not yet implemented over HTTP; the daemon currently has
// no /stats endpoint. Subsequent tasks may add one.
func (b *httpBackend) Stats(
	_ context.Context, _ StatsFilter,
) (*SessionStats, error) {
	return nil, errors.New("stats over HTTP backend: not yet implemented")
}

// parseSSE reads a Server-Sent Events stream and invokes emit for
// each complete event. emit returns false to stop parsing (e.g. on
// context cancel).
func parseSSE(r io.Reader, emit func(Event) bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var event, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event != "" {
				if !emit(Event{Event: event, Data: data}) {
					return
				}
			}
			event, data = "", ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event = line[len("event: "):]
		} else if strings.HasPrefix(line, "data: ") {
			data = line[len("data: "):]
		}
	}
}

// addAuth attaches the bearer token to req when the backend was
// constructed with one. Safe to call on a request without a token
// configured (no-op).
func (b *httpBackend) addAuth(req *http.Request) {
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
}

func (b *httpBackend) getJSON(
	ctx context.Context, path string, out any,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, b.baseURL+path, nil,
	)
	if err != nil {
		return err
	}
	b.addAuth(req)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errHTTPNotFound
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"GET %s: HTTP %d: %s", path, resp.StatusCode, msg,
		)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
