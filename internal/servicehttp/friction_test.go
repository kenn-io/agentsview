package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
)

func TestHTTPFrictionService(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "friction", "testdata", "golden", "summary.json"))
	require.NoError(t, err)
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/friction/digests":
			_, _ = w.Write([]byte(`{"digests":[{"date":"2026-09-14","timezone":"UTC","rules_version":"friction-v1","built_at":"2026-09-15T01:00:00Z","revision":1,"sessions_scanned":3}]}`))
		case "/api/v1/friction/digests/2026-09-14":
			_, _ = w.Write([]byte(`{"date":"2026-09-14","timezone":"UTC","rules_version":"friction-v1","built_at":"2026-09-15T01:00:00Z","revision":1,"sessions_scanned":3,"markdown_sha256":"s","web_url":"","summary":` + string(golden) + `,"signals":[],"p0_alerts":[]}`))
		case "/api/v1/friction/digests/2026-09-14/md":
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Friction Log — 2026-09-14\n"))
		case "/api/v1/friction/patterns":
			_, _ = w.Write([]byte(`{"patterns":[{"fingerprint":"fl1:x","kind":"error","title":"t","first_seen_date":"2026-09-10","last_seen_date":"2026-09-14","occurrence_count":4,"session_count":3,"last_subject_id":"claude:s1","last_ordinal":9}],"next_cursor":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"friction digest not found"}`))
		}
	}))
	t.Cleanup(ts.Close)

	svc := NewHTTPBackend(ts.URL+"/", "", false, "")
	require.True(t, service.SupportsFriction(svc))
	fs := svc.(service.FrictionService)

	view, err := fs.FrictionDigest(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-14", view.Date)
	assert.Equal(t, string(golden), string(view.Summary), "summary bytes are canonical, not re-encoded")
	assert.Equal(t, "# Friction Log — 2026-09-14\n", view.Markdown)
	assert.Equal(t, ts.URL+"/friction/2026-09-14", view.WebURL)

	_, err = fs.FrictionDigest(t.Context(), "2026-01-01")
	require.ErrorIs(t, err, service.ErrFrictionDigestNotFound)

	linked := false
	patterns, err := fs.FrictionPatterns(t.Context(), service.FrictionPatternFilter{Kind: "error", Since: "2026-09-01", Limit: 5, Linked: &linked})
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.Equal(t, ts.URL+"/sessions/claude/s1?msg=9", patterns[0].LastSessionURL)
	assert.Contains(t, paths, "/api/v1/friction/patterns?kind=error&limit=5&link_state=unlinked&since=2026-09-01")
}

func TestHTTPFrictionCapabilityFollowsAPIVersion(t *testing.T) {
	old := NewHTTPBackendForServer("http://127.0.0.1:1", "", HTTPServerCapabilities{APIVersion: 10})
	assert.False(t, service.SupportsFriction(old))
	current := NewHTTPBackendForServer("http://127.0.0.1:1", "", HTTPServerCapabilities{APIVersion: frictionAPIVersion})
	assert.True(t, service.SupportsFriction(current))
}
