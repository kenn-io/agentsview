//go:build pgtest

package postgres_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func usageHTTP(t *testing.T, h http.Handler, id, query string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+id+"/usage?"+query, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, status, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func TestSessionUsageRawIDHTTPPostgres(t *testing.T) {
	_, dataDir := newPGE2ETestDatabase(t)
	local := dbtest.OpenTestDB(t)
	for _, id := range []string{"codex:native:thread", "codex:child", "archive~codex:hosted", "a~codex:shared", "b~codex:shared", "claude-id", "codex:claude-id", "codex:codex:missing", "codex:freebuff:missing"} {
		s := db.Session{ID: id, Agent: "codex", Project: "project", Machine: "test", MessageCount: 1, TotalOutputTokens: 500, HasTotalOutputTokens: true}
		if id == "codex:child" {
			s.ParentSessionID = new("codex:native:thread")
			s.RelationshipType = "subagent"
		}
		require.NoError(t, local.UpsertSession(t.Context(), s))
		require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Model: "unknown-model", Timestamp: "2026-01-01T00:00:00Z", TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)}}))
	}
	pgURL := os.Getenv("TEST_PG_URL")
	syncer, err := postgres.New(pgURL, pgE2ESchema, local, "test", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(t.Context()))
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := postgres.NewStore(pgURL, pgE2ESchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.SetCustomPricing(map[string]config.CustomModelRate{
		"unknown-model": {InputMicrodollarsPerMTok: 3000000, OutputMicrodollarsPerMTok: 15000000},
	})
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: dataDir}, store, nil).Handler()
	for _, tc := range []struct {
		query       string
		rows, count int
		rollup      bool
	}{
		{"", 0, 1, false}, {"breakdown=true", 1, 1, false}, {"subagents=true", 0, 2, false}, {"subagents=true&breakdown=true", 2, 2, false},
		{"rollup=true", 0, 1, true}, {"rollup=true&breakdown=true", 1, 1, true}, {"rollup=true&subagents=true&breakdown=true", 1, 1, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := usageHTTP(t, h, "native:thread", tc.query, 200)
			assert.Equal(t, "codex:native:thread", got["session_id"])
			assert.Equal(t, true, got["has_token_data"])
			assert.InDelta(t, 500*tc.count, got["total_output_tokens"], 0)
			assert.Equal(t, map[string]any{"microdollars": float64(10500 * tc.count)}, got["cost"])
			assert.InDelta(t, tc.count, got["breakdown_count"], 0)
			assert.Len(t, got["breakdown"], tc.rows)
			if tc.rollup {
				assert.InDelta(t, 1, got["rollup_subagent_count"], 0)
				assert.NotContains(t, got, "subagent_count")
			}
		})
	}
	assert.Equal(t, "archive~codex:hosted", usageHTTP(t, h, "hosted", "", 200)["session_id"])
	assert.Equal(t, "claude-id", usageHTTP(t, h, "claude-id", "", 200)["session_id"])
	got := usageHTTP(t, h, "shared", "", 409)["error"].(map[string]any)
	assert.Equal(t, "ambiguous_session_id", got["code"])
	assert.ElementsMatch(t, []any{"a~codex:shared", "b~codex:shared"}, got["candidates"])
	for _, id := range []string{"absent", "codex:missing", "freebuff:missing", "other~hosted"} {
		assert.Equal(t, "session_not_found", usageHTTP(t, h, id, "", 404)["error"].(map[string]any)["code"])
	}
}

func TestHostedSessionUsageRawIDHTTP(t *testing.T) {
	store, split, _ := postgres.HostedPublicFixture(t)
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil).Handler()
	assert.Equal(t, "codex:portable", usageHTTP(t, h, "portable", "", 200)["session_id"])
	usageHTTP(t, h, "table", "", 404)
	pg := store.(*postgres.HostedStore).DB()
	_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES
 ('codex:legacy-portable','project','test','codex'),
 ('legacy-bare','project','test','claude'),
 ('archive~legacy-hosted','project','test','claude')`)
	require.NoError(t, err)
	assert.Equal(t, "codex:legacy-portable", usageHTTP(t, h, "legacy-portable", "", 200)["session_id"])
	assert.Equal(t, "legacy-bare", usageHTTP(t, h, "legacy-bare", "", 200)["session_id"])
	assert.Equal(t, "archive~legacy-hosted", usageHTTP(t, h, "legacy-hosted", "", 200)["session_id"])
	usageHTTP(t, h, "acy-portable", "", 404)
	split()
	got := usageHTTP(t, h, "portable", "", 409)["error"].(map[string]any)
	assert.Equal(t, "ambiguous_session_id", got["code"])
	candidates := got["candidates"].([]any)
	require.Len(t, candidates, 2)
	for _, candidate := range candidates {
		id := candidate.(string)
		assert.Contains(t, id, "codex:portable~")
		assert.Equal(t, id, usageHTTP(t, h, id, "", 200)["session_id"])
	}
}

var hostedUsageQueries = []string{
	"", "breakdown=true", "subagents=true", "subagents=true&breakdown=true",
	"rollup=true", "rollup=true&breakdown=true", "rollup=true&subagents=true", "rollup=true&subagents=true&breakdown=true",
}

func TestHostedSessionUsageClaudeAmbiguityHTTP(t *testing.T) {
	const rawID = "00000000-0000-4000-8000-000000000001"
	store := postgres.HostedClaudeUsageFixture(t, rawID, 2)
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil).Handler()
	candidates := usageHTTP(t, h, rawID, "", 409)["error"].(map[string]any)["candidates"].([]any)
	require.Len(t, candidates, 2)
	for _, query := range hostedUsageQueries {
		t.Run(query, func(t *testing.T) {
			got := usageHTTP(t, h, rawID, query, 409)["error"].(map[string]any)
			assert.Equal(t, "ambiguous_session_id", got["code"])
			assert.ElementsMatch(t, candidates, got["candidates"])
			for _, candidate := range candidates {
				id := candidate.(string)
				assert.Contains(t, id, rawID+"~")
				retry := usageHTTP(t, h, id, query, 200)
				assert.Equal(t, id, retry["session_id"])
				assert.Equal(t, "claude", retry["agent"])
			}
		})
	}
	trashed, survivor := candidates[0].(string), candidates[1].(string)
	require.NoError(t, store.SoftDeleteSession(t.Context(), trashed))
	for _, query := range hostedUsageQueries {
		t.Run("one live/"+query, func(t *testing.T) {
			usageHTTP(t, h, trashed, query, 404)
			assert.Equal(t, usageHTTP(t, h, survivor, query, 200), usageHTTP(t, h, rawID, query, 200))
		})
	}
	require.NoError(t, store.SoftDeleteSession(t.Context(), survivor))
	for _, query := range hostedUsageQueries {
		t.Run("none live/"+query, func(t *testing.T) {
			usageHTTP(t, h, survivor, query, 404)
			assert.Equal(t, "session_not_found", usageHTTP(t, h, rawID, query, 404)["error"].(map[string]any)["code"])
		})
	}
}

func TestHostedSessionUsageDeletedLegacyHTTP(t *testing.T) {
	store, _, _ := postgres.HostedPublicFixture(t)
	pg := store.(*postgres.HostedStore).DB()
	_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,deleted_at) VALUES
 ('a~codex:portable','project','test','codex',NOW()),
 ('b~codex:portable','project','test','codex',NOW()),
 ('c~codex:portable','project','test','codex',NOW()),
 ('d~codex:portable','project','test','codex',NOW()),
 ('e~codex:portable','project','test','codex',NOW()),
 ('archive~codex:deleted-only','project','test','codex',NOW())`)
	require.NoError(t, err)
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil).Handler()
	t.Run("visible match survives limit", func(t *testing.T) {
		assert.Equal(t, "codex:portable", usageHTTP(t, h, "portable", "", 200)["session_id"])
	})
	t.Run("deleted only", func(t *testing.T) {
		got := usageHTTP(t, h, "deleted-only", "", 404)["error"].(map[string]any)
		assert.Equal(t, "session_not_found", got["code"])
	})
}

func TestHostedSessionUsageDeletedProjectionHTTP(t *testing.T) {
	store, _, _ := postgres.HostedPublicFixture(t)
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil).Handler()
	assert.Equal(t, "codex:portable", usageHTTP(t, h, "portable", "", 200)["session_id"])
	_, err := store.(*postgres.HostedStore).DB().ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES ('archive~codex:portable','project','test','codex')`)
	require.NoError(t, err)
	usageHTTP(t, h, "portable", "", 409)
	require.NoError(t, store.SoftDeleteSession(t.Context(), "codex:portable"))
	for _, query := range hostedUsageQueries {
		t.Run(query, func(t *testing.T) {
			usageHTTP(t, h, "codex:portable", query, 404)
			assert.Equal(t, usageHTTP(t, h, "archive~codex:portable", query, 200), usageHTTP(t, h, "portable", query, 200))
		})
	}
	require.NoError(t, store.SoftDeleteSession(t.Context(), "archive~codex:portable"))
	for _, query := range hostedUsageQueries {
		usageHTTP(t, h, "portable", query, 404)
	}
}

func TestHostedSessionUsageDeletedProjectionsBeforeLimitHTTP(t *testing.T) {
	const rawID = "00000000-0000-4000-8000-000000000001"
	store := postgres.HostedClaudeUsageFixture(t, rawID, 7)
	ids, err := store.FindSessionIDsByRawSuffix(t.Context(), rawID, 100)
	require.NoError(t, err)
	require.Len(t, ids, 7)
	for _, id := range ids {
		require.NoError(t, store.SoftDeleteSession(t.Context(), id))
	}
	// Projected physical rows sort before this live legacy row. Deleted rows must
	// be filtered in SQL before the five-candidate limit, not after the query.
	_, err = store.(*postgres.HostedStore).DB().ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES ('z~00000000-0000-4000-8000-000000000001','project','test','claude')`)
	require.NoError(t, err)
	h := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil).Handler()
	assert.Equal(t, "z~00000000-0000-4000-8000-000000000001", usageHTTP(t, h, rawID, "", 200)["session_id"])
}
