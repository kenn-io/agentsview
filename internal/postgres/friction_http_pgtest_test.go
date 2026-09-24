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
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
)

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
