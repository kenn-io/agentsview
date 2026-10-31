package katatest

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func get(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+path, nil)
	require.NoError(t, err)
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return resp.StatusCode, out
}

func TestFakeServesDetectionAndRecords(t *testing.T) {
	s := New(t)
	tests := []struct {
		name   string
		path   string
		status int
		key    string
	}{
		{name: "health", path: "/api/v1/health", status: 200, key: "api_schema_version"},
		{name: "instance", path: "/api/v1/instance", status: 200, key: "instance_uid"},
		{name: "projects", path: "/api/v1/projects", status: 200, key: "projects"},
		{name: "unknown", path: "/api/v1/nope", status: 404, key: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := get(t, s, tt.path)
			assert.Equal(t, tt.status, status)
			assert.Contains(t, body, tt.key)
		})
	}
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1)
}

func post(t *testing.T, s *Server, path, key, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.UnmarshalRead(resp.Body, &out))
	return resp.StatusCode, out
}

func TestFakeUnixEndpointAndFaultQueue(t *testing.T) {
	s, endpoint := NewUnix(t)
	assert.Equal(t, endpoint, s.Endpoint())
	assert.True(t, strings.HasPrefix(endpoint, "unix://"))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.socketPath)
		},
	}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	s.Fail(http.MethodGet, "/api/v1/health", Fault{Status: 503, Code: "first", Message: "one"})
	s.Fail(http.MethodGet, "/api/v1/health", Fault{Status: 502, Code: "second", Message: "two"})
	for _, tt := range []struct {
		status int
		code   string
	}{
		{status: 503, code: "first"},
		{status: 502, code: "second"},
		{status: 200},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unix/api/v1/health", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.UnmarshalRead(resp.Body, &out))
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, tt.status, resp.StatusCode)
		if tt.code != "" {
			assert.Equal(t, tt.code, out["error"].(map[string]any)["code"])
		}
	}
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 3)
}

func TestUnixEndpointEscapesReservedPathCharacters(t *testing.T) {
	assert.Equal(t, "unix:///tmp/kata%23scratch%25dir/d.sock", UnixEndpoint("/tmp/kata#scratch%dir/d.sock"))
}

func TestFakeRejectsUnsupportedIssueMethods(t *testing.T) {
	s := New(t)
	issue := s.AddIssue(Issue{Title: "Existing"})
	for _, tt := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPatch, path: "/api/v1/projects/17/issues"},
		{method: http.MethodGet, path: "/api/v1/projects/17/issues/" + issue.UID + "/labels"},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tt.method, s.URL+tt.path, nil)
			require.NoError(t, err)
			resp, err := s.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			var out map[string]any
			require.NoError(t, json.UnmarshalRead(resp.Body, &out))
			assert.Equal(t, 404, resp.StatusCode)
			errBody, ok := out["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "not_found", errBody["code"])
		})
	}
}

func TestFakeSnapshotsMutableInputsAndOutputs(t *testing.T) {
	s := New(t)
	projects := []Project{{ID: DefaultProjectID, UID: "01J00000000000000000000017", Name: DefaultProjectName, Active: true}, {ID: 23, UID: "01J00000000000000000000023", Name: "sample", Active: true}}
	s.SetProjects(projects...)
	projects[0].Name = "changed outside"
	_, listedProjects := get(t, s, "/api/v1/projects")
	assert.Equal(t, DefaultProjectName, listedProjects["projects"].([]any)[0].(map[string]any)["name"])
	assert.Equal(t, "sample", listedProjects["projects"].([]any)[1].(map[string]any)["name"])

	priority := int64(2)
	labels := []string{"original"}
	nested := map[string]any{"nested": []any{"value"}}
	metadata := map[string]any{"detail": nested}
	created := s.AddIssue(Issue{Title: "Recorded", Priority: &priority, Labels: labels, Metadata: metadata})
	priority = 4
	labels[0] = "changed outside"
	nested["nested"].([]any)[0] = "changed outside"
	*created.Priority = 3
	created.Labels[0] = "changed result"
	created.Metadata["detail"].(map[string]any)["nested"].([]any)[0] = "changed result"
	read, ok := s.Issue(created.UID)
	require.True(t, ok)
	require.NotNil(t, read.Priority)
	assert.Equal(t, int64(2), *read.Priority)
	assert.Equal(t, []string{"original"}, read.Labels)
	assert.Equal(t, "value", read.Metadata["detail"].(map[string]any)["nested"].([]any)[0])
	*read.Priority = 1
	read.Labels[0] = "changed read"
	read.Metadata["detail"].(map[string]any)["nested"].([]any)[0] = "changed read"
	_, body := get(t, s, "/api/v1/projects/17/issues")
	issues := body["issues"].([]any)
	require.Len(t, issues, 1)
	assert.Equal(t, "original", issues[0].(map[string]any)["labels"].([]any)[0])
	assert.Equal(t, "value", issues[0].(map[string]any)["metadata"].(map[string]any)["detail"].(map[string]any)["nested"].([]any)[0])

	post(t, s, "/api/v1/projects/17/issues", "", `{"title":"Logged"}`)
	requests := s.Requests()
	require.NotEmpty(t, requests)
	requests[len(requests)-1].Body[0] = 'X'
	assert.Equal(t, []byte(`{"title":"Logged"}`), s.Requests()[len(requests)-1].Body)
}

func TestFakeGeneratedUIDsAreStrictULIDs(t *testing.T) {
	s := New(t)
	_, instance := get(t, s, "/api/v1/instance")
	_, projects := get(t, s, "/api/v1/projects")
	issue := s.AddIssue(Issue{Title: "Identifier"})
	_, comment := post(t, s, "/api/v1/projects/17/issues/"+issue.UID+"/comments", "", `{"body":"note"}`)
	for name, uid := range map[string]string{
		"instance": instance["instance_uid"].(string),
		"project":  projects["projects"].([]any)[0].(map[string]any)["uid"].(string),
		"issue":    issue.UID,
		"comment":  comment["comment"].(map[string]any)["uid"].(string),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ulid.ParseStrict(uid)
			assert.NoError(t, err)
		})
	}
}

func TestFakeFaultSnapshotsData(t *testing.T) {
	s := New(t)
	data := map[string]any{"detail": map[string]any{"reason": "original"}}
	s.Fail(http.MethodGet, "/api/v1/health", Fault{Status: 503, Code: "unavailable", Message: "down", Data: data})
	data["detail"].(map[string]any)["reason"] = "changed outside"
	status, body := get(t, s, "/api/v1/health")
	assert.Equal(t, 503, status)
	assert.Equal(t, "original", body["error"].(map[string]any)["data"].(map[string]any)["detail"].(map[string]any)["reason"])
}
