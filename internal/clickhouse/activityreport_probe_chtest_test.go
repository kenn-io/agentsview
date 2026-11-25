//go:build chtest

package clickhouse

import (
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

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
		if want.LauncherLinksFingerprint == "" {
			want.LauncherLinksFingerprint = got.LauncherLinksFingerprint
			require.NotEmpty(t, want.LauncherLinksFingerprint)
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
		for range 2 {
			got, probeErr := store.ActivityReportSourceProbe(ctx)
			require.NoError(t, probeErr)
			step.want.LauncherLinksFingerprint = want.LauncherLinksFingerprint
			require.Equal(t, step.want, got, step.sql)
		}
	}
}

func TestActivitySourceProbeTracksLauncherLinksAcrossMerges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	local, err := db.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := newTestStore(t, chtest.Open(t, dsn, database), target)
	store.SetCursorSecret([]byte("test-secret"))
	parent := fixtureSession("probe-parent", "probe-project", "", "2026-06-13T10:00:00Z", 1)
	child := fixtureSession("probe-child", "probe-project", "", "2026-06-14T10:00:00Z", 0)
	child.Agent = "codex"
	child.MessageCount = 2
	child.EndedAt = new("2026-06-14T10:04:00Z")
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: parent, DataVersion: 1, ReplaceMessages: true, Messages: []db.Message{
		{SessionID: parent.ID, Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-13T10:00:00Z"},
	}}, {Session: child, DataVersion: 1, ReplaceMessages: true, Messages: []db.Message{
		{SessionID: child.ID, Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
		{SessionID: child.ID, Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-14T10:04:00Z"},
	}}})
	require.NoError(t, err)
	archive, err := sql.Open("sqlite3", local.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	_, err = archive.ExecContext(ctx, `UPDATE sessions SET local_modified_at = '2035-06-13T10:00:00Z'`)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	initial, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, initial.MaxSessionModified)
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "test"}, store, nil).Handler()
	get := func(path string, dst any) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:0"+path, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), dst))
	}
	var report activity.Report
	get("/api/v1/activity/report?preset=day&date=2026-06-14&timezone=UTC", &report)
	require.NotEmpty(t, report.ReportID)
	require.Equal(t, 1, report.Peak.Agents)
	page := func(id string, refresh bool, peak int) activity.Report {
		t.Helper()
		var result struct {
			RefreshRequired bool             `json:"refresh_required"`
			Report          *activity.Report `json:"report"`
		}
		get("/api/v1/activity/report/"+id+"/sessions?limit=1&include_report=true", &result)
		require.Equal(t, refresh, result.RefreshRequired)
		require.NotNil(t, result.Report)
		require.Equal(t, peak, result.Report.Peak.Agents)
		return *result.Report
	}
	page(report.ReportID, false, 1)
	_, err = local.SetSessionExternalParent(ctx, child.ID, parent.ID)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	linked, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, initial.MaxSessionModified, linked.MaxSessionModified)
	require.Equal(t, initial.PreparedUsageFingerprint, linked.PreparedUsageFingerprint)
	require.NotEqual(t, initial, linked, "adding a link must expire the previous report ID")
	linkedReport := page(report.ReportID, true, 2)
	require.NotEqual(t, report.ReportID, linkedReport.ReportID)
	_, err = syncer.conn.ExecContext(ctx, `OPTIMIZE TABLE sessions FINAL`)
	require.NoError(t, err)
	merged, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, linked, merged, "a merge must leave report IDs valid")
	page(linkedReport.ReportID, false, 2)
	_, err = local.ClearSessionExternalParent(ctx, child.ID)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	cleared, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NotEqual(t, linked, cleared, "clearing a link must expire the previous report ID")
	clearedReport := page(linkedReport.ReportID, true, 1)
	require.NotEqual(t, linkedReport.ReportID, clearedReport.ReportID)
	_, err = syncer.conn.ExecContext(ctx, `OPTIMIZE TABLE sessions FINAL`)
	require.NoError(t, err)
	clearedMerged, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, cleared, clearedMerged, "a merge must leave the cleared report ID valid")
	page(clearedReport.ReportID, false, 1)

	for _, step := range []struct {
		update string
		peak   int
	}{
		{"relationship_type='subagent', parent_session_id='probe-parent'", 1},
		{"parent_session_id=''", 1},
		{"deleted_at='2026-06-15 00:00:00'", 0},
		{"deleted_at=NULL", 1},
	} {
		_, err = syncer.conn.ExecContext(ctx, `ALTER TABLE sessions UPDATE `+step.update+` WHERE id='probe-child' SETTINGS mutations_sync=2`)
		require.NoError(t, err)
		changed, err := store.ActivityReportSourceProbe(ctx)
		require.NoError(t, err)
		require.Equal(t, cleared.MaxSessionModified, changed.MaxSessionModified)
		require.NotEqual(t, cleared, changed)
		next := page(clearedReport.ReportID, true, step.peak)
		require.Equal(t, step.peak, next.SubagentPeak.Agents)
		_, err = syncer.conn.ExecContext(ctx, `OPTIMIZE TABLE sessions FINAL`)
		require.NoError(t, err)
		merged, err := store.ActivityReportSourceProbe(ctx)
		require.NoError(t, err)
		require.Equal(t, changed, merged)
		page(next.ReportID, false, step.peak)
		cleared, clearedReport = changed, next
	}
}
