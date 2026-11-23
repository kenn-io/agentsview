package db

import (
	"context"
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
)

type storedUsageCounters struct {
	outputTokens, webSearchRequests int64
}

func readStoredUsageCounters(t *testing.T, d *DB, sessionID string) []storedUsageCounters {
	t.Helper()
	rows, err := d.Reader().Query(t.Context(), `
		SELECT usage_output_tokens, usage_web_search_requests
		FROM messages WHERE session_id = ? ORDER BY ordinal`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var got []storedUsageCounters
	for rows.Next() {
		var c storedUsageCounters
		require.NoError(t, rows.Scan(&c.outputTokens, &c.webSearchRequests))
		got = append(got, c)
	}
	require.NoError(t, rows.Err())
	return got
}

func TestOpenFillsMessageUsageCountersOnOlderArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	withUsage := func(ordinal int, usage string) Message {
		m := asstMsg("s", ordinal, "reply")
		m.TokenUsage = jsontext.Value(usage)
		return m
	}
	insertMessages(t, d,
		userMsg("s", 0, "question"),
		withUsage(1, `{"output_tokens":250,"server_tool_use":{"web_search_requests":3}}`),
		withUsage(2, `{"output_tokens":-5,"server_tool_use":{"web_search_requests":-2}}`),
		withUsage(3, `{"output_tokens":40,"server_tool_use":{"web_sea`),
	)
	// Rebuild the shape an older release wrote: no stored counter columns, and
	// a covering index without them.
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	assert.Equal(t, []storedUsageCounters{
		{0, 0},
		{250, 3},
		{0, 0},
		// A cut-off blob still yields the output tokens read before the cut.
		{40, 0},
	}, readStoredUsageCounters(t, d, "s"))
}

func TestLateMessageUsageStoresUsageCounters(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, asstMsg("s1", 0, "reply"))

	_, err := d.WriteSessionIncremental(t.Context(), "s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 1,
		MessageTokenUsageUpdates: []MessageTokenUsageUpdate{{
			Ordinal:          0,
			TokenUsage:       jsontext.Value(`{"input_tokens":10,"output_tokens":250,"server_tool_use":{"web_search_requests":2}}`),
			ContextTokens:    10,
			OutputTokens:     250,
			HasContextTokens: true,
			HasOutputTokens:  true,
		}},
	})
	require.NoError(t, err)

	assert.Equal(t, []storedUsageCounters{{250, 2}}, readStoredUsageCounters(t, d, "s1"))
}

func TestOpenDaemonDefersMessageUsageCounterFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	m := asstMsg("s", 0, "reply")
	m.Model = "claude-sonnet"
	m.TokenUsage = jsontext.Value(`{"output_tokens":250,"server_tool_use":{"web_search_requests":3}}`)
	insertMessages(t, d, m)
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	assert.True(t, d.SearchRepairPending())
	assert.Equal(t, []storedUsageCounters{{0, 0}}, readStoredUsageCounters(t, d, "s"))
	var pending bool
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&pending))
	require.True(t, pending)
	readOnly, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	defer readOnly.Close()
	_, _, _, err = readOnly.loadActivityReportUsageCandidatesFrom(t.Context(), readOnly.getReader(), []string{"s"}, "2020-01-01", "2030-01-01", false)
	require.ErrorIs(t, err, ErrMessageUsageCountersPending)
	date := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = readOnly.ExportReportingDay(t.Context(), ReportingExportOptions{Date: date, Now: date.Add(24 * time.Hour)})
	require.ErrorIs(t, err, ErrMessageUsageCountersPending)
	_, err = readOnly.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{From: date, To: date, Now: date.Add(24 * time.Hour)})
	require.ErrorIs(t, err, ErrMessageUsageCountersPending)
	_, err = readOnly.ListSessions(t.Context(), SessionFilter{Limit: 25})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err = d.loadActivityReportUsageCandidatesFrom(ctx, d.getReader(), []string{"s"}, "2020-01-01", "2030-01-01", false)
	require.ErrorIs(t, err, context.Canceled)

	done := make(chan error, 1)
	go func() {
		_, _, _, err := d.loadActivityReportUsageCandidatesFrom(t.Context(), d.getReader(), []string{"s"}, "2020-01-01", "2030-01-01", false)
		done <- err
	}()
	snapshots := make(chan string, 2)
	reports := make(chan error, 2)
	go func() {
		_, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{Date: date, Now: date.Add(24 * time.Hour), afterSnapshot: func() { snapshots <- "day" }})
		reports <- err
	}()
	go func() {
		_, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{From: date, To: date, Now: date.Add(24 * time.Hour), afterSnapshot: func() { snapshots <- "digest" }})
		reports <- err
	}()
	dailyGot, err := d.GetDailyUsage(t.Context(), UsageFilter{})
	require.NoError(t, err)
	assert.Equal(t, 250, dailyGot.Totals.OutputTokens)
	select {
	case err := <-done:
		require.FailNow(t, "activity usage returned before fill", "%v", err)
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: the pending fill keeps activity usage waiting
	}
	select {
	case report := <-snapshots:
		require.FailNow(t, "report took a snapshot before fill", report)
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: pending fill keeps reporting snapshots waiting
	}
	require.NoError(t, d.RepairMessageFTS(t.Context()))
	require.NoError(t, <-done)
	require.NoError(t, <-reports)
	require.NoError(t, <-reports)
	assert.ElementsMatch(t, []string{"day", "digest"}, []string{<-snapshots, <-snapshots})
	readOnlyAfter, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	_, _, _, err = readOnlyAfter.loadActivityReportUsageCandidatesFrom(t.Context(), readOnlyAfter.getReader(), []string{"s"}, "2020-01-01", "2030-01-01", false)
	require.NoError(t, err)
	require.NoError(t, readOnlyAfter.Close())
	assert.Equal(t, []storedUsageCounters{{250, 3}}, readStoredUsageCounters(t, d, "s"))
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&pending))
	assert.False(t, pending)
	require.NoError(t, d.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer d.Close()
	dailyWant, err := d.GetDailyUsage(t.Context(), UsageFilter{})
	require.NoError(t, err)
	assert.Equal(t, dailyWant, dailyGot)
}

func TestReopenFinishesPendingMessageUsageCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	m := asstMsg("s", 0, "reply")
	m.TokenUsage = jsontext.Value(`{"output_tokens":42}`)
	insertMessages(t, d, m)
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	for range 2 {
		d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
		require.NoError(t, err)
		assert.True(t, d.SearchRepairPending())
		require.NoError(t, d.Close())
	}
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer d.Close()
	assert.Equal(t, []storedUsageCounters{{42, 0}}, readStoredUsageCounters(t, d, "s"))
	var pending bool
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&pending))
	assert.False(t, pending)
	_, err = d.getWriter().Exec(t.Context(), `CREATE TABLE fill_probe(n INTEGER); CREATE TRIGGER fill_probe_trigger AFTER UPDATE OF usage_output_tokens ON messages BEGIN INSERT INTO fill_probe VALUES(1); END`)
	require.NoError(t, err)
	tx, err := d.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, fillMessageUsageCounters(t.Context(), tx))
	require.NoError(t, tx.Commit())
	var updates int
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT count(*) FROM fill_probe`).Scan(&updates))
	assert.Zero(t, updates)
}

func TestUsageCounterReadersDifferBeforeFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	for _, id := range []string{"s1", "s2"} {
		insertSession(t, d, id, "proj", func(s *Session) { s.Agent = "claude"; s.StartedAt = new(tsZero); s.EndedAt = new(tsZeroS2) })
	}
	for _, m := range []Message{
		{SessionID: "s1", Role: "assistant", Timestamp: tsZero, Model: "claude-sonnet", TokenUsage: jsontext.Value(`{"input_tokens":100,"output_tokens":10}`), ClaudeMessageID: "m", ClaudeRequestID: "r"},
		{SessionID: "s1", Ordinal: 1, Role: "assistant", Timestamp: tsZero, Model: "claude-sonnet", TokenUsage: jsontext.Value(`{"input_tokens":200,"output_tokens":50,"server_tool_use":{"web_search_requests":3}}`), ClaudeMessageID: "m", ClaudeRequestID: "r"},
		{SessionID: "s2", Role: "assistant", Timestamp: tsZero, Model: "claude-sonnet", TokenUsage: jsontext.Value(`{"input_tokens":300,"output_tokens":30}`), ClaudeMessageID: "m", ClaudeRequestID: "r"},
	} {
		insertMessages(t, d, m)
	}
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	defer d.Close()
	read := func() map[string]any {
		ctx := t.Context()
		session, err := d.GetSessionUsage(ctx, "s1", true)
		require.NoError(t, err)
		rows, err := d.GetSessionUsageRows(ctx, []string{"s1", "s2"})
		require.NoError(t, err)
		candidates, _, _, err := d.loadActivityReportUsageCandidatesFrom(ctx, d.getReader(), []string{"s1", "s2"}, "2020-01-01", "2030-01-01", false)
		require.NoError(t, err)
		start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		end := start.Add(24 * time.Hour)
		activityRows, _, _, err := d.activityReportUsage(ctx, []string{"s1", "s2"}, "2023-12-31", "2024-01-03", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: end}, false)
		require.NoError(t, err)
		day, err := d.ExportReportingDay(ctx, ReportingExportOptions{Date: start, Now: end})
		require.NoError(t, err)
		digest, err := d.ExportReportingDigest(ctx, ReportingDigestExportOptions{From: start, To: start, Now: end})
		require.NoError(t, err)
		page, err := d.ExportSessionSummaries(ctx, SessionExportOptions{})
		require.NoError(t, err)
		all, err := d.ExportAllSessionSummaries(ctx, SessionExportOptions{})
		require.NoError(t, err)
		return map[string]any{"session": session, "rows": rows, "candidates": candidates, "activity": activityRows, "day": day, "digest": digest, "page": page, "all": all}
	}
	assert.Equal(t, []storedUsageCounters{{0, 0}, {0, 0}}, readStoredUsageCounters(t, d, "s1"))
	assert.Equal(t, []storedUsageCounters{{0, 0}}, readStoredUsageCounters(t, d, "s2"))
	d.usageCountersPending.Store(false)
	before := read()
	d.usageCountersPending.Store(true)
	require.NoError(t, d.RepairMessageFTS(t.Context()))
	assert.Equal(t, []storedUsageCounters{{10, 0}, {50, 3}}, readStoredUsageCounters(t, d, "s1"))
	assert.Equal(t, []storedUsageCounters{{30, 0}}, readStoredUsageCounters(t, d, "s2"))
	after := read()
	assert.NotEqual(t, before["candidates"], after["candidates"])
	for _, name := range []string{"session", "rows", "activity", "day", "digest", "page", "all"} {
		assert.Equal(t, before[name], after[name], name)
	}
}
