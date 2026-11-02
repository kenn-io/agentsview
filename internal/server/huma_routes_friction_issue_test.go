package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

type testFrictionIssue struct {
	QualifiedID string `json:"qualified_id"`
	IssueUID    string `json:"issue_uid"`
	Source      string `json:"source"`
}

type testFrictionFile struct {
	Issue   *testFrictionIssue `json:"issue"`
	Created bool               `json:"created"`
	Preview *struct {
		Title  string   `json:"title"`
		Labels []string `json:"labels"`
	} `json:"preview"`
}

func frictionIssueEnv(t *testing.T, cfg kata.Config) (*testEnv, *katatest.Server, string) {
	t.Helper()
	s := katatest.New(t)
	if cfg.Endpoint == "" {
		cfg.Endpoint = s.Endpoint()
	}
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(kata.NewConn(cfg))})
	const date = "2026-09-14"
	seedFrictionDigest(t, te.db, date)
	return te, s, frictionTestSnapshot(date).Signals[0].Fingerprint()
}

func patternIssue(t *testing.T, te *testEnv, fingerprint string) *testFrictionIssue {
	t.Helper()
	w := te.get(t, "/api/v1/friction/patterns")
	assertStatus(t, w, http.StatusOK)
	got := decode[struct {
		Patterns []struct {
			Fingerprint string             `json:"fingerprint"`
			Issue       *testFrictionIssue `json:"issue"`
		} `json:"patterns"`
	}](t, w)
	for _, p := range got.Patterns {
		if p.Fingerprint == fingerprint {
			return p.Issue
		}
	}
	require.FailNow(t, "pattern not listed", fingerprint)
	return nil
}

func TestFrictionIssueRoutes(t *testing.T) {
	te, s, fp := frictionIssueEnv(t, kata.Config{Enabled: true, Hub: true, Project: katatest.DefaultProjectName})
	path := "/api/v1/friction/patterns/" + fp + "/issue"

	w := te.post(t, path, `{"dry_run":true}`)
	assertStatus(t, w, http.StatusOK)
	preview := decode[testFrictionFile](t, w)
	require.NotNil(t, preview.Preview)
	assert.Contains(t, preview.Preview.Title, "[friction/correction]")
	assert.Equal(t, []string{"friction", "friction:correction"}, preview.Preview.Labels)
	assert.Nil(t, preview.Issue)
	assert.Empty(t, s.Requests(), "a dry run never contacts Kata")

	w = te.post(t, path, `{}`)
	assertStatus(t, w, http.StatusOK)
	filed := decode[testFrictionFile](t, w)
	require.NotNil(t, filed.Issue)
	assert.True(t, filed.Created)
	assert.Equal(t, "created", filed.Issue.Source)
	assert.Equal(t, filed.Issue, patternIssue(t, te, fp), "pattern reads carry the issue")

	digest := decode[struct {
		Patterns []struct {
			Fingerprint string             `json:"fingerprint"`
			Issue       *testFrictionIssue `json:"issue"`
		} `json:"patterns"`
	}](t, te.get(t, "/api/v1/friction/digests/2026-09-14"))
	var digestIssue *testFrictionIssue
	for _, p := range digest.Patterns {
		if p.Fingerprint == fp {
			digestIssue = p.Issue
		}
	}
	assert.Equal(t, filed.Issue, digestIssue, "the digest's patterns carry the issue beside the stored digest")

	w = te.post(t, path, `{}`)
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[testFrictionFile](t, w).Created, "filing again returns the existing link")
	assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), 1)

	other := s.AddIssue(katatest.Issue{Title: "tracked by hand", Status: "open"})
	w = te.put(t, path, `{"issue":"`+katatest.DefaultProjectName+`#`+other.ShortID+`"}`)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "manual", patternIssue(t, te, fp).Source)
	assert.Equal(t, other.UID, patternIssue(t, te, fp).IssueUID)
	assertStatus(t, te.put(t, path, `{"issue":"not/a ref"}`), http.StatusUnprocessableEntity)

	assertStatus(t, te.del(t, path), http.StatusNoContent)
	assert.Nil(t, patternIssue(t, te, fp))
	assertStatus(t, te.del(t, path), http.StatusNotFound)
	assertStatus(t, te.post(t, "/api/v1/friction/patterns/fl1:missing/issue", `{}`), http.StatusNotFound)
}

func TestFrictionIssueRoutesRefuse(t *testing.T) {
	tests := []struct {
		name   string
		cfg    kata.Config
		remote string
		want   int
	}{
		{name: "remote_without_auth", cfg: kata.Config{Enabled: true, Hub: true, Project: katatest.DefaultProjectName}, remote: "203.0.113.9:4000", want: http.StatusForbidden},
		{name: "kata_disabled", cfg: kata.Config{Enabled: false, Hub: true}, want: http.StatusConflict},
		{name: "not_the_hub", cfg: kata.Config{Enabled: true, Hub: false, Project: katatest.DefaultProjectName}, want: http.StatusConflict},
		{name: "kata_not_ready", cfg: kata.Config{Enabled: true, Hub: true, Project: "no-such-project"}, want: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te, s, fp := frictionIssueEnv(t, tt.cfg)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/api/v1/friction/patterns/"+fp+"/issue", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://127.0.0.1:0")
			if tt.remote != "" {
				req.RemoteAddr = tt.remote
			}
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, tt.want)
			assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
			assert.Nil(t, patternIssue(t, te, fp))
		})
	}
}
