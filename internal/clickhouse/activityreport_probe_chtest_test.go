//go:build chtest

package clickhouse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
)

func TestActivitySourceProbeRefreshesCachedLauncherReport(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	store := newTestStore(t, conn, Target{URL: dsn, Database: database})
	store.cursorSecret = []byte("activity-test-secret")
	for _, query := range []string{
		`INSERT INTO sessions (id, project, agent, started_at, ended_at, local_modified_at, message_count, data_version, push_version) VALUES
		 ('parent', 'launcher', 'claude', '2026-06-13 10:00:00', '2026-06-13 10:01:00', '2026-06-13 10:01:00', 1, 1, 1),
		 ('child', 'worker', 'codex', '2026-06-14 10:00:00', '2026-06-14 10:04:00', '2026-06-13 10:01:00', 2, 1, 1)`,
		`INSERT INTO messages (id, session_id, ordinal, role, timestamp, push_version) VALUES
		 (1, 'parent', 0, 'user', '2026-06-13 10:00:00', 1),
		 (2, 'child', 0, 'user', '2026-06-14 10:00:00', 1),
		 (3, 'child', 1, 'assistant', '2026-06-14 10:04:00', 1)`,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	srv := server.New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, store, nil)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(ctx))) })
	handler := srv.Handler()
	get := func(path string, out any) {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:0"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
	}
	probe := func() activity.SourceProbe {
		t.Helper()
		p, err := store.ActivityReportSourceProbe(ctx)
		require.NoError(t, err)
		return p
	}
	var initial activity.Report
	get("/api/v1/activity/report?preset=day&date=2026-06-14&timezone=UTC&project=launcher", &initial)
	require.NotEmpty(t, initial.ReportID)
	assert.Zero(t, initial.InteractivePeak.Agents)
	before := probe()
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (
		'parent' AS parent_session_id, 'subagent' AS relationship_type, true AS is_launched, push_version + 1 AS push_version)
		FROM sessions WHERE id='child'`)
	require.NoError(t, err)
	linked := probe()
	assert.NotEqual(t, before, linked)
	var page struct {
		RefreshRequired bool             `json:"refresh_required"`
		Report          *activity.Report `json:"report"`
	}
	get("/api/v1/activity/report/"+initial.ReportID+"/sessions?include_report=true", &page)
	assert.True(t, page.RefreshRequired)
	require.NotNil(t, page.Report)
	assert.Equal(t, 1, page.Report.InteractivePeak.Agents)
	assert.NotEqual(t, initial.ReportID, page.Report.ReportID)
	linkedID := page.Report.ReportID
	_, err = conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (
		CAST(NULL AS Nullable(String)) AS parent_session_id, '' AS relationship_type, false AS is_launched, push_version + 1 AS push_version)
		FROM sessions WHERE id='child'`)
	require.NoError(t, err)
	cleared := probe()
	assert.NotEqual(t, linked, cleared)
	page.Report = nil
	page.RefreshRequired = false
	get("/api/v1/activity/report/"+linkedID+"/sessions?include_report=true", &page)
	assert.True(t, page.RefreshRequired)
	require.NotNil(t, page.Report)
	assert.Zero(t, page.Report.InteractivePeak.Agents)
	assert.NotEqual(t, linkedID, page.Report.ReportID)
	_, err = conn.ExecContext(ctx, `OPTIMIZE TABLE sessions FINAL`)
	require.NoError(t, err)
	assert.Equal(t, cleared, probe())
}

// Reusing a probe must avoid data scans without hiding replacements or making
// an unchanged report token stale after a background merge.
func TestActivitySourceProbeCachePreservesFreshness(t *testing.T) {
	t.Parallel()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	ctx := t.Context()
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	store := newTestStore(t, conn, Target{URL: dsn, Database: database})
	for _, query := range []string{
		`SYSTEM STOP MERGES messages`,
		`INSERT INTO sessions (id,push_version,data_version,local_modified_at) VALUES ('probe',1,1,'2026-01-10 00:00:00')`,
		`INSERT INTO messages (id,session_id,ordinal,push_version) SELECT number+1,'probe',number,1 FROM numbers(2000)`,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	want := activity.SourceProbe{
		SessionCount: 1, MaxSessionModified: "2026-01-10 00:00:00.000000",
		MaxDataVersion: 1, MaxMessageID: 2000,
	}
	for _, id := range []string{"probe-cold", "probe-warm"} {
		got, err := store.ActivityReportSourceProbe(chdriver.Context(ctx, chdriver.WithQueryID(id)))
		require.NoError(t, err)
		if id == "probe-cold" {
			require.NotEmpty(t, got.SessionLinksFingerprint)
			want.SessionLinksFingerprint = got.SessionLinksFingerprint
		}
		require.Equal(t, want, got)
	}
	_, err := conn.ExecContext(ctx, `SYSTEM FLUSH LOGS query_log`)
	require.NoError(t, err)
	var cold, warm uint64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT sumIf(read_rows,query_id='probe-cold'),sumIf(read_rows,query_id='probe-warm') FROM system.query_log WHERE current_database=currentDatabase() AND type='QueryFinish' AND is_initial_query=1`).Scan(&cold, &warm))
	require.Positive(t, cold)
	require.Less(t, warm, cold/4, "unchanged data must reuse the probe without scanning messages")

	for _, step := range []struct {
		sql  string
		want activity.SourceProbe
	}{
		{`INSERT INTO messages (id,session_id,ordinal,push_version) VALUES (17,'probe',1999,2)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`SYSTEM START MERGES messages`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`OPTIMIZE TABLE messages FINAL`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`DELETE FROM messages WHERE id=1999 SETTINGS lightweight_deletes_sync=2`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1998}},
		{`TRUNCATE TABLE messages`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1}},
		{`INSERT INTO usage_events (id,session_id,push_version) VALUES (42,'probe',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxUsageID: 42}},
		{`INSERT INTO sessions (id,push_version,data_version,local_modified_at) VALUES ('probe',2,2,'2026-01-10 00:00:00')`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42}},
		{`INSERT INTO model_pricing (model_pattern,updated_at,push_version) VALUES ('probe','2026-01-11',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-11 00:00:00.000000"}},
		{`INSERT INTO genai_pricing (singleton,updated_at,push_version) VALUES (1,'2026-01-12',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-12 00:00:00.000000"}},
		{`INSERT INTO sync_metadata (key,value,push_version) VALUES ('agentsview_identity_revision:probe','7',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-12 00:00:00.000000", ProjectIdentityGeneration: 7}},
	} {
		_, err = conn.ExecContext(ctx, step.sql)
		require.NoError(t, err)
		step.want.SessionLinksFingerprint = want.SessionLinksFingerprint
		for range 2 {
			got, probeErr := store.ActivityReportSourceProbe(ctx)
			require.NoError(t, probeErr)
			require.Equal(t, step.want, got, step.sql)
		}
	}
}
