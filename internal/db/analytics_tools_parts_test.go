//go:build fts5

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func setToolsPartsReadObserver(observe func([]string)) func() {
	toolsPartsReadObserver = observe
	return func() { toolsPartsReadObserver = nil }
}

func TestToolsPartsDirtyOncePerRefresh(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	f := AnalyticsFilter{Timezone: "UTC"}
	first, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, first.TotalCalls)
	_, err = d.getWriter().ExecContext(ctx, `CREATE TABLE tools_update_count(n INTEGER NOT NULL); INSERT INTO tools_update_count VALUES(0); CREATE TRIGGER count_tools_update AFTER UPDATE OF tools_generation ON sessions BEGIN UPDATE tools_update_count SET n=n+1; END`)
	require.NoError(t, err)
	count := func() int {
		var n int
		require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT n FROM tools_update_count`).Scan(&n))
		return n
	}
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET result_content='stored' WHERE session_id='a'; UPDATE tool_calls SET result_content=NULL WHERE session_id='a'`)
	require.NoError(t, err)
	require.Zero(t, count())
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Write' WHERE session_id='a'; UPDATE tool_calls SET observed_outcome='empty' WHERE session_id='a'; UPDATE tool_calls SET observed_repeat='identical' WHERE session_id='a'`)
	require.NoError(t, err)
	require.Equal(t, 1, count())
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Write", got.ByTool[0].ToolName)
	require.Equal(t, 2, count())
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM tool_calls WHERE session_id='a'`)
	require.NoError(t, err)
	require.Equal(t, 3, count())
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Zero(t, got.TotalCalls)
}

func TestToolsPartsRefreshOnlyChangedSession(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		insertSession(t, d, id, "project")
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
	}
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Write' WHERE session_id='a'`)
	require.NoError(t, err)
	var queries []string
	analyticsQueryObserver = func(q string) { queries = append(queries, q) }
	t.Cleanup(func() { analyticsQueryObserver = nil })
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, queries, 1)
	require.Contains(t, queries[0], "IN (?)")
	analyticsQueryObserver = nil
	rows, err := d.analyticsToolRows(ctx, f)
	require.NoError(t, err)
	require.Equal(t, BuildToolsAnalytics(rows), got)
}

func TestToolsPartsFiltersAndChanges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		insertSession(t, d, id, "project", func(s *Session) { s.QualitySignalVersion = CurrentQualitySignalVersion })
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.Model = "model-a"
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
		updateSignals(t, d, id, SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	}
	_, err := d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET observed_outcome='empty'`)
	require.NoError(t, err)
	hour, monday := 8, 0
	filters := []AnalyticsFilter{
		{Timezone: "UTC"},
		{Timezone: "UTC", Model: "model-a"},
		{Timezone: "America/New_York", Hour: &hour, DayOfWeek: &monday},
		{Timezone: "UTC", From: "2025-03-10", To: "2025-03-10"},
		{Timezone: "UTC", ToolCategory: "Read", ToolMetric: "tool_empty_rate"},
		{Timezone: "UTC", Project: "project"},
	}
	for _, f := range filters {
		_, err = d.analyticsTools(ctx, f)
		require.NoError(t, err)
	}
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET observed_outcome='content' WHERE session_id='a'; DELETE FROM tool_calls WHERE session_id='b'; UPDATE sessions SET deleted_at='2025-03-11' WHERE id='c'`)
	require.NoError(t, err)
	for _, f := range filters {
		got, err := d.analyticsTools(ctx, f)
		require.NoError(t, err)
		rows, err := d.analyticsToolRows(ctx, f)
		require.NoError(t, err)
		require.Equal(t, BuildToolsAnalytics(rows), got, "%+v", f)
	}
	got, err := d.analyticsTools(ctx, AnalyticsFilter{Timezone: "UTC", ToolMetric: "tool_empty_rate"})
	require.NoError(t, err)
	require.Zero(t, got.TotalCalls)
}

func TestToolsPartsConcurrentChangeFallsBackWithoutSaving(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "project")
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
	}
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Write' WHERE session_id='a'`)
	require.NoError(t, err)
	var writeErr error
	reset := setToolsPartsReadObserver(func([]string) {
		_, writeErr = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Write' WHERE session_id='b'`)
	})
	got, err := d.analyticsTools(ctx, f)
	reset()
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, 2, got.TotalCalls)
	require.Len(t, got.ByTool, 1)
	var reads [][]string
	reset = setToolsPartsReadObserver(func(ids []string) { reads = append(reads, append([]string(nil), ids...)) })
	defer reset()
	_, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"a", "b"}}, reads)
}

func TestToolsPartsAppendAndProjectMove(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		insertSession(t, d, id, "project")
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
	}
	f := AnalyticsFilter{Timezone: "UTC", Project: "project"}
	first, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 3, first.TotalCalls)
	msg := asstMsgAt("a", 1, "call", "2025-03-10T13:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Write", Category: "Write"}}
	insertMessages(t, d, msg)
	var reads [][]string
	reset := setToolsPartsReadObserver(func(ids []string) { reads = append(reads, append([]string(nil), ids...)) })
	defer reset()
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 4, got.TotalCalls)
	require.Equal(t, [][]string{{"a"}}, reads)
	appendedRows, err := d.analyticsToolRows(ctx, f)
	require.NoError(t, err)
	require.Equal(t, BuildToolsAnalytics(appendedRows), got)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET project='elsewhere' WHERE id='b'`)
	require.NoError(t, err)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 3, got.TotalCalls)
	rows, err := d.analyticsToolRows(ctx, f)
	require.NoError(t, err)
	require.Equal(t, BuildToolsAnalytics(rows), got)
}

func TestToolsPartsFreshArchive(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testFreshDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	f := AnalyticsFilter{Timezone: "UTC"}
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, got.TotalCalls)
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM tool_calls WHERE session_id='a'`)
	require.NoError(t, err)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Zero(t, got.TotalCalls)
}

func TestToolsPartsRefreshOutcomeAndDeletion(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c"} {
		insertSession(t, d, id, "project", func(s *Session) { s.QualitySignalVersion = CurrentQualitySignalVersion })
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
		updateSignals(t, d, id, SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	}
	_, err := d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET observed_outcome='empty'`)
	require.NoError(t, err)
	f := AnalyticsFilter{Timezone: "UTC", ToolMetric: "tool_empty_rate"}
	initial, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 3, initial.TotalCalls)
	var reads [][]string
	reset := setToolsPartsReadObserver(func(ids []string) { reads = append(reads, append([]string(nil), ids...)) })
	defer reset()
	for i, change := range []struct {
		query, id string
		total     int
	}{
		{`UPDATE tool_calls SET observed_outcome='content' WHERE session_id='a'`, "a", 2},
		{`DELETE FROM tool_calls WHERE session_id='b'`, "b", 1},
	} {
		_, err := d.getWriter().ExecContext(ctx, change.query)
		require.NoError(t, err)
		got, err := d.analyticsTools(ctx, f)
		require.NoError(t, err)
		require.Equal(t, change.total, got.TotalCalls)
		require.Len(t, reads, i+1)
		require.Equal(t, []string{change.id}, reads[i])
	}
	require.Len(t, reads, 2)
}

func TestToolsPartsFingerprintSeparatesSessionFields(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "c", func(s *Session) { s.Agent = "a|b" })
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	f := AnalyticsFilter{Timezone: "UTC"}
	first, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, first.ByAgent, 1)
	require.Equal(t, "a|b", first.ByAgent[0].Agent)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET agent='a', project='b|c' WHERE id='a'`)
	require.NoError(t, err)
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByAgent, 1)
	require.Equal(t, "a", got.ByAgent[0].Agent)
	rows, err := d.analyticsToolRows(ctx, f)
	require.NoError(t, err)
	require.Equal(t, BuildToolsAnalytics(rows), got)
}

func TestToolsPartsIdentityFailureUsesDirectReader(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	_, err := d.getWriter().ExecContext(ctx, `DELETE FROM archive_metadata WHERE key='database_id'`)
	require.NoError(t, err)
	got, err := d.analyticsTools(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.Equal(t, 1, got.TotalCalls)
}

func TestToolsPartsMessageMetadataChanges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.Model = "model-a"
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	hour := 12
	f := AnalyticsFilter{Timezone: "UTC", Model: "model-a", Hour: &hour}
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, got.TotalCalls)
	for _, change := range []struct {
		query string
		total int
	}{
		{`UPDATE messages SET model='model-b' WHERE session_id='a'`, 0},
		{`UPDATE messages SET model='model-a' WHERE session_id='a'`, 1},
		{`UPDATE messages SET timestamp='2025-03-10T13:00:00Z' WHERE session_id='a'`, 0},
	} {
		_, err := d.getWriter().ExecContext(ctx, change.query)
		require.NoError(t, err)
		got, err := d.analyticsTools(ctx, f)
		require.NoError(t, err)
		require.Equal(t, change.total, got.TotalCalls)
		rows, err := d.analyticsToolRows(ctx, f)
		require.NoError(t, err)
		require.Equal(t, BuildToolsAnalytics(rows), got)
	}
}

func TestToolsPartsMigrationInvalidatesOldParts(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := openCopiedTestDB(t, path)
	require.NoError(t, err)
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET tools_generation=NULL WHERE id='a'`)
	require.NoError(t, err)
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, before.ByTool, 1)
	require.Equal(t, "Read", before.ByTool[0].ToolName)
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER sessions_tools_generation_insert; DROP TRIGGER tool_calls_tools_generation_insert; DROP TRIGGER tool_calls_tools_generation_update; DROP TRIGGER tool_calls_tools_generation_delete; ALTER TABLE sessions DROP COLUMN tools_generation; UPDATE tool_calls SET tool_name='Write' WHERE session_id='a'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	_, err = d.getWriter().ExecContext(ctx, `CREATE TABLE tools_update_count(n INTEGER NOT NULL); INSERT INTO tools_update_count VALUES(0); CREATE TRIGGER count_tools_update AFTER UPDATE OF tools_generation ON sessions BEGIN UPDATE tools_update_count SET n=n+1; END`)
	require.NoError(t, err)
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Write", got.ByTool[0].ToolName)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT n FROM tools_update_count`).Scan(&count))
	require.Zero(t, count)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Grep' WHERE session_id='a'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = Open(ctx, path)
	require.NoError(t, err)
	var generation *string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT tools_generation FROM sessions WHERE id='a'`).Scan(&generation))
	require.Nil(t, generation)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Grep", got.ByTool[0].ToolName)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT n FROM tools_update_count`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestToolsPartsFallbackFilters(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	var reads int
	reset := setToolsPartsReadObserver(func([]string) { reads++ })
	defer reset()
	for _, f := range []AnalyticsFilter{{Timezone: "Local"}, {Timezone: "UTC", ActiveSince: "2025-03-01T00:00:00Z"}, {Timezone: "UTC", Termination: "active"}} {
		got, err := d.analyticsTools(ctx, f)
		require.NoError(t, err)
		rows, err := d.analyticsToolRows(ctx, f)
		require.NoError(t, err)
		require.Equal(t, BuildToolsAnalytics(rows), got)
	}
	require.Zero(t, reads)
}

func TestToolsPartsFreshMessageMetadata(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testFreshDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.Model = "model-a"
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	hour := 12
	f := AnalyticsFilter{Timezone: "UTC", Model: "model-a", Hour: &hour}
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, got.TotalCalls)
	second := asstMsgAt("a", 1, "call", "2025-03-10T12:30:00Z")
	second.Model = "model-a"
	second.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Write", Category: "Write"}}
	insertMessages(t, d, second)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 2, got.TotalCalls)
	for _, change := range []struct {
		query string
		total int
	}{
		{`UPDATE messages SET model='model-b' WHERE session_id='a'`, 0},
		{`UPDATE messages SET model='model-a' WHERE session_id='a'`, 2},
		{`UPDATE messages SET timestamp='2025-03-10T13:00:00Z' WHERE session_id='a'`, 0},
	} {
		_, err := d.getWriter().ExecContext(ctx, change.query)
		require.NoError(t, err)
		got, err := d.analyticsTools(ctx, f)
		require.NoError(t, err)
		require.Equal(t, change.total, got.TotalCalls)
		rows, err := d.analyticsToolRows(ctx, f)
		require.NoError(t, err)
		require.Equal(t, BuildToolsAnalytics(rows), got)
	}
}

func TestToolsPartsRestoredBackupDiverges(t *testing.T) {
	for _, dirtyBackup := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirtyBackup), func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "archive.db")
			backup := filepath.Join(dir, "backup.db")
			d, err := openCopiedTestDB(t, path)
			require.NoError(t, err)
			insertSession(t, d, "a", "project")
			msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
			msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
			insertMessages(t, d, msg)
			f := AnalyticsFilter{Timezone: "UTC"}
			_, err = d.analyticsTools(ctx, f)
			require.NoError(t, err)
			if dirtyBackup {
				_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET observed_outcome='empty' WHERE id=1`)
				require.NoError(t, err)
			}
			_, err = d.getWriter().ExecContext(ctx, `VACUUM INTO ?`, backup)
			require.NoError(t, err)
			insert := func(name string) {
				_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id,session_id,tool_name,category) SELECT id,session_id,?,? FROM messages WHERE session_id='a'`, name, name)
				require.NoError(t, err)
				var id int
				require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT MAX(id) FROM tool_calls`).Scan(&id))
				require.Equal(t, 2, id)
			}
			insert("Bash")
			first, err := d.analyticsTools(ctx, f)
			require.NoError(t, err)
			require.Equal(t, 2, first.TotalCalls)
			require.NoError(t, d.Close())
			bytes, err := os.ReadFile(backup)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, bytes, 0o600))
			d, err = Open(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			insert("Grep")
			got, err := d.analyticsTools(ctx, f)
			require.NoError(t, err)
			require.Equal(t, 2, got.TotalCalls)
			require.Len(t, got.ByTool, 2)
			names := []string{got.ByTool[0].ToolName, got.ByTool[1].ToolName}
			require.ElementsMatch(t, []string{"Read", "Grep"}, names)
			rows, err := d.analyticsToolRows(ctx, f)
			require.NoError(t, err)
			require.Equal(t, BuildToolsAnalytics(rows), got)
		})
	}
}

func TestToolsPartsConcurrentFilters(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.Model = "model-a"
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	filters := []AnalyticsFilter{{Timezone: "UTC", Model: "model-a"}, {Timezone: "UTC", ToolCategory: "Read"}}
	var wg sync.WaitGroup
	results := make([]ToolsAnalyticsResponse, len(filters))
	errs := make([]error, len(filters))
	for i, f := range filters {
		wg.Go(func() { results[i], errs[i] = d.analyticsTools(ctx, f) })
	}
	wg.Wait()
	for i, f := range filters {
		require.NoError(t, errs[i])
		require.Equal(t, 1, results[i].TotalCalls)
		rows, err := d.analyticsToolRows(ctx, f)
		require.NoError(t, err)
		require.Equal(t, BuildToolsAnalytics(rows), results[i])
	}
	var token *string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT tools_generation FROM sessions WHERE id='a'`).Scan(&token))
	require.NotNil(t, token)
}

func TestToolsPartsWritesBeforeStampAndDuringReuse(t *testing.T) {
	t.Cleanup(func() {
		toolsPartsAfterSessionsObserver = nil
		toolsPartsBeforeValidationObserver = nil
	})
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Write' WHERE session_id='a'`)
	require.NoError(t, err)
	var writeErr error
	toolsPartsAfterSessionsObserver = func() {
		_, writeErr = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Grep' WHERE session_id='a'`)
	}
	got, err := d.analyticsTools(ctx, f)
	toolsPartsAfterSessionsObserver = nil
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Grep", got.ByTool[0].ToolName)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, "Grep", got.ByTool[0].ToolName)
	toolsPartsBeforeValidationObserver = func() {
		_, writeErr = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Read' WHERE session_id='a'`)
	}
	got, err = d.analyticsTools(ctx, f)
	toolsPartsBeforeValidationObserver = nil
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Read", got.ByTool[0].ToolName)
	got, err = d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, "Read", got.ByTool[0].ToolName)
}

func TestToolsPartsRawCallMoveAndRepeatedUpdates(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "project")
		msg := asstMsgAt(id, 0, "call", "2025-03-10T12:00:00Z")
		msg.ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, msg)
	}
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `CREATE TABLE tools_update_count(n INTEGER NOT NULL); INSERT INTO tools_update_count VALUES(0); CREATE TRIGGER count_tools_update AFTER UPDATE OF tools_generation ON sessions BEGIN UPDATE tools_update_count SET n=n+1; END`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET session_id='b', message_id=(SELECT id FROM messages WHERE session_id='b') WHERE session_id='a'; UPDATE tool_calls SET observed_outcome='empty' WHERE session_id='b'; UPDATE tool_calls SET observed_outcome=NULL WHERE session_id='b';`)
	require.NoError(t, err)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT n FROM tools_update_count`).Scan(&count))
	require.Equal(t, 2, count)
	got, err := d.analyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 2, got.TotalCalls)
	require.Len(t, got.ByTool, 1)
	require.Equal(t, 1, got.ByTool[0].SessionCount)
	rows, err := d.analyticsToolRows(ctx, f)
	require.NoError(t, err)
	require.Equal(t, BuildToolsAnalytics(rows), got)
}

func TestToolsPartsMissingGenerationFallsBack(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project")
	msg := asstMsgAt("a", 0, "call", "2025-03-10T12:00:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	_, err := d.getWriter().ExecContext(ctx, `DROP TRIGGER sessions_tools_generation_insert; DROP TRIGGER tool_calls_tools_generation_insert; DROP TRIGGER tool_calls_tools_generation_update; DROP TRIGGER tool_calls_tools_generation_delete; ALTER TABLE sessions DROP COLUMN tools_generation`)
	require.NoError(t, err)
	var reads int
	reset := setToolsPartsReadObserver(func([]string) { reads++ })
	defer reset()
	got, err := d.analyticsTools(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.Equal(t, 1, got.TotalCalls)
	require.Zero(t, reads)
}
