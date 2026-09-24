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

func TestFakeFaultIsOneShot(t *testing.T) {
	s := New(t)
	s.Fail(http.MethodGet, "/api/v1/health", Fault{Status: 503, Code: "unavailable", Message: "down"})
	status, body := get(t, s, "/api/v1/health")
	assert.Equal(t, 503, status)
	assert.Equal(t, "unavailable", body["error"].(map[string]any)["code"])
	status, _ = get(t, s, "/api/v1/health")
	assert.Equal(t, 200, status)
}

func TestFakeRequiresTokenExceptHealth(t *testing.T) {
	s := New(t)
	s.SetToken("tok-1")
	status, _ := get(t, s, "/api/v1/health")
	assert.Equal(t, 200, status)
	status, body := get(t, s, "/api/v1/projects")
	assert.Equal(t, 401, status)
	assert.Equal(t, "auth_required", body["error"].(map[string]any)["code"])
}

func TestFakeListFiltersByMetadata(t *testing.T) {
	s := New(t)
	s.AddIssue(Issue{Title: "a", Status: "open", Metadata: map[string]any{"friction.fingerprint": "fl1:aa"}})
	s.AddIssue(Issue{Title: "b", Status: "closed", ClosedReason: "done", Metadata: map[string]any{"friction.fingerprint": "fl1:bb"}})
	_, body := get(t, s, "/api/v1/projects/17/issues?meta=friction.fingerprint%3Dfl1%3Abb")
	issues := body["issues"].([]any)
	require.Len(t, issues, 1)
	assert.Equal(t, "b", issues[0].(map[string]any)["title"])
	assert.True(t, strings.HasPrefix(issues[0].(map[string]any)["qualified_id"].(string), "agentsview#"))
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

func TestFakeCreateIdempotencyAndRequestLog(t *testing.T) {
	s := New(t)
	path := "/api/v1/projects/17/issues"
	body := `{"title":"Capture friction","metadata":{"friction.fingerprint":"fl1:aa"}}`
	status, first := post(t, s, path, "create-1", body)
	assert.Equal(t, 200, status)
	assert.Equal(t, true, first["changed"])
	firstIssue := first["issue"].(map[string]any)
	assert.Equal(t, "Capture friction", firstIssue["title"])
	status, reused := post(t, s, path, "create-1", body)
	assert.Equal(t, 200, status)
	assert.Equal(t, true, reused["reused"])
	assert.Equal(t, firstIssue["uid"], reused["issue"].(map[string]any)["uid"])
	status, mismatch := post(t, s, path, "create-1", `{"title":"Different"}`)
	assert.Equal(t, 409, status)
	assert.Equal(t, "idempotency_mismatch", mismatch["error"].(map[string]any)["code"])
	requests := s.RequestsMatching(http.MethodPost, path)
	require.Len(t, requests, 3)
	assert.Equal(t, "create-1", requests[0].IdempotencyKey)
	assert.Equal(t, []byte(body), requests[0].Body)
	assert.Equal(t, path, requests[0].Path)
	_, listed := get(t, s, path)
	assert.Len(t, listed["issues"], 1)
}

func TestFakeIssueActionsAreIdempotent(t *testing.T) {
	s := New(t)
	issue := s.AddIssue(Issue{Title: "Closed", Status: "closed", ClosedReason: "done"})
	path := "/api/v1/projects/17/issues/" + issue.UID
	for _, tt := range []struct {
		name, suffix, key, body string
		changed                 bool
	}{
		{name: "reopen", suffix: "/actions/reopen", body: `{}`, changed: true},
		{name: "reopen again", suffix: "/actions/reopen", body: `{}`, changed: false},
		{name: "comment", suffix: "/comments", key: "comment-1", body: `{"body":"Observed twice"}`, changed: true},
		{name: "comment again", suffix: "/comments", key: "comment-1", body: `{"body":"Observed twice"}`, changed: false},
		{name: "label", suffix: "/labels", body: `{"label":"friction"}`, changed: true},
		{name: "label again", suffix: "/labels", body: `{"label":"friction"}`, changed: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, out := post(t, s, path+tt.suffix, tt.key, tt.body)
			assert.Equal(t, 200, status)
			assert.Equal(t, tt.changed, out["changed"])
		})
	}
	stored, ok := s.Issue(issue.UID)
	require.True(t, ok)
	assert.Equal(t, "open", stored.Status)
	assert.Empty(t, stored.ClosedReason)
	assert.Equal(t, []string{"Observed twice"}, stored.Comments)
	assert.Equal(t, []string{"friction"}, stored.Labels)
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

func TestFakeCreateIdempotencyIgnoresForceNew(t *testing.T) {
	s := New(t)
	path := "/api/v1/projects/17/issues"
	_, first := post(t, s, path, "same-key", `{"title":"Same"}`)
	status, replay := post(t, s, path, "same-key", `{"title":"Same","force_new":true}`)
	assert.Equal(t, 200, status)
	assert.Equal(t, true, replay["reused"])
	assert.Equal(t, first["issue"].(map[string]any)["uid"], replay["issue"].(map[string]any)["uid"])
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

func TestFakeCommentIdempotencyIncludesActorAndTeammate(t *testing.T) {
	for _, tt := range []struct {
		name, first, replay string
	}{
		{name: "actor", first: `{"body":"note","actor":"one"}`, replay: `{"body":"note","actor":"two"}`},
		{name: "teammate", first: `{"body":"note","teammate":"one"}`, replay: `{"body":"note","teammate":"two"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(t)
			issue := s.AddIssue(Issue{Title: "Existing"})
			path := "/api/v1/projects/17/issues/" + issue.UID + "/comments"
			status, first := post(t, s, path, "comment-key", tt.first)
			assert.Equal(t, 200, status)
			assert.Equal(t, true, first["changed"])
			status, mismatch := post(t, s, path, "comment-key", tt.replay)
			assert.Equal(t, 409, status)
			assert.Equal(t, "idempotency_mismatch", mismatch["error"].(map[string]any)["code"])
			stored, ok := s.Issue(issue.UID)
			require.True(t, ok)
			assert.Equal(t, []string{"note"}, stored.Comments)
		})
	}
}

func TestFakeShowIncludesStoredComments(t *testing.T) {
	s := New(t)
	issue := s.AddIssue(Issue{Title: "Existing"})
	path := "/api/v1/projects/17/issues/" + issue.UID
	post(t, s, path+"/comments", "", `{"body":"first"}`)
	post(t, s, path+"/comments", "", `{"body":"second"}`)
	_, shown := get(t, s, path)
	comments := shown["comments"].([]any)
	require.Len(t, comments, 2)
	assert.Equal(t, "first", comments[0].(map[string]any)["body"])
	assert.Equal(t, "second", comments[1].(map[string]any)["body"])
	assert.NotEqual(t, comments[0].(map[string]any)["uid"], comments[1].(map[string]any)["uid"])
}

func TestFakeCreateIdempotencyIgnoresLabelOrder(t *testing.T) {
	s := New(t)
	path := "/api/v1/projects/17/issues"
	_, first := post(t, s, path, "label-key", `{"title":"Same","labels":["alpha","beta"]}`)
	status, replay := post(t, s, path, "label-key", `{"title":"Same","labels":["beta","alpha"]}`)
	assert.Equal(t, 200, status)
	assert.Equal(t, true, replay["reused"])
	assert.Equal(t, first["issue"].(map[string]any)["uid"], replay["issue"].(map[string]any)["uid"])
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

func TestFakeCommentReplayReturnsOriginalCommentIdentity(t *testing.T) {
	s := New(t)
	issue := s.AddIssue(Issue{Title: "Existing"})
	path := "/api/v1/projects/17/issues/" + issue.UID + "/comments"
	_, first := post(t, s, path, "key-a", `{"body":"first"}`)
	_, second := post(t, s, path, "key-b", `{"body":"second"}`)
	status, replay := post(t, s, path, "key-a", `{"body":"first"}`)
	assert.Equal(t, 200, status)
	assert.Equal(t, false, replay["changed"])
	firstComment := first["comment"].(map[string]any)
	secondComment := second["comment"].(map[string]any)
	replayComment := replay["comment"].(map[string]any)
	assert.Equal(t, firstComment["body"], replayComment["body"])
	assert.Equal(t, firstComment["id"], replayComment["id"])
	assert.Equal(t, firstComment["uid"], replayComment["uid"])
	assert.NotEqual(t, secondComment["uid"], replayComment["uid"])
	stored, ok := s.Issue(issue.UID)
	require.True(t, ok)
	assert.Equal(t, []string{"first", "second"}, stored.Comments)
}
