package server_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
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
	Summary  struct {
		SchemaVersion int                 `json:"schema_version"`
		Corrections   int                 `json:"corrections"`
		P0Alerts      map[string][]string `json:"p0_alerts"`
	} `json:"summary"`
	Signals  []testFrictionSignal `json:"signals"`
	P0Alerts []struct {
		Tool       string   `json:"tool"`
		SubjectIDs []string `json:"subject_ids"`
	} `json:"p0_alerts"`
	Patterns []struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"patterns"`
}

func frictionTestSnapshot(date string) review.Snapshot {
	return review.Snapshot{
		Timezone: "UTC", RulesVersion: friction.RulesVersion, Date: date,
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
	summary := friction.RenderSummaryJSON(snap.DigestSnapshot)
	md := friction.RenderMarkdown(snap.DigestSnapshot)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1,
		SessionsScanned: 1, SnapshotJSON: snapJSON, SummaryJSON: summary,
		Markdown: md, MarkdownSHA256: "sha", RunID: "run",
	}, []db.FrictionDigestSubject{{SubjectID: "claude:s1", Date: date, SubjectKind: "session"}},
		frictionTestPatterns(snap.Signals, date)))
}

// frictionTestPatterns records each digest signal's pattern plus one pattern
// from another digest, so reads can tell the two apart.
func frictionTestPatterns(sigs []friction.Signal, date string) []db.FrictionPatternUpdate {
	out := []db.FrictionPatternUpdate{{
		Fingerprint: "fl1:" + strings.Repeat("0", 64), Kind: "error", Title: "elsewhere",
		Date: date, SubjectID: "claude:other", Occurrences: 9,
	}}
	for _, sig := range sigs {
		out = append(out, db.FrictionPatternUpdate{
			Fingerprint: sig.Fingerprint(), Kind: string(sig.Kind), Title: sig.Title(),
			Date: date, SubjectID: sig.SubjectID, Occurrences: 1,
		})
	}
	return out
}

func TestFrictionDigestRoutes(t *testing.T) {
	// public_url is an origin; links add the --base-path prefix.
	te := setupWithServerOpts(t, []server.Option{server.WithBasePath("/base")},
		withPublicURL("https://av.example.test"))
	seedFrictionDigest(t, te.db, "2026-09-14")

	t.Run("list", func(t *testing.T) {
		w := te.get(t, "/base/api/v1/friction/digests?from=2026-09-01&to=2026-09-30")
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
		w := te.get(t, "/base/api/v1/friction/digests/2026-09-14")
		assertStatus(t, w, http.StatusOK)
		got := decode[testFrictionDigest](t, w)
		assert.Equal(t, friction.SummarySchemaVersion, got.Summary.SchemaVersion)
		assert.Equal(t, 1, got.Summary.Corrections)
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
		fingerprints := []string{}
		for _, p := range got.Patterns {
			fingerprints = append(fingerprints, p.Fingerprint)
		}
		assert.ElementsMatch(t, []string{got.Signals[0].Fingerprint, got.Signals[1].Fingerprint}, fingerprints,
			"only this digest's patterns")
	})
	t.Run("markdown", func(t *testing.T) {
		w := te.get(t, "/base/api/v1/friction/digests/2026-09-14/md")
		assertStatus(t, w, http.StatusOK)
		assert.Contains(t, w.Header().Get("Content-Type"), "text/markdown")
		assert.True(t, strings.HasPrefix(w.Body.String(), "---\n"))
		assert.Contains(t, w.Body.String(), "# Friction Log — 2026-09-14")
	})
	t.Run("missing_is_404", func(t *testing.T) {
		assertStatus(t, te.get(t, "/base/api/v1/friction/digests/2026-01-01"), http.StatusNotFound)
		assertStatus(t, te.get(t, "/base/api/v1/friction/digests/2026-01-01/md"), http.StatusNotFound)
	})
	t.Run("bad_date_is_400", func(t *testing.T) {
		assertStatus(t, te.get(t, "/base/api/v1/friction/digests/yesterday"), http.StatusBadRequest)
		assertStatus(t, te.get(t, "/base/api/v1/friction/digests?from=2026-09-30&to=2026-09-01"), http.StatusBadRequest)
	})
	t.Run("corrupt_summary_is_500", func(t *testing.T) {
		require.NoError(t, te.db.SaveFrictionDigest(t.Context(), db.FrictionDigest{
			Date: "2026-09-10", Timezone: "UTC", RulesVersion: friction.RulesVersion,
			BuiltAt: time.Now().UTC(), Revision: 1, SnapshotJSON: []byte("{}"),
			SummaryJSON: []byte("not json"), Markdown: []byte("x"), MarkdownSHA256: "x", RunID: "r",
		}, nil, nil))
		w := te.get(t, "/base/api/v1/friction/digests/2026-09-10")
		assertStatus(t, w, http.StatusInternalServerError)
		assertBodyContains(t, w, "internal error")
	})
}

func TestFrictionListRoutes(t *testing.T) {
	te := setup(t)
	seedFrictionDigest(t, te.db, "2026-09-14")
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "claude:s1", Project: "p", Machine: "m", Agent: "claude", MessageCount: 1}))
	require.NoError(t, te.db.UpdateSessionSignals(t.Context(), "claude:s1", db.SessionSignalUpdate{Friction: &db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Hash: "h", Findings: []db.FrictionFinding{
		{SessionID: "claude:s1", Kind: "error", Detector: "error", ToolName: "Bash", Title: "t1", Fingerprint: "fl1:x", Seq: 0, RulesVersion: friction.RulesVersion},
		{SessionID: "claude:s1", Kind: "workaround", Detector: "workaround", Label: "for now", Title: "t2", Fingerprint: "fl1:y", Seq: 1, RulesVersion: friction.RulesVersion},
		{SessionID: "claude:s1", Kind: "frustration", Detector: "frustration", Text: "why is this still broken", Title: "t3", Fingerprint: "fl1:z", Seq: 2, RulesVersion: friction.RulesVersion},
	}}}))

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
		{"patterns_all", "/api/v1/friction/patterns", http.StatusOK, 3, ""},
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
			Summary struct {
				Date          string `json:"date"`
				SchemaVersion int    `json:"schema_version"`
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
		assert.Equal(t, "2026-09-14", got.Reports[0].Summary.Date)
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-09-14"), http.StatusNotFound)
	})
	t.Run("build_then_read", func(t *testing.T) {
		w := post(t, `{"date":"2026-09-14"}`, "")
		assertStatus(t, w, http.StatusOK)
		got := decode[runResp](t, w)
		require.Len(t, got.Reports, 1)
		assert.True(t, got.Reports[0].Written)
		assert.Equal(t, friction.SummarySchemaVersion, got.Reports[0].Summary.SchemaVersion)
		assertStatus(t, te.get(t, "/api/v1/friction/digests/2026-09-14"), http.StatusOK)
	})
}

func TestFrictionRunRouteDisabled(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/friction/run", `{}`)
	assertStatus(t, w, http.StatusServiceUnavailable)
	assertBodyContains(t, w, "friction review is not enabled")
}

func TestFrictionRoutesOnMirrorWithoutFrictionTables(t *testing.T) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		t.Skip("duckdb-go-bindings does not ship a windows/arm64 library")
	}
	te := setup(t)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := duckdb.Push(t.Context(), path, te.db, "installation-a", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	store, err := duckdb.NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cfg := config.Config{Host: "127.0.0.1"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	for _, path := range []string{
		"/api/v1/friction/digests",
		"/api/v1/friction/digests/2026-09-14",
		"/api/v1/friction/findings",
		"/api/v1/friction/patterns",
	} {
		t.Run(path, func(t *testing.T) {
			w := te.get(t, path)
			assertStatus(t, w, http.StatusNotImplemented)
			assertBodyContains(t, w, "not available from this backend")
		})
	}
}
