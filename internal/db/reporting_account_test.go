package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestCursorAccountQueriesUseIndexes(t *testing.T) {
	d := testDB(t)
	for _, tc := range []struct{ name, query, index, rangeCondition string }{
		{"poll hours", cursorAccountHoursQuery, "idx_cursor_account_polls_hour", "hour_start>? AND hour_start<?"},
		{"empty polls", reportingEmptyCursorPollsQuery, "idx_cursor_usage_events_account_time", "account_user_id=? AND occurred_at>? AND occurred_at<?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := strings.Join(explainQueryPlan(t, d, tc.query, 0, 3600), "\n")
			assert.Contains(t, plan, tc.index)
			assert.Contains(t, plan, tc.rangeCondition)
		})
	}
}

func TestCursorAccountPollStatementsUseDedupIndex(t *testing.T) {
	d := testDB(t)
	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"previous poll", cursorAccountPreviousPollQuery, []any{"stamp", "12", "[]", "stamp"}},
		{"assignment", cursorAccountAssignQuery, []any{"12", "stamp", "[]", "stamp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := strings.Join(explainQueryPlan(t, d, tc.query, tc.args...), "\n")
			assert.Contains(t, plan, "idx_cursor_usage_events_dedup")
			assert.NotContains(t, plan, "SCAN cursor_usage_events")
		})
	}
}

func TestCursorAccountReassignmentAndClearing(t *testing.T) {
	for _, owner := range []string{"", "34"} {
		t.Run("new owner "+owner, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "local"))
			start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
			event := CursorUsageEvent{OccurredAt: "2026-07-28T10:03:00Z", Model: "a", OutputTokens: 7}
			require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{event}))
			first := start.Add(24 * time.Hour)
			require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(event)}}, first, []CursorAccountHour{{UserID: "12", Start: start}}))
			require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{owner: {CursorUsageEventDedupKey(event)}}, first.Add(2*time.Hour), nil))
			if owner != "" {
				require.NoError(t, d.RecordCursorAccountPoll(ctx, nil, first.Add(2*time.Hour), []CursorAccountHour{{UserID: owner, Start: start}}))
			}
			require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(event)}}, first.Add(time.Hour), nil))
			hours, err := d.CursorAccountHours(ctx, start, start.Add(time.Hour))
			require.NoError(t, err)
			assert.Contains(t, hours, CursorAccountHour{UserID: "12", Start: start})
			var polledAt, account string
			require.NoError(t, d.Reader().QueryRowContext(ctx, `SELECT polled_at FROM cursor_account_polls WHERE account_user_id='12' AND hour_start=?`, start.Unix()).Scan(&polledAt))
			assert.Equal(t, "2026-07-29T12:00:00.000000Z", polledAt)
			require.NoError(t, d.Reader().QueryRowContext(ctx, `SELECT account_user_id FROM cursor_usage_events WHERE dedup_key=?`, CursorUsageEventDedupKey(event)).Scan(&account))
			assert.Equal(t, owner, account)
			day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: start.Truncate(24 * time.Hour), Now: first.Add(3 * time.Hour), SchemaVersion: 6, ByMachine: true})
			require.NoError(t, err)
			old := day.Hours[10].Machines["cursor-account:12"]
			assert.Zero(t, old.Usage.Totals.OutputTokens)
			assert.Equal(t, "2026-07-29T12:00:00.000000Z", old.PolledAt)
			if owner == "34" {
				assert.EqualValues(t, 7, day.Hours[10].Machines["cursor-account:34"].Usage.Totals.OutputTokens)
			} else {
				var archiveTokens int64
				for id, machine := range day.Hours[10].Machines {
					if strings.HasPrefix(id, "archive:") {
						archiveTokens += machine.Usage.Totals.OutputTokens
					}
				}
				assert.EqualValues(t, 7, archiveTokens)
			}
		})
	}
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
	a := CursorUsageEvent{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, UserID: "152683922"}
	b := CursorUsageEvent{OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20, Charged: money.MustParseDollars("0.007")}
	c := CursorUsageEvent{OccurredAt: "2026-07-28T10:03:00Z", Model: "c", OutputTokens: 30}
	unassigned := CursorUsageEvent{OccurredAt: "2026-07-28T10:04:00Z", Model: "d", OutputTokens: 5, UserID: "77"}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{a, b, c, unassigned}))
	first := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	hourStart := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(a)}}, first, []CursorAccountHour{{UserID: "12", Start: hourStart}}))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(b)}}, first.Add(time.Hour), []CursorAccountHour{{UserID: "12", Start: hourStart}}))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"34": {CursorUsageEventDedupKey(c)}}, first, []CursorAccountHour{{UserID: "34", Start: hourStart}}))
	var raw, id, stamp string
	require.NoError(t, d.Reader().QueryRowContext(ctx, `SELECT user_id,account_user_id,account_polled_at FROM cursor_usage_events WHERE dedup_key=?`, CursorUsageEventDedupKey(a)).Scan(&raw, &id, &stamp))
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
	opts.SchemaVersion = 7
	current, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Zero(t, current.Hours[10].Machines["archive:archive-a"].Usage.Totals.OutputTokens)
	assert.NotContains(t, current.Hours[10].Machines, "cursor-account:12")
	require.Len(t, current.Hours[10].CursorEvents, 4)
	assert.Empty(t, current.Hours[10].CursorPolls)
	assert.EqualValues(t, 65, current.Hours[10].Usage.Totals.OutputTokens)
	opts.ProjectKeys = []string{"other"}
	scoped, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.NotContains(t, scoped.Hours[10].Machines, "cursor-account:12")
	assert.NotContains(t, scoped.Hours[10].Machines, "cursor-account:34")
	assert.Empty(t, scoped.Hours[10].CursorEvents)
	assert.Empty(t, scoped.Hours[10].CursorPolls)
	opts.SchemaVersion = 6
	legacyScoped, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, legacyScoped.Hours[10].Usage, scoped.Hours[10].Usage)
	opts.ProjectKeys = nil
	opts.SchemaVersion = 5
	legacy, err := d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, "cursor-account:12", legacy.Hours[10].LocalMachineID)
	assert.InDelta(t, 4, legacy.Hours[10].Machines["cursor-account:12"].Activity.Totals.AgentMinutes, 0.001)
	assert.InDelta(t, 3, legacy.Hours[10].Machines[machine].Activity.Totals.AgentMinutes, 0.001)
	assert.EqualValues(t, 65, legacy.Hours[10].Machines["archive:archive-a"].Usage.Totals.OutputTokens)

	// A later empty poll moves the earlier account usage back to the archive.
	require.NoError(t, d.RecordCursorAccountPoll(ctx, nil, first.Add(3*time.Hour), []CursorAccountHour{{UserID: "12", Start: hourStart}}))
	opts.SchemaVersion = 6
	day, err = d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	empty := day.Hours[10].Machines["cursor-account:12"]
	assert.Zero(t, empty.Usage.Totals.OutputTokens)
	assert.Equal(t, "2026-07-29T04:00:00.000000Z", empty.PolledAt)
	assert.EqualValues(t, 35, day.Hours[10].Machines["archive:archive-a"].Usage.Totals.OutputTokens)
	opts.SchemaVersion = 7
	day, err = d.ExportReportingDay(ctx, opts)
	require.NoError(t, err)
	assert.Equal(t, "2026-07-29T04:00:00.000000Z", day.Hours[10].CursorPolls[0].PolledAt)
	assert.NotContains(t, day.Hours[10].Machines, "cursor-account:12")
	assert.EqualValues(t, 65, day.Hours[10].Usage.Totals.OutputTokens)
}

func TestCursorAccountOverlappingArchivesEvidence(t *testing.T) {
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	poll := start.Add(24 * time.Hour)
	events := []CursorUsageEvent{
		{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, ConversationID: "conversation-a"},
		{OccurredAt: "2026-07-28T10:02:00Z", Model: "b", OutputTokens: 20},
		{OccurredAt: "2026-07-28T10:03:00Z", Model: "c", OutputTokens: 40},
	}
	for _, tc := range []struct {
		name    string
		bEvents []int
		empty   bool
	}{
		{"reproduction", []int{0, 1, 2}, false},
		{"different history", []int{0, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hours []export.ReportingHour
			for i := range 2 {
				d := testDB(t)
				ctx := t.Context()
				require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, []string{"host-a", "host-b"}[i]))
				indices := []int{0, 1}
				if i == 1 {
					indices = tc.bEvents
				}
				for _, j := range indices {
					event := events[j]
					if i == 1 && j == 0 {
						event.ConversationID = "conversation-b"
					}
					require.NoError(t, d.InsertCursorUsageEvents(ctx, []CursorUsageEvent{event}))
				}
				if i == 0 {
					require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(events[0]), CursorUsageEventDedupKey(events[1])}}, poll, []CursorAccountHour{{UserID: "12", Start: start}}))
					if !tc.empty {
						require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(events[1])}}, poll.Add(time.Hour), []CursorAccountHour{{UserID: "12", Start: start}}))
					}
				} else if tc.empty {
					require.NoError(t, d.RecordCursorAccountPoll(ctx, nil, poll.Add(2*time.Hour), []CursorAccountHour{{UserID: "12", Start: start}}))
				} else {
					require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(events[0]), CursorUsageEventDedupKey(events[1])}}, poll.Add(2*time.Hour), []CursorAccountHour{{UserID: "12", Start: start}}))
				}
				day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: start.Truncate(24 * time.Hour), Now: poll.Add(24 * time.Hour), SchemaVersion: 7, ByMachine: true})
				require.NoError(t, err)
				hours = append(hours, day.Hours[10])
			}
			first := export.ReportingCursorEvent{At: events[0].OccurredAt, Key: CursorUsageEventDedupKey(events[0]), Model: "a", OutputTokens: 10, ConversationID: "conversation-a", Account: "12", PolledAt: poll.Format("2006-01-02T15:04:05.000000Z")}
			assert.Contains(t, hours[0].CursorEvents, first)
			second := first
			second.ConversationID = "conversation-b"
			if tc.empty {
				second.Account, second.PolledAt = "", ""
				assert.Contains(t, hours[1].CursorPolls, export.ReportingCursorPoll{Account: "12", PolledAt: poll.Add(2 * time.Hour).Format("2006-01-02T15:04:05.000000Z")})
			} else {
				second.PolledAt = poll.Add(2 * time.Hour).Format("2006-01-02T15:04:05.000000Z")
			}
			assert.Contains(t, hours[1].CursorEvents, second)
			assert.Contains(t, hours[1].CursorEvents, export.ReportingCursorEvent{At: events[2].OccurredAt, Key: CursorUsageEventDedupKey(events[2]), Model: "c", OutputTokens: 40})
		})
	}
}

func TestCursorAccountLedgerRebuildsBuckets(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.ConfigureArtifactLocalMachine(ctx, "host-a"))
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	poll := start.Add(24 * time.Hour)
	events := []CursorUsageEvent{
		{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 10, ConversationID: "conversation-a"},
		{OccurredAt: "2026-07-28T10:07:00Z", Model: "a", OutputTokens: 20},
	}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, events))
	require.NoError(t, d.RecordCursorAccountPoll(ctx, map[string][]string{"12": {CursorUsageEventDedupKey(events[0]), CursorUsageEventDedupKey(events[1])}}, poll, []CursorAccountHour{{UserID: "12", Start: start}}))
	for _, empty := range []bool{false, true} {
		if empty {
			require.NoError(t, d.RecordCursorAccountPoll(ctx, nil, poll.Add(time.Hour), []CursorAccountHour{{UserID: "12", Start: start}}))
		}
		opts := ReportingExportOptions{Date: start.Truncate(24 * time.Hour), Now: poll.Add(24 * time.Hour), SchemaVersion: 6, ByMachine: true}
		legacy, err := d.ExportReportingDay(ctx, opts)
		require.NoError(t, err)
		opts.SchemaVersion = 7
		ledger, err := d.ExportReportingDay(ctx, opts)
		require.NoError(t, err)
		hour := ledger.Hours[10]
		assert.Contains(t, hour.CursorEvents, export.ReportingCursorEvent{At: events[0].OccurredAt, Key: CursorUsageEventDedupKey(events[0]), Model: "a", OutputTokens: 10, ConversationID: "conversation-a", Account: "12", PolledAt: poll.Format("2006-01-02T15:04:05.000000Z")})
		latest := map[string]string{}
		for _, p := range hour.CursorPolls {
			latest[p.Account] = p.PolledAt
		}
		for _, e := range hour.CursorEvents {
			if e.PolledAt > latest[e.Account] {
				latest[e.Account] = e.PolledAt
			}
		}
		buckets := map[string]int64{}
		for _, e := range hour.CursorEvents {
			if e.Account != "" && e.PolledAt == latest[e.Account] {
				at, err := time.Parse(time.RFC3339Nano, e.At)
				require.NoError(t, err)
				buckets[at.Truncate(5*time.Minute).Format(time.RFC3339)] += e.OutputTokens
			}
		}
		want := map[string]int64{}
		for _, cell := range legacy.Hours[10].Machines["cursor-account:12"].Joint.Cells {
			want[cell.BucketStart] += cell.Usage.OutputTokens
		}
		assert.Equal(t, want, buckets)

	}
}

func TestCursorAccountReportingSharedUsageRead(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.rawWriter().ExecContext(ctx, `INSERT INTO cursor_usage_events(agent, occurred_at, model, output_tokens, dedup_key) VALUES
		('cursor', '2026-07-28T10:01:00Z', 'cursor-model', 10, 'cursor-event'),
		('cursor', '2026-07-28T11:00:00Z', 'outside', 20, 'outside-event'),
		('grok', '2026-07-28T10:02:00Z', 'other-model', 30, 'other-event')`)
	require.NoError(t, err)
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	tx, err := d.getReader().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	source, err := d.loadReportingExportSource(ctx, tx, start, start.Add(time.Hour), 7)
	require.NoError(t, err)
	require.Len(t, source.cursorEvents, 1)
	assert.Equal(t, "2026-07-28T10:01:00Z", source.cursorEvents[0].event.At)
	assert.EqualValues(t, 10, source.cursorEvents[0].event.OutputTokens)
}

func TestCursorAccountConversationIDRefetch(t *testing.T) {
	d := testDB(t)
	event := CursorUsageEvent{OccurredAt: "2026-07-28T10:01:00Z", Model: "a", OutputTokens: 1}
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{event}))
	event.ConversationID = " conversation-a "
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{event}))
	event.ConversationID = "conversation-b"
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{event}))
	var count int
	var conversation string
	require.NoError(t, d.Reader().QueryRowContext(t.Context(), `SELECT count(*), max(conversation_id) FROM cursor_usage_events`).Scan(&count, &conversation))
	assert.Equal(t, 1, count)
	assert.Equal(t, "conversation-a", conversation)
}
