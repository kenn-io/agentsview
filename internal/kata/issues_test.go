package kata

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/kata/katatest"
)

func readyClient(t *testing.T, s *katatest.Server) *Client {
	t.Helper()
	st, c := Probe(t.Context(), readyConfig(s.Endpoint()))
	require.Equal(t, StateReady, st.State, st.Message)
	return c
}

func TestFindByMetadata(t *testing.T) {
	s := katatest.New(t)
	s.SetWebOrigin("https://kata.example.test")
	open := s.AddIssue(katatest.Issue{Title: "open one", Status: "open", Metadata: map[string]any{"friction.fingerprint": "fl1:aa"}})
	s.AddIssue(katatest.Issue{Title: "closed one", Status: "closed", ClosedReason: "wontfix", Metadata: map[string]any{"friction.fingerprint": "fl1:bb"}})
	c := readyClient(t, s)

	got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:aa")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, open.UID, got[0].UID)
	assert.Equal(t, "agentsview#"+open.ShortID, got[0].QualifiedID)
	assert.Equal(t, "https://kata.example.test/issues/"+open.UID, got[0].WebURL)

	got, err = c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:bb")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "closed", got[0].Status)
	assert.Equal(t, "wontfix", got[0].ClosedReason)

	reqs := s.RequestsMatching(http.MethodGet, "/api/v1/projects/17/issues")
	require.NotEmpty(t, reqs)
	assert.Equal(t, "meta=friction.fingerprint%3Dfl1%3Abb", reqs[len(reqs)-1].RawQuery, "no status filter, no limit")
}

func TestSupportedProjectNamesInIssueRefs(t *testing.T) {
	for _, project := range []string{"team/ops", "team?ops"} {
		t.Run(project, func(t *testing.T) {
			s := katatest.New(t)
			s.SetProjects(katatest.Project{ID: 17, UID: "01J00000000000000000000001", Name: project, Active: true})
			want := s.AddIssue(katatest.Issue{Title: "existing", Status: "closed", Metadata: map[string]any{"friction.fingerprint": "fl1:existing"}})
			cfg := readyConfig(s.Endpoint())
			cfg.Project = project
			st, c := Probe(t.Context(), cfg)
			require.Equal(t, StateReady, st.State, st.Message)

			listed, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:existing")
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.Equal(t, project+"#"+want.ShortID, listed[0].QualifiedID)

			s.ResetRequests()
			got, err := c.GetIssue(t.Context(), project+"#"+want.ShortID)
			require.NoError(t, err)
			assert.Equal(t, want.UID, got.UID)
			assert.Equal(t, project+"#"+want.ShortID, got.QualifiedID)
			reqs := s.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "/api/v1/projects/17/issues/"+want.ShortID, reqs[0].Path)
			assert.Empty(t, reqs[0].RawQuery)

			s.ResetRequests()
			changed, err := c.Reopen(t.Context(), project+"#"+want.ShortID)
			require.NoError(t, err)
			assert.True(t, changed)
			reqs = s.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "/api/v1/projects/17/issues/"+want.ShortID+"/actions/reopen", reqs[0].Path)
			assert.Empty(t, reqs[0].RawQuery)

			created, err := c.CreateIssue(t.Context(), "project-name-key", CreateIssue{Title: "new"})
			require.NoError(t, err)
			assert.Equal(t, project+"#"+created.Issue.ShortID, created.Issue.QualifiedID)
			assert.Equal(t, "open", created.Issue.Status)
			assert.Len(t, s.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 1)

			s.ResetRequests()
			for _, ref := range []string{
				project + "#" + want.ShortID + "/../",
				project + "#" + want.ShortID + "?leak=1",
				"other#" + want.ShortID,
			} {
				_, err := c.GetIssue(t.Context(), ref)
				require.Error(t, err, ref)
				_, err = c.Reopen(t.Context(), ref)
				require.Error(t, err, ref)
			}
			assert.Empty(t, s.Requests(), "invalid refs never reach Kata")
		})
	}
}

// A malformed list is an error, never an empty result.
func TestFindByMetadataDriftIsLoud(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "parse_list_response_malformed_json_is_loud_not_empty", body: "not json at all"},
		{name: "parse_list_response_missing_issues_collection_is_loud", body: `{"kata_api_version":1,"items":[]}`},
		{name: "parse_list_response_missing_status_is_loud", body: `{"issues":[{"uid":"01AAA","short_id":"ab12","title":"drifty","qualified_id":"agentsview#ab12"}]}`},
		{name: "parse_list_response_issue_without_ref_is_loud", body: `{"issues":[{"title":"no refs here","status":"open"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: tt.body})
			got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Nil(t, got)
		})
	}
}

func TestFindByMetadataRejectsForeignOrMismatchedIssues(t *testing.T) {
	const local = `{"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open","qualified_id":"agentsview#f008","metadata":{"friction.fingerprint":"fl1:x"}}`
	tests := []struct {
		name string
		body string
	}{
		{name: "foreign_project_id", body: `{"issues":[` + local + `,{"uid":"01J00000000000000000000009","short_id":"f009","project_id":18,"status":"open","qualified_id":"agentsview#f009","metadata":{"friction.fingerprint":"fl1:x"}}]}`},
		{name: "foreign_project_uid", body: `{"issues":[{"uid":"01J00000000000000000000009","short_id":"f009","project_id":17,"project_uid":"01J00000000000000000000003","status":"open","qualified_id":"agentsview#f009","metadata":{"friction.fingerprint":"fl1:x"}}]}`},
		{name: "wrong_qualified_id", body: `{"issues":[{"uid":"01J00000000000000000000009","short_id":"f009","project_id":17,"status":"open","qualified_id":"other#f009","metadata":{"friction.fingerprint":"fl1:x"}}]}`},
		{name: "missing_qualified_id", body: `{"issues":[{"uid":"01J00000000000000000000009","short_id":"f009","project_id":17,"status":"open","metadata":{"friction.fingerprint":"fl1:x"}}]}`},
		{name: "leading_space_short_id", body: `{"issues":[{"uid":"01J00000000000000000000009","short_id":" f009","project_id":17,"status":"open","qualified_id":"agentsview# f009","metadata":{"friction.fingerprint":"fl1:x"}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: tt.body})
			got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Nil(t, got, "never expose a partial list or a foreign issue")
		})
	}
}

func TestFindByMetadataRejectsMalformedIssueUID(t *testing.T) {
	for _, tt := range []struct {
		name, uid string
	}{
		{name: "short", uid: "not-a-uid"},
		{name: "invalid_alphabet", uid: "01J0000000000000000000000I"},
		{name: "overflow", uid: "81J00000000000000000000008"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			body := `{"issues":[{"uid":"` + tt.uid + `","short_id":"f008","project_id":17,"status":"open","qualified_id":"agentsview#f008","metadata":{"friction.fingerprint":"fl1:x"}}]}`
			s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: body})

			got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), tt.uid)
			assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/projects/17/issues"), 1)
		})
	}
}

func TestFindByMetadataRejectsUnfilteredRows(t *testing.T) {
	const good = `{"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open","qualified_id":"agentsview#f008","metadata":{"friction.fingerprint":"fl1:x"}}`
	const missing = `{"uid":"01J00000000000000000000009","short_id":"f009","project_id":17,"status":"open","qualified_id":"agentsview#f009","metadata":{"other":"fl1:x"}}`
	const wrong = `{"uid":"01J00000000000000000000010","short_id":"f010","project_id":17,"status":"open","qualified_id":"agentsview#f010","metadata":{"friction.fingerprint":"fl1:y"}}`
	const wrongType = `{"uid":"01J00000000000000000000011","short_id":"f011","project_id":17,"status":"open","qualified_id":"agentsview#f011","metadata":{"friction.fingerprint":["fl1:x"]}}`
	tests := []struct {
		name    string
		body    string
		wantUID string
	}{
		{name: "matching_metadata", body: `{"issues":[` + good + `]}`, wantUID: "01J00000000000000000000008"},
		{name: "missing_key", body: `{"issues":[` + missing + `]}`},
		{name: "different_value", body: `{"issues":[` + wrong + `]}`},
		{name: "non_string_value", body: `{"issues":[` + wrongType + `]}`},
		{name: "valid_then_missing", body: `{"issues":[` + good + `,` + missing + `]}`},
		{name: "valid_then_different", body: `{"issues":[` + good + `,` + wrong + `]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: tt.body})

			got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
			if tt.wantUID != "" {
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, tt.wantUID, got[0].UID)
				return
			}
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Nil(t, got, "a faulty filter must never expose even a valid partial list")
		})
	}
}

func TestCreateIssue(t *testing.T) {
	s := katatest.New(t)
	s.SetWebOrigin("https://kata.example.test")
	c := readyClient(t, s)
	req := CreateIssue{
		Title: "[friction/error] bash: boom", Body: "b", Priority: 3,
		Labels: []string{"friction", "friction:error"}, Metadata: map[string]any{"friction.fingerprint": "fl1:cc"},
	}

	first, err := c.CreateIssue(t.Context(), "key-1", req)
	require.NoError(t, err)
	assert.False(t, first.Reused)
	assert.Equal(t, "open", first.Issue.Status)
	assert.Equal(t, "agentsview#"+first.Issue.ShortID, first.Issue.QualifiedID)
	assert.Equal(t, "https://kata.example.test/issues/"+first.Issue.UID, first.Issue.WebURL)
	assert.Equal(t, []string{"friction", "friction:error"}, first.Issue.Labels)
	assert.Equal(t, map[string]any{"friction.fingerprint": "fl1:cc"}, first.Issue.Metadata)
	require.NotNil(t, first.Issue.Priority)
	assert.Equal(t, 3, *first.Issue.Priority)

	again, err := c.CreateIssue(t.Context(), "key-1", req)
	require.NoError(t, err)
	assert.True(t, again.Reused)
	assert.Equal(t, first.Issue.UID, again.Issue.UID)

	post := s.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues")
	require.Len(t, post, 2)
	assert.Equal(t, "key-1", post[0].IdempotencyKey)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(post[0].Body, &sent))
	assert.Equal(t, "agentsview", sent["actor"])
	assert.Equal(t, "[friction/error] bash: boom", sent["title"])
	assert.Equal(t, "b", sent["body"])
	assert.Equal(t, []any{"friction", "friction:error"}, sent["labels"])
	assert.Equal(t, map[string]any{"friction.fingerprint": "fl1:cc"}, sent["metadata"])
	assert.NotContains(t, sent, "force_new", "force_new is sent only when true")
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/issues/"+first.Issue.UID), 2)

	_, err = c.CreateIssue(t.Context(), "", req)
	require.ErrorIs(t, err, ErrIdempotencyKeyRequired)

	req.Body = "changed"
	_, err = c.CreateIssue(t.Context(), "key-1", req)
	assert.True(t, IsCode(err, "idempotency_mismatch"))
}

func TestCreateIssueForceNewAndPriorityBounds(t *testing.T) {
	s := katatest.New(t)
	c := readyClient(t, s)
	_, err := c.CreateIssue(t.Context(), "k", CreateIssue{Title: "t", ForceNew: true, Priority: 2})
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(s.RequestsMatching(http.MethodPost, "/issues")[0].Body, &sent))
	assert.Equal(t, true, sent["force_new"])

	_, err = c.CreateIssue(t.Context(), "k2", CreateIssue{Title: "t", Priority: 5})
	require.Error(t, err)
	assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), 1, "invalid priority never reaches Kata")
}

func TestCreateIssueRejectsForeignProjectResponse(t *testing.T) {
	s := katatest.New(t)
	c := readyClient(t, s)
	s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{
		Status:  200,
		RawBody: `{"changed":true,"issue":{"uid":"01J00000000000000000000003","short_id":"f003","project_id":18,"status":"open","title":"foreign"}}`,
	})
	got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "local"})
	require.ErrorIs(t, err, ErrInvalidResponse)
	assert.Empty(t, got.Issue.QualifiedID)
}

func TestCreateIssueRereadFailureRequiresCompleteAcknowledgement(t *testing.T) {
	const uid = "01J00000000000000000000003"
	tests := []struct {
		name       string
		ack        string
		wantStatus string
		wantError  bool
	}{
		{
			name:      "missing_status",
			ack:       `{"changed":true,"issue":{"uid":"` + uid + `","short_id":"f003","project_id":17,"title":"created"}}`,
			wantError: true,
		},
		{
			name:      "missing_title",
			ack:       `{"changed":true,"issue":{"uid":"` + uid + `","short_id":"f003","project_id":17,"status":"open"}}`,
			wantError: true,
		},
		{
			name:       "complete_acknowledgement",
			ack:        `{"changed":true,"issue":{"uid":"` + uid + `","short_id":"f003","project_id":17,"status":"open","title":"created"}}`,
			wantStatus: "open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: tt.ack})
			s.Fail(http.MethodGet, "/api/v1/issues/"+uid, katatest.Fault{Status: 503, Code: "unavailable", Message: "temporary failure"})

			got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "created"})
			if tt.wantError {
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.Empty(t, got.Issue.UID, "incomplete acknowledgement cannot look successful")
			} else {
				require.NoError(t, err)
				assert.Equal(t, uid, got.Issue.UID)
				assert.Equal(t, tt.wantStatus, got.Issue.Status)
			}
			assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/issues/"+uid), 1)
		})
	}
}

func TestCreateIssueRejectsMalformedIssueUIDBeforeReread(t *testing.T) {
	for _, tt := range []struct {
		name, uid string
	}{
		{name: "short", uid: "not-a-uid"},
		{name: "invalid_alphabet", uid: "01J0000000000000000000000I"},
		{name: "overflow", uid: "81J00000000000000000000008"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			body := `{"changed":true,"issue":{"uid":"` + tt.uid + `","short_id":"f008","project_id":17,"status":"open","title":"created"}}`
			s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: body})

			got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "created"})
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Equal(t, CreateResult{}, got)
			assert.NotContains(t, err.Error(), tt.uid)
			assert.Len(t, s.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 1)
			assert.Empty(t, s.RequestsMatching(http.MethodGet, "/issues/"+tt.uid), "malformed acknowledgement cannot reach the reread route")
		})
	}
}

func TestGetIssueRefForms(t *testing.T) {
	s := katatest.New(t)
	is := s.AddIssue(katatest.Issue{Title: "t", Status: "closed", ClosedReason: "done", Labels: []string{"friction"}})
	c := readyClient(t, s)
	tests := []struct {
		name     string
		ref      string
		wantPath string
	}{
		{name: "uid_uses_global_route", ref: is.UID, wantPath: "/api/v1/issues/" + is.UID},
		{name: "short_id", ref: is.ShortID, wantPath: "/api/v1/projects/17/issues/" + is.ShortID},
		{name: "qualified", ref: "agentsview#" + is.ShortID, wantPath: "/api/v1/projects/17/issues/" + is.ShortID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.ResetRequests()
			got, err := c.GetIssue(t.Context(), tt.ref)
			require.NoError(t, err)
			assert.Equal(t, is.UID, got.UID)
			assert.Equal(t, "done", got.ClosedReason)
			assert.Equal(t, []string{"friction"}, got.Labels)
			reqs := s.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, tt.wantPath, reqs[0].Path)
		})
	}
	_, err := c.GetIssue(t.Context(), "zzzz")
	assert.True(t, IsCode(err, "issue_not_found"))
	_, err = c.GetIssue(t.Context(), "a/b")
	require.Error(t, err, "refs with a slash are rejected before any request")
}

func TestGetIssueRejectsDifferentIssueInSuccessfulResponse(t *testing.T) {
	const requestedUID = "01J00000000000000000000008"
	const returnedUID = "01J00000000000000000000009"
	const returned = `{"issue":{"uid":"` + returnedUID + `","short_id":"f009","project_id":17,"status":"open","title":"different"},"labels":[],"comments":[],"links":[]}`
	tests := []struct {
		name, ref, path string
	}{
		{name: "uid", ref: requestedUID, path: "/api/v1/issues/" + requestedUID},
		{name: "short_id", ref: "f008", path: "/api/v1/projects/17/issues/f008"},
		{name: "qualified_id", ref: "agentsview#f008", path: "/api/v1/projects/17/issues/f008"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(http.MethodGet, tt.path, katatest.Fault{Status: 200, RawBody: returned})
			got, err := c.GetIssue(t.Context(), tt.ref)
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Equal(t, Issue{}, got, "a 200 response for another issue cannot be exposed")
			assert.Len(t, s.RequestsMatching(http.MethodGet, tt.path), 1)
		})
	}
}

func TestGetIssueRejectsMalformedIssueUID(t *testing.T) {
	for _, tt := range []struct {
		name, uid string
	}{
		{name: "short", uid: "not-a-uid"},
		{name: "invalid_alphabet", uid: "01J0000000000000000000000I"},
		{name: "overflow", uid: "81J00000000000000000000008"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			body := `{"issue":{"uid":"` + tt.uid + `","short_id":"f008","project_id":17,"status":"open","title":"created"},"labels":[],"comments":[],"links":[]}`
			path := "/api/v1/projects/17/issues/f008"
			s.Fail(http.MethodGet, path, katatest.Fault{Status: 200, RawBody: body})

			got, err := c.GetIssue(t.Context(), "f008")
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Equal(t, Issue{}, got)
			assert.NotContains(t, err.Error(), tt.uid)
			assert.Len(t, s.RequestsMatching(http.MethodGet, path), 1)
		})
	}
}

func TestCreateIssueRereadRejectsDifferentIssue(t *testing.T) {
	const requestedUID = "01J00000000000000000000008"
	const returnedUID = "01J00000000000000000000009"
	s := katatest.New(t)
	c := readyClient(t, s)
	s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: `{"changed":true,"issue":{"uid":"` + requestedUID + `","short_id":"f008","project_id":17,"status":"open","title":"created"}}`})
	s.Fail(http.MethodGet, "/api/v1/issues/"+requestedUID, katatest.Fault{Status: 200, RawBody: `{"issue":{"uid":"` + returnedUID + `","short_id":"f009","project_id":17,"status":"open","title":"different"},"labels":[],"comments":[],"links":[]}`})

	got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "created"})
	require.NoError(t, err)
	assert.Equal(t, requestedUID, got.Issue.UID, "the reread cannot replace the create acknowledgement")
	assert.Equal(t, "f008", got.Issue.ShortID)
	assert.Equal(t, "created", got.Issue.Title)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/issues/"+requestedUID), 1)
}

func TestGetIssueRejectsMalformedShortIDInSuccessfulResponse(t *testing.T) {
	const uid = "01J00000000000000000000008"
	tests := []struct {
		name, shortID string
	}{
		{name: "extra_separator", shortID: "f#008"},
		{name: "path_separator", shortID: "f/008"},
		{name: "leading_space", shortID: " f008"},
		{name: "embedded_space", shortID: "f 008"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			path := "/api/v1/issues/" + uid
			body := `{"issue":{"uid":"` + uid + `","short_id":"` + tt.shortID + `","project_id":17,"status":"open","title":"created"},"labels":[],"comments":[],"links":[]}`
			s.Fail(http.MethodGet, path, katatest.Fault{Status: 200, RawBody: body})

			got, err := c.GetIssue(t.Context(), uid)
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Equal(t, Issue{}, got)
			assert.NotContains(t, err.Error(), tt.shortID)
			assert.Len(t, s.RequestsMatching(http.MethodGet, path), 1)
		})
	}
}

func TestCreateIssueRejectsMalformedShortIDAfterRereadFailure(t *testing.T) {
	const uid = "01J00000000000000000000008"
	tests := []struct {
		name, shortID string
	}{
		{name: "extra_separator", shortID: "f#008"},
		{name: "path_separator", shortID: "f/008"},
		{name: "leading_space", shortID: " f008"},
		{name: "embedded_space", shortID: "f 008"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			body := `{"changed":true,"issue":{"uid":"` + uid + `","short_id":"` + tt.shortID + `","project_id":17,"status":"open","title":"created"}}`
			s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: body})
			s.Fail(http.MethodGet, "/api/v1/issues/"+uid, katatest.Fault{Status: 503, Code: "unavailable", Message: "temporary failure"})

			got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "created"})
			require.ErrorIs(t, err, ErrInvalidResponse)
			assert.Equal(t, CreateResult{}, got)
			assert.NotContains(t, err.Error(), tt.shortID)
			assert.Len(t, s.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 1)
			assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/issues/"+uid), 1, "the malformed create acknowledgement must also fail after a failed re-read")
		})
	}
}

func TestIssueResponsesRejectMalformedShortIDAlphabetAndLength(t *testing.T) {
	const uid = "01J00000000000000000000008"
	shortIDs := []struct {
		name, value string
	}{
		{name: "invalid_letter", value: "f00i"},
		{name: "uppercase", value: "F008"},
		{name: "control", value: "f0\\u0000"},
		{name: "too_short", value: "abc"},
		{name: "too_long", value: "aaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, tt := range shortIDs {
		t.Run(tt.name, func(t *testing.T) {
			issueFields := `"uid":"` + uid + `","short_id":"` + tt.value + `","project_id":17,"status":"open","title":"created"`
			issue := `{` + issueFields + `}`
			t.Run("list", func(t *testing.T) {
				s := katatest.New(t)
				c := readyClient(t, s)
				s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: `{"issues":[{` + issueFields + `,"qualified_id":"agentsview#` + tt.value + `","metadata":{"friction.fingerprint":"fl1:x"}}]}`})
				got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.Nil(t, got)
				assert.NotContains(t, err.Error(), tt.value)
			})
			t.Run("show", func(t *testing.T) {
				s := katatest.New(t)
				c := readyClient(t, s)
				s.Fail(http.MethodGet, "/api/v1/issues/"+uid, katatest.Fault{Status: 200, RawBody: `{"issue":` + issue + `,"labels":[],"comments":[],"links":[]}`})
				got, err := c.GetIssue(t.Context(), uid)
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.Equal(t, Issue{}, got)
				assert.NotContains(t, err.Error(), tt.value)
			})
			t.Run("create", func(t *testing.T) {
				s := katatest.New(t)
				c := readyClient(t, s)
				s.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: `{"changed":true,"issue":` + issue + `}`})
				s.Fail(http.MethodGet, "/api/v1/issues/"+uid, katatest.Fault{Status: 503, Code: "unavailable", Message: "temporary failure"})
				got, err := c.CreateIssue(t.Context(), "create-key", CreateIssue{Title: "created"})
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.Equal(t, CreateResult{}, got)
				assert.NotContains(t, err.Error(), tt.value)
			})
			t.Run("action_by_uid", func(t *testing.T) {
				s := katatest.New(t)
				c := readyClient(t, s)
				s.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 200, RawBody: `{"changed":true,"issue":` + issue + `}`})
				changed, err := c.Reopen(t.Context(), uid)
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.False(t, changed)
				assert.NotContains(t, err.Error(), tt.value)
			})
		})
	}
}

func TestGetIssueAcceptsShortIDLengthBoundaries(t *testing.T) {
	const uid = "01J00000000000000000000008"
	for _, tt := range []struct {
		name, shortID, wantQualifiedID string
	}{
		{name: "minimum", shortID: "f008", wantQualifiedID: "agentsview#f008"},
		{name: "maximum", shortID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", wantQualifiedID: "agentsview#aaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			body := `{"issue":{"uid":"` + uid + `","short_id":"` + tt.shortID + `","project_id":17,"status":"open","title":"created"},"labels":[],"comments":[],"links":[]}`
			s.Fail(http.MethodGet, "/api/v1/issues/"+uid, katatest.Fault{Status: 200, RawBody: body})

			got, err := c.GetIssue(t.Context(), uid)
			require.NoError(t, err)
			assert.Equal(t, tt.wantQualifiedID, got.QualifiedID)
		})
	}
}

func TestGetIssueRejectsForeignProjectUID(t *testing.T) {
	s := katatest.New(t)
	s.SetProjects(
		katatest.Project{ID: 17, UID: "01J00000000000000000000001", Name: "agentsview", Active: true},
		katatest.Project{ID: 18, UID: "01J00000000000000000000003", Name: "another", Active: true},
	)
	foreign := s.AddIssue(katatest.Issue{Title: "foreign", ProjectID: 18})
	local := s.AddIssue(katatest.Issue{Title: "local", ProjectID: 17})
	c := readyClient(t, s)
	s.ResetRequests()

	got, err := c.GetIssue(t.Context(), foreign.UID)
	require.ErrorIs(t, err, ErrInvalidResponse)
	assert.Empty(t, got.QualifiedID, "never assign this project's slug to a foreign issue")

	got, err = c.GetIssue(t.Context(), local.UID)
	require.NoError(t, err)
	assert.Equal(t, "agentsview#"+local.ShortID, got.QualifiedID)
	reqs := s.Requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, "/api/v1/issues/"+foreign.UID, reqs[0].Path)
	assert.Equal(t, "/api/v1/issues/"+local.UID, reqs[1].Path)
}

func TestIssueRejectsMismatchedProjectUID(t *testing.T) {
	const issueUID = "01J00000000000000000000008"
	const issueJSON = `{"uid":"` + issueUID + `","short_id":"f008","project_id":17,"project_uid":"01J00000000000000000000003","status":"open"}`
	tests := []struct {
		name, method, suffix, response string
		call                           func(*testing.T, *Client) error
	}{
		{
			name: "show", method: http.MethodGet, suffix: "/api/v1/issues/" + issueUID,
			response: `{"issue":` + issueJSON + `,"labels":[],"comments":[],"links":[]}`,
			call: func(t *testing.T, c *Client) error {
				t.Helper()
				_, err := c.GetIssue(t.Context(), issueUID)
				return err
			},
		},
		{
			name: "create", method: http.MethodPost, suffix: "/api/v1/projects/17/issues",
			response: `{"changed":true,"issue":` + issueJSON + `}`,
			call: func(t *testing.T, c *Client) error {
				t.Helper()
				_, err := c.CreateIssue(t.Context(), "key", CreateIssue{Title: "t"})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			c := readyClient(t, s)
			s.Fail(tt.method, tt.suffix, katatest.Fault{Status: 200, RawBody: tt.response})
			require.ErrorIs(t, tt.call(t, c), ErrInvalidResponse)
		})
	}
}

func TestReopenCommentLabel(t *testing.T) {
	s := katatest.New(t)
	is := s.AddIssue(katatest.Issue{Title: "t", Status: "closed", ClosedReason: "done"})
	c := readyClient(t, s)

	changed, err := c.Reopen(t.Context(), is.UID)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = c.Reopen(t.Context(), is.UID)
	require.NoError(t, err)
	assert.False(t, changed, "reopen of an open issue is a no-op")

	require.NoError(t, c.Comment(t.Context(), is.UID, "ck-1", "Recurred"))
	require.NoError(t, c.Comment(t.Context(), is.UID, "ck-1", "Recurred"))
	require.NoError(t, c.AddLabel(t.Context(), is.UID, "friction:recurred"))
	require.NoError(t, c.AddLabel(t.Context(), is.UID, "friction:recurred"))

	after, ok := s.Issue(is.UID)
	require.True(t, ok)
	assert.Equal(t, []string{"Recurred"}, after.Comments, "same comment key does not duplicate")
	assert.Equal(t, []string{"friction:recurred"}, after.Labels)

	reopen := s.RequestsMatching(http.MethodPost, "/actions/reopen")
	require.Len(t, reopen, 2)
	assert.JSONEq(t, `{"actor":"agentsview"}`, string(reopen[0].Body), "never retry_protocol")
	assert.Equal(t, "ck-1", s.RequestsMatching(http.MethodPost, "/comments")[0].IdempotencyKey)
}

func TestIssueActionsRejectEmptySuccess(t *testing.T) {
	tests := []struct {
		name, suffix string
		call         func(*testing.T, *Client, string) error
	}{
		{name: "reopen", suffix: "/actions/reopen", call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			_, err := c.Reopen(t.Context(), ref)
			return err
		}},
		{name: "comment", suffix: "/comments", call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.Comment(t.Context(), ref, "comment-key", "Recurred")
		}},
		{name: "label", suffix: "/labels", call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.AddLabel(t.Context(), ref, "friction:recurred")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			is := s.AddIssue(katatest.Issue{Title: "t", Status: "closed"})
			c := readyClient(t, s)
			s.Fail(http.MethodPost, tt.suffix, katatest.Fault{Status: 200, RawBody: `{}`})
			err := tt.call(t, c, is.UID)
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestIssueActionsValidateAcknowledgement(t *testing.T) {
	tests := []struct {
		name, suffix, body string
		call               func(*testing.T, *Client, string) error
	}{
		{name: "reopen_wrong_issue", suffix: "/actions/reopen", body: `{"changed":true,"issue":{"uid":"01J00000000000000000000009","short_id":"f009","project_id":17,"status":"open"}}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			_, err := c.Reopen(t.Context(), ref)
			return err
		}},
		{name: "comment_missing_uid", suffix: "/comments", body: `{"changed":true,"issue":{"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open"},"comment":{"body":"Recurred"}}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.Comment(t.Context(), ref, "comment-key", "Recurred")
		}},
		{name: "label_wrong_label", suffix: "/labels", body: `{"changed":true,"issue":{"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open"},"label":{"label":"other"}}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.AddLabel(t.Context(), ref, "friction:recurred")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			is := s.AddIssue(katatest.Issue{UID: "01J00000000000000000000008", ShortID: "f008", Title: "t", Status: "closed"})
			c := readyClient(t, s)
			s.Fail(http.MethodPost, tt.suffix, katatest.Fault{Status: 200, RawBody: tt.body})
			err := tt.call(t, c, is.UID)
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestIssueActionsRejectMalformedIssueUID(t *testing.T) {
	actions := []struct {
		name, suffix, attachment string
		call                     func(*testing.T, *Client) error
	}{
		{name: "reopen", suffix: "/actions/reopen", call: func(t *testing.T, c *Client) error {
			t.Helper()
			_, err := c.Reopen(t.Context(), "f008")
			return err
		}},
		{name: "comment", suffix: "/comments", attachment: `,"comment":{"uid":"01J00000000000000000000009","body":"Recurred","issue_id":1}`, call: func(t *testing.T, c *Client) error {
			t.Helper()
			return c.Comment(t.Context(), "f008", "comment-key", "Recurred")
		}},
		{name: "label", suffix: "/labels", attachment: `,"label":{"label":"friction:recurred","issue_id":1}`, call: func(t *testing.T, c *Client) error {
			t.Helper()
			return c.AddLabel(t.Context(), "f008", "friction:recurred")
		}},
	}
	for _, uid := range []string{"not-a-uid", "01J0000000000000000000000I", "81J00000000000000000000008"} {
		for _, action := range actions {
			t.Run(action.name+"/"+uid, func(t *testing.T) {
				s := katatest.New(t)
				c := readyClient(t, s)
				body := `{"changed":true,"issue":{"id":1,"uid":"` + uid + `","short_id":"f008","project_id":17,"status":"open"}` + action.attachment + `}`
				s.Fail(http.MethodPost, action.suffix, katatest.Fault{Status: 200, RawBody: body})

				err := action.call(t, c)
				require.ErrorIs(t, err, ErrInvalidResponse)
				assert.NotContains(t, err.Error(), uid)
				assert.Len(t, s.RequestsMatching(http.MethodPost, action.suffix), 1)
			})
		}
	}
}

func TestIssueActionAttachmentMatchesIssue(t *testing.T) {
	const issue = `{"id":1,"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open"}`
	const issueWithoutID = `{"uid":"01J00000000000000000000008","short_id":"f008","project_id":17,"status":"open"}`
	tests := []struct {
		name, suffix, issue, attachment string
		call                            func(*testing.T, *Client, string) error
	}{
		{name: "comment_wrong_issue", suffix: "/comments", attachment: `"comment":{"uid":"01J00000000000000000000009","body":"Recurred","issue_id":2}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.Comment(t.Context(), ref, "comment-key", "Recurred")
		}},
		{name: "comment_missing_issue", suffix: "/comments", attachment: `"comment":{"uid":"01J00000000000000000000009","body":"Recurred"}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.Comment(t.Context(), ref, "comment-key", "Recurred")
		}},
		{name: "comment_both_ids_missing", suffix: "/comments", issue: issueWithoutID, attachment: `"comment":{"uid":"01J00000000000000000000009","body":"Recurred"}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.Comment(t.Context(), ref, "comment-key", "Recurred")
		}},
		{name: "label_wrong_issue", suffix: "/labels", attachment: `"label":{"label":"friction:recurred","issue_id":2}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.AddLabel(t.Context(), ref, "friction:recurred")
		}},
		{name: "label_missing_issue", suffix: "/labels", attachment: `"label":{"label":"friction:recurred"}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.AddLabel(t.Context(), ref, "friction:recurred")
		}},
		{name: "label_both_ids_missing", suffix: "/labels", issue: issueWithoutID, attachment: `"label":{"label":"friction:recurred"}`, call: func(t *testing.T, c *Client, ref string) error {
			t.Helper()
			return c.AddLabel(t.Context(), ref, "friction:recurred")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			is := s.AddIssue(katatest.Issue{UID: "01J00000000000000000000008", ShortID: "f008", Title: "t"})
			c := readyClient(t, s)
			issueBody := tt.issue
			if issueBody == "" {
				issueBody = issue
			}
			s.Fail(http.MethodPost, tt.suffix, katatest.Fault{Status: 200, RawBody: `{"issue":` + issueBody + `,` + tt.attachment + `}`})
			require.ErrorIs(t, tt.call(t, c, is.UID), ErrInvalidResponse)
		})
	}
}

func TestProjectNotFoundReresolvesOnce(t *testing.T) {
	s := katatest.New(t)
	c := readyClient(t, s)
	s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 404, Code: "project_not_found", Message: "gone"})
	got, err := c.FindByMetadata(t.Context(), "friction.fingerprint", "fl1:x")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/projects"), 2, "probe + one re-resolve")
}

func TestIssueErrorsDoNotEchoUntrustedText(t *testing.T) {
	s := katatest.New(t)
	c := readyClient(t, s)
	const secret = "secret-capability-value"

	_, err := c.GetIssue(t.Context(), "a/"+secret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Empty(t, s.RequestsMatching(http.MethodGet, "/issues/a/"))

	s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{
		Status: 200, RawBody: `{"issues":[{"title":"secret-capability-value","status":"open"}]}`,
	})
	_, err = c.FindByMetadata(t.Context(), "k", "v")
	require.ErrorIs(t, err, ErrInvalidResponse)
	assert.NotContains(t, err.Error(), secret)
}
