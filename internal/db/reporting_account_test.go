package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestCursorAccountEmptyPollIgnoresOtherDates(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.rawWriter().ExecContext(ctx, `INSERT INTO cursor_usage_events(occurred_at, model, account_user_id, account_polled_at) VALUES ('2025-07-28T10:00:00Z', 'a', '12', '2025-07-29T00:00:00.000000Z'); INSERT INTO cursor_account_polls(account_user_id, hour_start, polled_at) VALUES ('12', 1785232800, '2026-07-29T00:00:00.000000Z')`)
	require.NoError(t, err)
	conn, err := d.getReader().Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	var oldRows int
	require.NoError(t, conn.Raw(func(raw any) error {
		return raw.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("agentsview_timestamp_unix_micro", func(value string) (int64, error) {
			if value == "2025-07-28T10:00:00Z" {
				oldRows++
			}
			stamp, err := time.Parse(time.RFC3339Nano, value)
			return stamp.UnixMicro(), err
		}, true)
	}))
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var source reportingExportSource
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	require.NoError(t, d.loadReportingAccountUsage(ctx, tx, start, start.Add(24*time.Hour), &source))
	require.Len(t, source.accountUsage, 1)
	assert.Zero(t, oldRows, "a selected hour should not inspect another year's events")
}

func TestCursorAccountColumnsAbsentInOldResyncSource(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	src := testDBAtPath(t, path, "source")
	require.NoError(t, src.InsertCursorUsageEvents(ctx, []CursorUsageEvent{{OccurredAt: "2026-07-28T10:00:00Z", Model: "a", DedupKey: "a"}}))
	require.NoError(t, src.Close())

	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `ALTER TABLE cursor_usage_events DROP COLUMN account_user_id; ALTER TABLE cursor_usage_events DROP COLUMN account_polled_at`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	older := testDB(t)
	require.NoError(t, older.CopySessionMetadataFrom(ctx, path))
	var account, polled string
	require.NoError(t, older.Reader().QueryRowContext(ctx, `SELECT account_user_id,account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&account, &polled))
	assert.Empty(t, account)
	assert.Empty(t, polled)
}

func TestCursorAccountReporting(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "cursor-account:12"))
	_, err := d.rawWriter().ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES ('archive_id','archive-a') ON CONFLICT(key) DO UPDATE SET value=excluded.value;
		INSERT INTO pg_sync_state(key,value) VALUES ('machine_alias:peer','cursor-account:12');
		INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at) VALUES
		('native','project','local','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:01:00Z'),
		('peer','project','peer','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:02:00Z'),
		('direct','project','cursor-account:12','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:01:00Z'),
		('escaped','project','machine:2939296221cda1ef57dff3bf4333caa28eafdc25e0bc81db56cfd1ad9b755e9e','claude',2,'2026-07-28T10:00:00Z','2026-07-28T10:03:00Z');
		INSERT INTO messages(session_id,ordinal,role,content,timestamp,model) VALUES
		('native',0,'user','start','2026-07-28T10:00:00Z',''),('native',1,'assistant','end','2026-07-28T10:01:00Z','model-a'),
		('peer',0,'user','start','2026-07-28T10:00:00Z',''),('peer',1,'assistant','end','2026-07-28T10:02:00Z','model-a'),
		('direct',0,'user','start','2026-07-28T10:00:00Z',''),('direct',1,'assistant','end','2026-07-28T10:01:00Z','model-a'),
		('escaped',0,'user','start','2026-07-28T10:00:00Z',''),('escaped',1,'assistant','end','2026-07-28T10:03:00Z','model-a')`)
	require.NoError(t, err)
	a := CursorUsageEvent{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, DedupKey: "a", UserID: "152683922"}
	b := CursorUsageEvent{OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20, Charged: money.MustParseDollars("0.007"), DedupKey: "b"}
	c := CursorUsageEvent{OccurredAt: "2026-07-28T10:03:00Z", Model: "c", OutputTokens: 30, DedupKey: "c"}
	unassigned := CursorUsageEvent{OccurredAt: "2026-07-28T10:04:00Z", Model: "d", OutputTokens: 5, DedupKey: "d", UserID: "77"}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{a, b, c, unassigned}))
	first := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {"a"}}, first, nil))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {"b"}}, first.Add(time.Hour), nil))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"34": {"c"}}, first, nil))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"99": {"a"}}, first.Add(-time.Hour), nil))
	var raw, id, stamp string
	require.NoError(t, d.Reader().QueryRowContext(ctx, `SELECT user_id,account_user_id,account_polled_at FROM cursor_usage_events WHERE dedup_key='a'`).Scan(&raw, &id, &stamp))
	assert.Equal(t, "152683922", raw)
	assert.Equal(t, "12", id)
	assert.Equal(t, "2026-07-29T01:00:00.000000Z", stamp)
	opts := ReportingExportOptions{Date: first.Add(-25 * time.Hour), Now: first.Add(24 * time.Hour), SchemaVersion: 6, ByMachine: true}
	day, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	hour := day.Hours[10]
	const machine = "machine:2939296221cda1ef57dff3bf4333caa28eafdc25e0bc81db56cfd1ad9b755e9e"
	assert.Equal(t, machine, hour.LocalMachineID)
	assert.InDelta(t, 4, hour.Machines[machine].Activity.Totals.AgentMinutes, 0.001)
	assert.NotEmpty(t, hour.Machines[machine].Joint.Cells)
	assert.InDelta(t, 3, hour.Machines["machine:99b14951da0e7986cffad677018196fe8842ed68aa0eac7824b9c304fba6f930"].Activity.Totals.AgentMinutes, 0.001)
	assert.EqualValues(t, 20, hour.Machines["cursor-account:12"].Usage.Totals.OutputTokens)
	part := hour.Machines["cursor-account:12"]
	require.Len(t, part.Joint.Cells, 1)
	assert.Equal(t, part.Usage.Totals, part.Joint.Cells[0].Usage)
	assert.Equal(t, part.Usage.Totals.Cost, part.Joint.Cells[0].Pricing.ReportedCost)
	assert.NotZero(t, part.Joint.Cells[0].Pricing.ReportedCost.Microdollars)
	assert.Empty(t, part.Joint.Cells[0].ProjectKey)
	assert.Equal(t, "2026-07-29T02:00:00.000000Z", hour.Machines["cursor-account:12"].PolledAt)
	assert.EqualValues(t, 30, hour.Machines["cursor-account:34"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 15, hour.Machines["archive:archive-a"].Usage.Totals.OutputTokens)
	assert.NotContains(t, hour.Machines, "cursor-account:77")
	assert.EqualValues(t, 65, hour.Usage.Totals.OutputTokens)
	opts.ProjectKeys = []string{"other"}
	scoped, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.NotContains(t, scoped.Hours[10].Machines, "cursor-account:12")
	assert.NotContains(t, scoped.Hours[10].Machines, "cursor-account:34")
	opts.ProjectKeys = nil
	opts.SchemaVersion = 5
	legacy, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, "cursor-account:12", legacy.Hours[10].LocalMachineID)
	assert.InDelta(t, 4, legacy.Hours[10].Machines["cursor-account:12"].Activity.Totals.AgentMinutes, 0.001)
	assert.InDelta(t, 3, legacy.Hours[10].Machines[machine].Activity.Totals.AgentMinutes, 0.001)
	assert.EqualValues(t, 65, legacy.Hours[10].Machines["archive:archive-a"].Usage.Totals.OutputTokens)
}
