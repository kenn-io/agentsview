package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorAccountExportUpgrade(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "old")
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{{OccurredAt: "2026-07-28T10:00:00Z", Model: "a", UserID: "42", DedupKey: "a"}}))
	require.NoError(t, d.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `ALTER TABLE cursor_usage_events DROP COLUMN account_user_id; ALTER TABLE cursor_usage_events DROP COLUMN account_polled_at`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	_, err = OpenReadOnly(ctx, path)
	require.Error(t, err)
	assert.True(t, IsSchemaUpgradeRequired(err))
	require.NoError(t, UpgradeExportSchemaInPlace(ctx, path, &SchemaUpgradeRequiredError{Table: "cursor_usage_events", Column: "account_user_id"}))
	err = UpgradeExportSchemaInPlace(ctx, path, &SchemaUpgradeRequiredError{Table: "cursor_usage_events", Column: "agent"})
	require.Error(t, err)
	upgraded, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	defer upgraded.Close()
	var userID, account, polled string
	require.NoError(t, upgraded.Reader().QueryRowContext(ctx, `SELECT user_id,account_user_id,account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&userID, &account, &polled))
	assert.Equal(t, "42", userID)
	assert.Empty(t, account)
	assert.Empty(t, polled)
}

func TestCursorAccountColumnsSurviveResync(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	src := testDBAtPath(t, path, "source")
	require.NoError(t, src.InsertCursorUsageEvents(ctx, []CursorUsageEvent{{OccurredAt: "2026-07-28T10:00:00Z", Model: "a", DedupKey: "a"}}))
	require.NoError(t, src.AssignCursorUsageAccount(ctx, "12", []string{"a"}, time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)))
	require.NoError(t, src.Close())
	dst := testDB(t)
	require.NoError(t, dst.CopySessionMetadataFrom(ctx, path))
	var account, polled string
	require.NoError(t, dst.Reader().QueryRowContext(ctx, `SELECT account_user_id,account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&account, &polled))
	assert.Equal(t, "12", account)
	assert.Equal(t, "2026-07-29T01:00:00.000000Z", polled)
}

func TestCursorAccountResyncFromOlderArchives(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drop    string
		account string
		polled  string
	}{
		{"both missing", "ALTER TABLE cursor_usage_events DROP COLUMN account_user_id; ALTER TABLE cursor_usage_events DROP COLUMN account_polled_at", "", ""},
		{"account missing", "ALTER TABLE cursor_usage_events DROP COLUMN account_user_id", "", "2026-07-29T01:00:00.000000Z"},
		{"poll missing", "ALTER TABLE cursor_usage_events DROP COLUMN account_polled_at", "12", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "source.db")
			src := testDBAtPath(t, path, "source")
			require.NoError(t, src.InsertCursorUsageEvents(ctx, []CursorUsageEvent{{OccurredAt: "2026-07-28T10:00:00Z", Model: "a", DedupKey: "a"}}))
			require.NoError(t, src.AssignCursorUsageAccount(ctx, "12", []string{"a"}, time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)))
			require.NoError(t, src.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), tc.drop)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			dst := testDB(t)
			require.NoError(t, dst.CopySessionMetadataFrom(ctx, path))
			var account, polled string
			require.NoError(t, dst.Reader().QueryRowContext(ctx, `SELECT account_user_id, account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&account, &polled))
			assert.Equal(t, tc.account, account)
			assert.Equal(t, tc.polled, polled)
		})
	}
}

func TestCursorAccountCoverageStaysWithReturnedEvents(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "machine-a"))
	a := CursorUsageEvent{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, DedupKey: "a", UserID: "raw-a"}
	b := CursorUsageEvent{OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20, DedupKey: "b"}
	c := CursorUsageEvent{OccurredAt: "2026-07-28T10:03:00Z", Model: "c", OutputTokens: 30, DedupKey: "c"}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{a, b, c}))
	t1 := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	require.Error(t, d.AssignCursorUsageAccount(ctx, "invalid", []string{"a"}, t1))
	require.NoError(t, d.AssignCursorUsageAccount(ctx, "12", []string{"a"}, t1))
	require.NoError(t, d.AssignCursorUsageAccount(ctx, "34", []string{"b"}, t1.Add(time.Hour)))
	require.NoError(t, d.AssignCursorUsageAccount(ctx, "99", []string{"a"}, t1.Add(-time.Hour)))
	var raw, id, stamp string
	require.NoError(t, d.Reader().QueryRowContext(ctx, `SELECT user_id, account_user_id, account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&raw, &id, &stamp))
	assert.Equal(t, "raw-a", raw)
	assert.Equal(t, "12", id)
	assert.Equal(t, "2026-07-29T01:00:00.000000Z", stamp)
	day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: t1.Add(-25 * time.Hour), Now: t1.Add(24 * time.Hour), SchemaVersion: 6, ByMachine: true})
	require.NoError(t, err)
	hour := day.Hours[10]
	assert.EqualValues(t, 10, hour.Machines["cursor-account:12"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 20, hour.Machines["cursor-account:34"].Usage.Totals.OutputTokens)
	assert.Equal(t, "2026-07-29T01:00:00.000000Z", hour.Machines["cursor-account:12"].PolledAt)
	assert.EqualValues(t, 60, hour.Usage.Totals.OutputTokens)
}

func TestCursorAccountReportingAcrossArchives(t *testing.T) {
	ctx := t.Context()
	t1 := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	opts := ReportingExportOptions{Date: t1.Add(-25 * time.Hour), Now: t1.Add(24 * time.Hour), SchemaVersion: 6, ByMachine: true}
	for _, archive := range []string{"archive-a", "archive-b"} {
		t.Run(archive, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "machine-a"))
			_, err := d.rawWriter().ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES ('archive_id',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, archive)
			require.NoError(t, err)
			require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, DedupKey: "a"}, {OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20, DedupKey: "b"}}))
			day, err := d.ExportReportingDay(ctx, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 30, day.Hours[10].Machines["archive:"+archive].Usage.Totals.OutputTokens)
			require.NoError(t, d.AssignCursorUsageAccount(ctx, "12", []string{"a"}, t1))
			require.NoError(t, d.AssignCursorUsageAccount(ctx, "34", []string{"b"}, t1))
			day, err = d.ExportReportingDay(ctx, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 10, day.Hours[10].Machines["cursor-account:12"].Usage.Totals.OutputTokens)
			assert.EqualValues(t, 20, day.Hours[10].Machines["cursor-account:34"].Usage.Totals.OutputTokens)
			assert.EqualValues(t, 30, day.Hours[10].Usage.Totals.OutputTokens)
		})
	}
}

func TestExportReportingHourLoadsOnlySelectedAccountRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "machine-a"))
	_, err := d.rawWriter().ExecContext(ctx, `INSERT INTO cursor_usage_events(occurred_at, model, output_tokens, dedup_key, account_user_id, account_polled_at) VALUES
		('2026-07-28T09:59:59.999999Z','a',1,'a','1','2026-07-29T01:00:00.000000Z'),
		('2026-07-28T10:00:00Z','a',2,'b','2','2026-07-29T01:00:00.000000Z'),
		('2026-07-28T10:30:00Z','a',1,'e','5','2026-07-29T01:00:00.000000Z'),
		('2026-07-28T10:59:59.999999Z','a',3,'c','3','2026-07-29T01:00:00.000000Z'),
		('2026-07-28T11:00:00Z','a',4,'d','4','2026-07-29T01:00:00.000000Z');
		WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<200)
		INSERT INTO cursor_usage_events(occurred_at,model,output_tokens,dedup_key,account_user_id,account_polled_at)
		SELECT '2026-07-28T11:05:00Z','a',1,printf('other-%d',x),printf('%d',x+100),'2026-07-29T01:00:00.000000Z' FROM n`)
	require.NoError(t, err)
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	var loaded int
	opts := ReportingExportOptions{Now: start.Add(48 * time.Hour), SchemaVersion: 6, ByMachine: true, afterSourceLoad: func(s reportingSourceLoadStats) { loaded = s.AccountUsageRows }}
	hour, err := d.ExportReportingHour(ctx, opts, start)
	require.NoError(t, err)
	assert.Equal(t, 3, loaded)
	assert.EqualValues(t, 6, hour.Usage.Totals.OutputTokens)
	assert.Contains(t, hour.Machines, "cursor-account:2")
	assert.Contains(t, hour.Machines, "cursor-account:3")
	assert.NotContains(t, hour.Machines, "cursor-account:4")
	opts.Date = start.Truncate(24 * time.Hour)
	day, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, 205, loaded)
	assert.Equal(t, day.Hours[10], hour)
}

func TestReportingDayManyAccountEvents(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "machine-a"))
	_, err := d.rawWriter().ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<5000)
		INSERT INTO cursor_usage_events(occurred_at,model,output_tokens,dedup_key,account_user_id,account_polled_at)
		SELECT '2026-07-28T10:05:00Z','a',1,printf('event-%d',x),printf('%d',1+((x-1)%200)),'2026-07-29T01:00:00.000000Z' FROM n`)
	require.NoError(t, err)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: date, Now: date.Add(48 * time.Hour), SchemaVersion: 6, ByMachine: true})
	require.NoError(t, err)
	hour := day.Hours[10]
	assert.Len(t, hour.Machines, 201)
	assert.EqualValues(t, 5000, hour.Usage.Totals.OutputTokens)
	assert.EqualValues(t, 25, hour.Machines["cursor-account:1"].Usage.Totals.OutputTokens)
	assert.Empty(t, hour.Machines["cursor-account:1"].Joint.Cells)
}

func TestCursorAccountHourUsesLatestPollRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "machine-a"))
	teamA := CursorUsageEvent{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, DedupKey: "a"}
	teamB := CursorUsageEvent{OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20, DedupKey: "b"}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{teamA}))
	first := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	require.NoError(t, d.AssignCursorUsageAccount(ctx, "12", []string{"a"}, first))
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{teamB}))
	require.NoError(t, d.AssignCursorUsageAccount(ctx, "12", []string{"b"}, first.Add(time.Hour)))
	day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: first.Add(-25 * time.Hour), Now: first.Add(24 * time.Hour), SchemaVersion: 6, ByMachine: true})
	require.NoError(t, err)
	account := day.Hours[10].Machines["cursor-account:12"]
	assert.EqualValues(t, 20, account.Usage.Totals.OutputTokens)
	assert.Equal(t, "2026-07-29T02:00:00.000000Z", account.PolledAt)
}
