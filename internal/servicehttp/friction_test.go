package servicehttp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
)

func markdownSHA(md string) string {
	sum := sha256.Sum256([]byte(md))
	return hex.EncodeToString(sum[:])
}

// TestHTTPFrictionDigestRereadsAcrossARebuild serves Markdown from revision 2
// while the first detail read still describes revision 1.
func TestHTTPFrictionDigestRereadsAcrossARebuild(t *testing.T) {
	tests := []struct {
		name          string
		staleDetails  int
		wantRevision  int
		wantErrSubstr string
	}{
		{name: "rebuild_between_reads_is_reread", staleDetails: 1, wantRevision: 2},
		{name: "digest_that_keeps_changing_fails", staleDetails: 10, wantErrSubstr: "changed while it was read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const md2 = "# Friction Log — 2026-09-14\n\nrebuilt\n"
			details := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/friction/digests/2026-09-14":
					details++
					revision, sha := 2, markdownSHA(md2)
					if details <= tt.staleDetails {
						revision, sha = 1, markdownSHA("# Friction Log — 2026-09-14\n")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"date":"2026-09-14","timezone":"UTC","built_at":"2026-09-15T01:00:00Z","revision":%d,"markdown_sha256":%q,"summary":{}}`, revision, sha)
				case "/api/v1/friction/digests/2026-09-14/md":
					w.Header().Set("Content-Type", "text/markdown")
					_, _ = w.Write([]byte(md2))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(ts.Close)
			fs := NewHTTPBackend(ts.URL, "", false, "").(service.FrictionService)
			view, err := fs.FrictionDigest(t.Context(), "2026-09-14")
			if tt.wantErrSubstr != "" {
				require.ErrorContains(t, err, tt.wantErrSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRevision, view.Revision)
			assert.Equal(t, md2, view.Markdown)
		})
	}
}

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
			_, _ = w.Write([]byte(`{"date":"2026-09-14","timezone":"UTC","rules_version":"friction-v1","built_at":"2026-09-15T01:00:00Z","revision":1,"sessions_scanned":3,"markdown_sha256":"` + markdownSHA("# Friction Log — 2026-09-14\n") + `","summary":` + string(golden) + `,"signals":[],"p0_alerts":[]}`))
		case "/api/v1/friction/digests/2026-09-14/md":
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Friction Log — 2026-09-14\n"))
		case "/api/v1/friction/findings":
			_, _ = w.Write([]byte(`{"findings":[{"session_id":"claude:s1","kind":"error","detector":"error","message_ordinal":9,"call_index":null,"tool_name":"Bash","label":"","text":"boom","evidence":"","title":"t","fingerprint":"fl1:x","occurred_at":null,"seq":0,"rules_version":"friction-v1"}],"next_cursor":"1"}`))
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
	assert.Equal(t, ts.URL+"/friction?date=2026-09-14", view.WebURL)

	_, err = fs.FrictionDigest(t.Context(), "2026-01-01")
	require.ErrorIs(t, err, service.ErrFrictionDigestNotFound)

	digests, err := fs.FrictionDigests(t.Context(), "2026-09-01", "2026-09-30")
	require.NoError(t, err)
	require.Len(t, digests, 1)
	assert.Equal(t, 3, digests[0].SessionsScanned)
	assert.Contains(t, paths, "/api/v1/friction/digests?from=2026-09-01&to=2026-09-30")

	findings, err := fs.FrictionFindings(t.Context(), service.FrictionFindingFilter{Date: "2026-09-14", Kind: "error", Limit: 1})
	require.NoError(t, err)
	require.Len(t, findings.Findings, 1)
	assert.Equal(t, "boom", findings.Findings[0].Text)
	assert.Equal(t, "1", findings.NextCursor)
	assert.Contains(t, paths, "/api/v1/friction/findings?date=2026-09-14&kind=error&limit=1")

	patterns, err := fs.FrictionPatterns(t.Context(), service.FrictionPatternFilter{Kind: "error", Since: "2026-09-01", Limit: 5, Cursor: "5"})
	require.NoError(t, err)
	require.Len(t, patterns.Patterns, 1)
	assert.Equal(t, ts.URL+"/sessions/claude/s1?msg=9", patterns.Patterns[0].LastSessionURL)
	assert.Contains(t, paths, "/api/v1/friction/patterns?cursor=5&kind=error&limit=5&since=2026-09-01")
}

func TestHTTPFrictionCapabilityFollowsServerStore(t *testing.T) {
	tests := []struct {
		name, version string
		want          bool
	}{
		{"friction_store", `{"api_version":14,"data_version":1,"friction_readable":true}`, true},
		{"mirror_without_friction_tables", `{"api_version":14,"data_version":1}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.version))
			}))
			t.Cleanup(ts.Close)
			capabilities, err := ProbeHTTPServerCapabilities(t.Context(), ts.URL, "")
			require.NoError(t, err)
			svc := NewHTTPBackendForServer(ts.URL, "", capabilities)
			assert.Equal(t, tt.want, service.SupportsFriction(svc))
		})
	}
}
