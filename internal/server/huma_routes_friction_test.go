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

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
)

// Wire mirrors used only to decode responses in tests.
type testFrictionSignal struct {
	Kind           string `json:"kind"`
	SubjectID      string `json:"subject_id"`
	SubjectKind    string `json:"subject_kind"`
	Fingerprint    string `json:"fingerprint"`
	Title          string `json:"title"`
	MessageOrdinal *int   `json:"message_ordinal"`
	SessionURL     string `json:"session_url"`
}

type testFrictionDigest struct {
	Date     string `json:"date"`
	Revision int    `json:"revision"`
	WebURL   string `json:"web_url"`
	Summary  struct {
		SchemaVersion int                 `json:"schema_version"`
		Corrections   int                 `json:"corrections"`
		P0Alerts      map[string][]string `json:"p0_alerts"`
		Personas      map[string]any      `json:"personas"`
		DigestPath    *string             `json:"digest_path"`
	} `json:"summary"`
	Signals  []testFrictionSignal `json:"signals"`
	P0Alerts []struct {
		Tool       string   `json:"tool"`
		SubjectIDs []string `json:"subject_ids"`
	} `json:"p0_alerts"`
}

func frictionTestSnapshot(date string) friction.DigestSnapshot {
	return friction.DigestSnapshot{
		Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		Signals: []friction.Signal{{
			Kind: friction.KindCorrection, SubjectID: "claude:s1", SubjectKind: friction.SubjectSession,
			Detector: "correction.coding", Text: "no, use the other flag", Ordinal: new(4),
		}, {
			Kind: friction.KindError, SubjectID: "run-7:ci_check", SubjectKind: friction.SubjectDiagnostic,
			Detector: "error", ToolName: "ci_check", Text: "run-7:ci_check: nightly check failed",
		}},
		P0Alerts:        map[string][]string{"bash": {"claude:s3", "claude:s1", "claude:s2"}},
		SessionsScanned: 1,
	}
}

func seedFrictionDigest(t *testing.T, d *db.DB, date string) {
	t.Helper()
	snap := frictionTestSnapshot(date)
	snapJSON, err := review.EncodeSnapshot(snap)
	require.NoError(t, err)
	path := "friction:" + date
	summary := friction.RenderSummaryJSON(snap, friction.SummaryMeta{DigestPath: &path})
	md := friction.RenderMarkdown(snap, friction.RenderLinks{})
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1,
		SessionsScanned: 1, SnapshotJSON: snapJSON, SummaryJSON: summary,
		Markdown: md, MarkdownSHA256: "sha", RunID: "run",
	}, []db.FrictionDigestSubject{{SubjectID: "claude:s1", Date: date, SubjectKind: "session"}}, nil))
}

func TestFrictionPatternsRouteCarriesLink(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-09-14", Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("# x\n"), MarkdownSHA256: "s", RunID: "r",
	}, nil, []db.FrictionPatternUpdate{
		{Fingerprint: "fl1:a", Kind: "error", Title: "[friction/error] Bash: boom", Date: "2026-09-14", SubjectID: "claude:s1", Occurrences: 1},
	}))
	require.NoError(t, te.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: "fl1:a", State: db.FrictionLinkStateLinked, IssueUID: "u12",
		QualifiedID: "kata#12", WebURL: "https://kata.example.test/issues/12", LinkSource: db.FrictionLinkSourceManual,
	}))
	w := te.get(t, "/api/v1/friction/patterns")
	assertStatus(t, w, http.StatusOK)
	got := decode[struct {
		Patterns []struct {
			Link *struct {
				State       string `json:"state"`
				QualifiedID string `json:"qualified_id"`
				WebURL      string `json:"web_url"`
			} `json:"link"`
		} `json:"patterns"`
	}](t, w)
	require.Len(t, got.Patterns, 1)
	require.NotNil(t, got.Patterns[0].Link)
	assert.Equal(t, "linked", got.Patterns[0].Link.State)
	assert.Equal(t, "kata#12", got.Patterns[0].Link.QualifiedID)
	assert.Equal(t, "https://kata.example.test/issues/12", got.Patterns[0].Link.WebURL)
}

func TestFrictionDigestRoutes(t *testing.T) {
	te := setup(t, withPublicURL("https://av.example.test/base/"))
	seedFrictionDigest(t, te.db, "2026-09-14")

	t.Run("list", func(t *testing.T) {
		w := te.get(t, "/api/v1/friction/digests?from=2026-09-01&to=2026-09-30")
		assertStatus(t, w, http.StatusOK)
		got := decode[struct {
			Digests []struct {
				Date     string `json:"date"`
				Revision int    `json:"revision"`
			} `json:"digests"`
		}](t, w)
		require.Len(t, got.Digests, 1)
		assert.Equal(t, "2026-09-14", got.Digests[0].Date)
	})
	t.Run("detail", func(t *testing.T) {
		w := te.get(t, "/api/v1/friction/digests/2026-09-14")
		assertStatus(t, w, http.StatusOK)
		got := decode[testFrictionDigest](t, w)
		assert.Equal(t, "https://av.example.test/base/friction/2026-09-14", got.WebURL)
		assert.Equal(t, 3, got.Summary.SchemaVersion)
		assert.Equal(t, 1, got.Summary.Corrections)
		assert.NotNil(t, got.Summary.Personas, "personas is always present (jilog json_output_has_documented_keys)")
		require.Len(t, got.Signals, 2)
		assert.Equal(t, "claude:s1", got.Signals[0].SubjectID)
		assert.Equal(t, "session", got.Signals[0].SubjectKind)
		assert.True(t, strings.HasPrefix(got.Signals[0].Fingerprint, "fl1:"))
		assert.Equal(t, "https://av.example.test/base/sessions/claude/s1?msg=4", got.Signals[0].SessionURL)
		assert.Equal(t, "diagnostic", got.Signals[1].SubjectKind)
		assert.Empty(t, got.Signals[1].SessionURL, "diagnostic identities are not sessions")
		require.Len(t, got.P0Alerts, 1)
		assert.Equal(t, "bash", got.P0Alerts[0].Tool)
		assert.Equal(t, []string{"claude:s1", "claude:s2", "claude:s3"}, got.P0Alerts[0].SubjectIDs)
	})
	t.Run("markdown", func(t *testing.T) {
		w := te.get(t, "/api/v1/friction/digests/2026-09-14/md")
		assertStatus(t, w, http.StatusOK)
		assert.Contains(t, w.Header().Get("Content-Type"), "text/markdown")
		assert.True(t, strings.HasPrefix(w.Body.String(), "---\n"))
		assert.Contains(t, w.Body.String(), "# Friction Log — 2026-09-14")
	})
	t.Run("missing_is_404", func(t *testing.T) {
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-01-01"), http.StatusNotFound)
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-01-01/md"), http.StatusNotFound)
	})
	t.Run("bad_date_is_400", func(t *testing.T) {
		assertStatus(t, te.get(t, "/api/v1/friction/digests/yesterday"), http.StatusBadRequest)
		assertStatus(t, te.get(t, "/api/v1/friction/digests?from=2026-09-30&to=2026-09-01"), http.StatusBadRequest)
	})
	t.Run("corrupt_summary_is_500", func(t *testing.T) {
		require.NoError(t, te.db.SaveFrictionDigest(t.Context(), db.FrictionDigest{
			Date: "2026-09-10", Timezone: "UTC", RulesVersion: friction.RulesVersion,
			BuiltAt: time.Now().UTC(), Revision: 1, SnapshotJSON: []byte("{}"),
			SummaryJSON: []byte("not json"), Markdown: []byte("x"), MarkdownSHA256: "x", RunID: "r",
		}, nil, nil))
		w := te.get(t, "/api/v1/friction/digests/2026-09-10")
		assertStatus(t, w, http.StatusInternalServerError)
		assertBodyContains(t, w, "internal error")
	})
}

func TestFrictionListRoutes(t *testing.T) {
	te := setup(t)
	seedFrictionDigest(t, te.db, "2026-09-14")
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "claude:s1", Project: "p", Machine: "m", Agent: "claude", MessageCount: 1}))
	require.NoError(t, te.db.ReplaceSessionFriction(t.Context(), "claude:s1", []db.FrictionFinding{
		{SessionID: "claude:s1", Kind: "error", Detector: "error", ToolName: "Bash", Title: "t1", Fingerprint: "fl1:x", Seq: 0, RulesVersion: friction.RulesVersion},
		{SessionID: "claude:s1", Kind: "workaround", Detector: "workaround", Label: "for now", Title: "t2", Fingerprint: "fl1:y", Seq: 1, RulesVersion: friction.RulesVersion},
		{SessionID: "claude:s1", Kind: "frustration", Detector: "frustration", Text: "why is this still broken", Title: "t3", Fingerprint: "fl1:z", Seq: 2, RulesVersion: friction.RulesVersion},
	}, nil, friction.RulesVersion, "h"))

	tests := []struct {
		name   string
		path   string
		status int
		count  int
		next   string
	}{
		{"findings_all", "/api/v1/friction/findings", http.StatusOK, 3, ""},
		{"findings_kind", "/api/v1/friction/findings?kind=error", http.StatusOK, 1, ""},
		{"findings_kind_frustration", "/api/v1/friction/findings?kind=frustration", http.StatusOK, 1, ""},
		{"findings_kind_interruption", "/api/v1/friction/findings?kind=interruption", http.StatusOK, 0, ""},
		{"findings_session_and_date", "/api/v1/friction/findings?session_id=claude:s1&date=2026-09-14", http.StatusOK, 3, ""},
		{"findings_page", "/api/v1/friction/findings?limit=1", http.StatusOK, 1, "1"},
		{"findings_bad_kind", "/api/v1/friction/findings?kind=nope", http.StatusBadRequest, 0, ""},
		{"bad_cursor_is_400", "/api/v1/friction/findings?cursor=abc", http.StatusBadRequest, 0, ""},
		{"limit_over_max_is_400", "/api/v1/friction/findings?limit=5000", http.StatusBadRequest, 0, ""},
		{"patterns_empty", "/api/v1/friction/patterns", http.StatusOK, 0, ""},
		{"patterns_bad_link_state", "/api/v1/friction/patterns?link_state=maybe", http.StatusBadRequest, 0, ""},
		{"patterns_bad_since", "/api/v1/friction/patterns?since=last-week", http.StatusBadRequest, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, tt.path)
			assertStatus(t, w, tt.status)
			if tt.status != http.StatusOK {
				return
			}
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			key := "findings"
			if strings.Contains(tt.path, "patterns") {
				key = "patterns"
			}
			items, ok := body[key].([]any)
			require.True(t, ok, "%s must be a non-null array", key)
			assert.Len(t, items, tt.count)
			assert.Equal(t, tt.next, body["next_cursor"])
		})
	}
}

func TestFrictionRunRoute(t *testing.T) {
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	te := setupWithServerOpts(t, []server.Option{server.WithFriction(runner, nil)})
	runner.Store = te.db

	post := func(t *testing.T, body, remote string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/api/v1/friction/run", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if remote != "" {
			req.RemoteAddr = remote
		}
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		return w
	}
	type runResp struct {
		Reports []struct {
			Date    string `json:"date"`
			Written bool   `json:"written"`
			Human   string `json:"human"`
			Summary struct {
				SchemaVersion int     `json:"schema_version"`
				DigestPath    *string `json:"digest_path"`
			} `json:"summary"`
		} `json:"reports"`
	}
	t.Run("remote_without_auth_is_403", func(t *testing.T) {
		assertStatus(t, post(t, `{}`, "203.0.113.9:4000"), http.StatusForbidden)
	})
	t.Run("today_is_400", func(t *testing.T) {
		assertStatus(t, post(t, `{"date":"2026-09-16"}`, ""), http.StatusBadRequest)
	})
	t.Run("malformed_date_is_400", func(t *testing.T) {
		assertStatus(t, post(t, `{"date":"9/14"}`, ""), http.StatusBadRequest)
	})
	t.Run("dry_run_writes_nothing", func(t *testing.T) {
		w := post(t, `{"date":"2026-09-14","dry_run":true}`, "")
		assertStatus(t, w, http.StatusOK)
		got := decode[runResp](t, w)
		require.Len(t, got.Reports, 1)
		assert.False(t, got.Reports[0].Written)
		assert.Nil(t, got.Reports[0].Summary.DigestPath, "dry run digest_path is null (jilog json_dry_run_uses_null_digest_path)")
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-09-14"), http.StatusNotFound)
	})
	t.Run("build_then_read", func(t *testing.T) {
		w := post(t, `{"date":"2026-09-14"}`, "")
		assertStatus(t, w, http.StatusOK)
		got := decode[runResp](t, w)
		require.Len(t, got.Reports, 1)
		assert.True(t, got.Reports[0].Written)
		assert.Equal(t, 3, got.Reports[0].Summary.SchemaVersion)
		assert.Contains(t, got.Reports[0].Human, "session(s) scanned")
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-09-14"), http.StatusOK)
	})
}

func TestFrictionRunRouteDisabled(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/friction/run", `{}`)
	assertStatus(t, w, http.StatusServiceUnavailable)
	assertBodyContains(t, w, "friction review is not enabled")
}

type readOnlyFrictionStore struct{ db.Store }

func (readOnlyFrictionStore) ListFrictionFindings(_ context.Context, _ db.FrictionFindingFilter) ([]db.FrictionFinding, string, error) {
	return nil, "", db.ErrReadOnly
}

func (readOnlyFrictionStore) ListFrictionDigests(_ context.Context, _, _ string) ([]db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func (readOnlyFrictionStore) ListFrictionPatterns(_ context.Context, _ db.FrictionPatternFilter) ([]db.FrictionPattern, string, error) {
	return nil, "", db.ErrReadOnly
}

func (readOnlyFrictionStore) GetFrictionDigest(_ context.Context, _ string) (*db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func TestFrictionRoutesReadOnlyBackends(t *testing.T) {
	te := setup(t)
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}
	handler := server.New(cfg, readOnlyFrictionStore{Store: te.db}, nil).Handler()
	for _, path := range []string{
		"/api/v1/friction/digests",
		"/api/v1/friction/digests/2026-09-14",
		"/api/v1/friction/digests/2026-09-14/md",
		"/api/v1/friction/findings",
		"/api/v1/friction/patterns",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0"+path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusNotImplemented)
		})
	}
}
