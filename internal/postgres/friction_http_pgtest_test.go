//go:build pgtest

package postgres_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
)

func TestImmediateFrictionEvidenceOnPostgres(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must point to a dedicated test database")
	}
	pg, _ := newPGE2ETestDatabase(t)
	store, err := postgres.NewStore(pgURL, pgE2ESchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx := t.Context()
	sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession,
		SubjectID: "claude:current", Text: "No, edit the configuration first.", Ordinal: new(1)}
	_, err = pg.ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count, user_message_count)
		VALUES ('claude:current', 'm', 'p', 'claude', '2026-10-04T00:30:00Z', 2, 1)`)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `INSERT INTO messages (session_id, ordinal, role, content, content_length)
		VALUES ('claude:current', 0, 'assistant', 'I changed the wrong configuration.', 34),
		       ('claude:current', 1, 'user', 'No, edit the configuration first.', 33)`)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `INSERT INTO friction_findings (session_id, kind, detector, text, title, fingerprint, seq, rules_version, message_ordinal)
		VALUES ($1, 'correction', '', $2, 'Correction', $3, 0, $4, 1)`, sig.SubjectID, sig.Text, sig.Fingerprint(), friction.RulesVersion)
	require.NoError(t, err)
	f := &filing.Filer{Store: store, Archive: store, Location: time.UTC, PublicURL: "https://archive.example.test"}
	stored, run, err := f.SignalForFingerprint(ctx, sig.Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, "2026-10-04", run.Date)
	assert.Empty(t, run.DigestURL)
	plan, err := f.PlanWithContext(ctx, stored, run)
	require.NoError(t, err)
	assert.Contains(t, plan.Body, "I changed the wrong configuration.")
	assert.Contains(t, plan.Body, "Message 1 (user)")
	assert.Contains(t, plan.Body, "/sessions/claude/current?msg=1")
	_, err = pg.ExecContext(ctx, `UPDATE sessions SET deleted_at = NOW() WHERE id = $1`, sig.SubjectID)
	require.NoError(t, err)
	_, _, err = f.SignalForFingerprint(ctx, sig.Fingerprint())
	assert.ErrorIs(t, err, filing.ErrSignalNotFound)
}

func TestFrictionRoutesOnPostgres(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must point to a dedicated test database")
	}
	pg, dataDir := newPGE2ETestDatabase(t)
	store, err := postgres.NewStore(pgURL, pgE2ESchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx := t.Context()
	_, err = pg.ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, first_message, message_count, user_message_count)
		VALUES ('claude:s1', 'm', 'p', 'claude', 'x', 1, 0)`)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `INSERT INTO friction_findings (session_id, kind, detector, tool_name, label, text, evidence, title, fingerprint, seq, rules_version)
		VALUES ('claude:s1', 'error', 'error', 'Bash', '', 'boom', '', 't', 'fl1:x', 0, 'friction-v1')`)
	require.NoError(t, err)
	snap := friction.DigestSnapshot{Date: "2026-09-14", Timezone: "UTC", RulesVersion: friction.RulesVersion, SessionsScanned: 1}
	snapJSON, err := review.EncodeSnapshot(snap)
	require.NoError(t, err)
	require.NoError(t, store.SaveFrictionDigest(ctx, db.FrictionDigest{
		Date: "2026-09-14", Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 1,
		SnapshotJSON: snapJSON, SummaryJSON: friction.RenderSummaryJSON(snap, friction.SummaryMeta{}),
		Markdown: friction.RenderMarkdown(snap, friction.RenderLinks{}), MarkdownSHA256: "s", RunID: "r",
	}, []db.FrictionDigestSubject{{SubjectID: "claude:s1", Date: "2026-09-14", SubjectKind: "session"}},
		[]db.FrictionPatternUpdate{{Fingerprint: "fl1:x", Kind: "error", Title: "t", Date: "2026-09-14", SubjectID: "claude:s1", Occurrences: 1}}))

	handler := server.New(config.Config{Host: "127.0.0.1", DataDir: dataDir}, store, nil).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	tests := []struct {
		path, key string
		count     int
	}{
		{"/api/v1/friction/digests", "digests", 1},
		{"/api/v1/friction/findings?date=2026-09-14", "findings", 1},
		{"/api/v1/friction/patterns", "patterns", 1},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := get(tt.path)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Len(t, body[tt.key], tt.count)
		})
	}
	w := get("/api/v1/friction/digests/2026-09-14")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = get("/api/v1/friction/digests/2026-09-14/md")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "# Friction Log — 2026-09-14")
}
