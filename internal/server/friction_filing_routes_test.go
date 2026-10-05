// internal/server/friction_filing_routes_test.go
package server_test

import (
	"context"
	"encoding/json/v2"
	"errors"
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

func frictionFilingEnv(
	t *testing.T, hub bool, rerender ...func(context.Context, string) error,
) (*testEnv, *katatest.Server, friction.Signal, *filing.Filer) {
	t.Helper()
	return frictionFilingEnvWithExclusive(t, hub, nil, rerender...)
}

func frictionFilingEnvWithExclusive(
	t *testing.T,
	hub bool,
	exclusive func(func() error) error,
	rerender ...func(context.Context, string) error,
) (*testEnv, *katatest.Server, friction.Signal, *filing.Filer) {
	t.Helper()
	fake := katatest.New(t)
	sig := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", ToolName: "Bash", Text: "boom", Ordinal: new(1)}
	conn := kata.NewConn(kata.Config{Enabled: true, Hub: hub, Endpoint: fake.Endpoint(), Project: "agentsview"})
	opts := []server.Option{server.WithKataConn(conn)}
	if exclusive != nil {
		opts = append(opts, server.WithFriction(nil, exclusive))
	}
	var te *testEnv
	var f *filing.Filer
	if hub {
		f = &filing.Filer{
			Kata: conn, Now: time.Now, Policy: filing.Policy{Kinds: filing.DefaultKinds()},
			Snapshot: func(context.Context, string) (friction.DigestSnapshot, error) {
				return friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}, nil
			},
		}
		if len(rerender) > 0 {
			f.Rerender = rerender[0]
		}
		opts = append(opts, server.WithFrictionFiler(f))
		te = setupWithServerOpts(t, opts)
		f.Store = fixedDates{LinkStore: te.db, dates: []string{"2026-09-20"}}
	} else {
		te = setupWithServerOpts(t, opts)
	}
	return te, fake, sig, f
}

func TestFrictionFileRouteRerendersOnlyWhenLinkChanges(t *testing.T) {
	var dates []string
	te, _, sig, _ := frictionFilingEnv(t, true, func(_ context.Context, date string) error {
		dates = append(dates, date)
		return nil
	})
	path := "/api/v1/friction/patterns/" + sig.Fingerprint() + "/file"
	assertStatus(t, te.post(t, path, `{}`), http.StatusOK)
	assertStatus(t, te.post(t, path, `{}`), http.StatusOK)
	assert.Equal(t, []string{"2026-09-20"}, dates)
}

type fixedDates struct {
	filing.LinkStore
	dates []string
}

func (f fixedDates) DigestDatesForFingerprints(_ context.Context, fingerprints []string) (map[string][]string, error) {
	out := make(map[string][]string, len(fingerprints))
	for _, fingerprint := range fingerprints {
		out[fingerprint] = f.dates
	}
	return out, nil
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
			te, fake, sig, _ := frictionFilingEnv(t, tt.hub)
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

func TestFrictionFilingRoutesExplainMissingReviewFiler(t *testing.T) {
	fake := katatest.New(t)
	conn := kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: fake.Endpoint(), Project: "agentsview",
	})
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(conn)})
	fingerprint := (friction.Signal{
		Kind: friction.KindError, SubjectKind: friction.SubjectSession,
		SubjectID: "claude:s1", ToolName: "Bash", Text: "boom", Ordinal: new(1),
	}).Fingerprint()
	base := "/api/v1/friction/patterns/" + fingerprint
	tests := []struct {
		name, method, path, body string
	}{
		{name: "file", method: http.MethodPost, path: base + "/file", body: `{}`},
		{name: "link", method: http.MethodPut, path: base + "/link", body: `{"issue_ref":"agentsview#f001"}`},
		{name: "unlink", method: http.MethodDelete, path: base + "/link"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w *httptest.ResponseRecorder
			switch tt.method {
			case http.MethodPost:
				w = te.post(t, tt.path, tt.body)
			case http.MethodPut:
				w = te.put(t, tt.path, tt.body)
			case http.MethodDelete:
				w = te.del(t, tt.path)
			}
			assertStatus(t, w, http.StatusServiceUnavailable)
			var response map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, "kata_unavailable", response["code"])
			assert.Equal(t, "ready", response["kata_state"])
			assert.Contains(t, response["error"], "Friction filing is unavailable")
			assert.NotContains(t, response["error"], "Kata is ready")
		})
	}
	assert.Empty(t, fake.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"))
}

func TestFrictionFileRouteUsesRequestedDigestSnapshot(t *testing.T) {
	te, fake, sig, f := frictionFilingEnv(t, true)
	old := sig
	old.Evidence = "old digest evidence"
	newest := sig
	newest.Evidence = "newest digest evidence"
	require.Equal(t, old.Fingerprint(), newest.Fingerprint())
	f.Store = fixedDates{LinkStore: te.db, dates: []string{"2026-09-20", "2026-09-21"}}
	f.Snapshot = func(_ context.Context, date string) (friction.DigestSnapshot, error) {
		if date == "2026-09-20" {
			return friction.DigestSnapshot{Date: date, Signals: []friction.Signal{old}}, nil
		}
		return friction.DigestSnapshot{Date: date, Signals: []friction.Signal{newest}}, nil
	}

	w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{"date":"2026-09-20"}`)
	assertStatus(t, w, http.StatusOK)
	posts := fake.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues")
	require.Len(t, posts, 1)
	assert.Contains(t, string(posts[0].Body), "old digest evidence")
	assert.NotContains(t, string(posts[0].Body), "newest digest evidence")
}

func TestFrictionFileRouteUnknownFingerprint(t *testing.T) {
	te, _, _, _ := frictionFilingEnv(t, true)
	w := te.post(t, "/api/v1/friction/patterns/fl1:nope/file", `{}`)
	assertStatus(t, w, http.StatusNotFound)
	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "signal_not_found", response["code"])
}

func TestFrictionFileRouteKataDown(t *testing.T) {
	te, fake, sig, _ := frictionFilingEnv(t, true)
	fake.Close()
	w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`)
	assertStatus(t, w, http.StatusServiceUnavailable)
	assert.Contains(t, w.Body.String(), `"kata_state":"unavailable"`)
}

func TestFrictionLinkUnlinkRoutes(t *testing.T) {
	te, fake, sig, _ := frictionFilingEnv(t, true)
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
	te, _, sig, _ := frictionFilingEnv(t, true)
	w := te.postFrom(t, "192.0.2.10:5555", "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`)
	assertStatus(t, w, http.StatusForbidden)
}

type failingLinkedUpsert struct {
	filing.LinkStore
	err error
}

func (s failingLinkedUpsert) UpsertFrictionIssueLink(ctx context.Context, row db.FrictionIssueLink) error {
	if row.State == db.FrictionLinkStateLinked {
		return s.err
	}
	return s.LinkStore.UpsertFrictionIssueLink(ctx, row)
}

func TestFrictionFileRoutePropagatesLinkPersistenceFailure(t *testing.T) {
	te, fake, sig, f := frictionFilingEnv(t, true)
	f.Store = failingLinkedUpsert{LinkStore: f.Store, err: errors.New("disk unavailable")}

	w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`)
	assertStatus(t, w, http.StatusInternalServerError)
	assert.Len(t, fake.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 1)
}

func TestFrictionFilingMutationsUseExclusive(t *testing.T) {
	var exclusiveCalls int
	te, fake, sig, _ := frictionFilingEnvWithExclusive(t, true, func(fn func() error) error {
		exclusiveCalls++
		return fn()
	})
	is := fake.AddIssue(katatest.Issue{Title: "manually linked"})

	assertStatus(t, te.put(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/link", `{"issue_ref":"agentsview#`+is.ShortID+`"}`), http.StatusOK)
	assertStatus(t, te.del(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/link"), http.StatusOK)
	assertStatus(t, te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", `{}`), http.StatusOK)
	assert.Equal(t, 3, exclusiveCalls)
}
