// internal/server/friction_filing_routes_test.go
package server_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func frictionFilingEnv(t *testing.T, hub bool) (*testEnv, *katatest.Server, friction.Signal) {
	t.Helper()
	fake := katatest.New(t)
	sig := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", ToolName: "Bash", Text: "boom", Ordinal: new(1)}
	conn := kata.NewConn(kata.Config{Enabled: true, Hub: hub, Endpoint: fake.Endpoint(), Project: "agentsview"})
	opts := []server.Option{server.WithKataConn(conn)}
	var te *testEnv
	if hub {
		f := &filing.Filer{
			Kata: conn, Now: time.Now, Policy: filing.Policy{Kinds: filing.DefaultKinds()},
			Snapshot: func(context.Context, string) (friction.DigestSnapshot, error) {
				return friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}, nil
			},
		}
		opts = append(opts, server.WithFrictionFiler(f))
		te = setupWithServerOpts(t, opts)
		f.Store = fixedDates{LinkStore: te.db, dates: []string{"2026-09-20"}}
	} else {
		te = setupWithServerOpts(t, opts)
	}
	return te, fake, sig
}

type fixedDates struct {
	filing.LinkStore
	dates []string
}

func (f fixedDates) DigestDatesForFingerprints(context.Context, []string) ([]string, error) {
	return f.dates, nil
}

func (te *testEnv) postFrom(t *testing.T, remoteAddr, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	return w
}

func TestFrictionFileRoute(t *testing.T) {
	tests := []struct {
		name       string
		hub        bool
		body       string
		wantStatus int
		wantCode   string
		wantState  string
		check      func(t *testing.T, fake *katatest.Server, resp map[string]any)
	}{
		{name: "files", hub: true, body: `{}`, wantStatus: 200, check: func(t *testing.T, fake *katatest.Server, resp map[string]any) {
			t.Helper()
			assert.Equal(t, "linked", resp["link"].(map[string]any)["state"])
			assert.Len(t, fake.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 1)
		}},
		{name: "dry_run_previews_without_kata_writes", hub: true, body: `{"dry_run":true}`, wantStatus: 200, check: func(t *testing.T, fake *katatest.Server, resp map[string]any) {
			t.Helper()
			preview := resp["preview"].(map[string]any)
			assert.Contains(t, preview["title"], "[friction/error] Bash: boom")
			assert.Empty(t, fake.RequestsMatching(http.MethodPost, "/issues"))
		}},
		{name: "pusher_not_hub", hub: false, body: `{}`, wantStatus: 503, wantCode: "kata_unavailable", wantState: "not_hub", check: func(t *testing.T, fake *katatest.Server, _ map[string]any) {
			t.Helper()
			assert.Empty(t, fake.Requests(), "a pusher never contacts Kata")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te, fake, sig := frictionFilingEnv(t, tt.hub)
			fake.ResetRequests()
			w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", tt.body)
			assertStatus(t, w, tt.wantStatus)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, resp["code"])
				assert.Equal(t, tt.wantState, resp["kata_state"])
			}
			if tt.check != nil {
				tt.check(t, fake, resp)
			}
		})
	}
}

func TestFrictionFileRouteUnknownFingerprint(t *testing.T) {
	te, _, _ := frictionFilingEnv(t, true)
	w := te.post(t, "/api/v1/friction/patterns/fl1:nope/file", `{}`)
	assertStatus(t, w, http.StatusNotFound)
}

func TestFrictionFileRouteKataDown(t *testing.T) {
	te, fake, sig := frictionFilingEnv(t, true)
	fake.Close()
	w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`)
	assertStatus(t, w, http.StatusServiceUnavailable)
	assert.Contains(t, w.Body.String(), `"kata_state":"unavailable"`)
}

func TestFrictionLinkUnlinkRoutes(t *testing.T) {
	te, fake, sig := frictionFilingEnv(t, true)
	is := fake.AddIssue(katatest.Issue{Title: "hand filed"})
	w := te.put(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/link", `{"issue_ref":"agentsview#`+is.ShortID+`"}`)
	assertStatus(t, w, http.StatusOK)
	links, err := te.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkSourceManual, links[sig.Fingerprint()].LinkSource)

	w = te.del(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/link")
	assertStatus(t, w, http.StatusOK)
	links, err = te.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestFrictionFilingRoutesRequireLocalhostWithoutAuth(t *testing.T) {
	te, _, sig := frictionFilingEnv(t, true)
	w := te.postFrom(t, "192.0.2.10:5555", "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`)
	assertStatus(t, w, http.StatusForbidden)
}
