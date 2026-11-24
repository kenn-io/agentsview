package db

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestCancelledUsageRequestDoesNotCancelCounterFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	m := asstMsg("s", 0, "reply")
	m.Model = "claude-sonnet"
	m.TokenUsage = jsontext.Value(`{"output_tokens":42}`)
	insertMessages(t, d, m)
	removeMessageUsageCounters(t, d)
	require.NoError(t, d.Close())
	d, err := OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	require.NoError(t, d.lockWriterCtx(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := d.GetSessionUsage(ctx, "s", true); done <- err }()
	select {
	case err := <-done:
		d.mu.Unlock()
		require.FailNow(t, "usage returned before fill", "%v", err)
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: writer lock holds the fill
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	d.usageCountersMu.Lock()
	fill := d.usageCountersFill
	d.usageCountersMu.Unlock()
	d.mu.Unlock()
	require.NotNil(t, fill)
	<-fill.done
	require.NoError(t, fill.err)
	assert.Equal(t, []storedUsageCounters{{42, 0}}, readStoredUsageCounters(t, d, "s"))
}

func TestUsageIndexReplacementFailurePreservesIndex(t *testing.T) {
	d := testDB(t)
	err := ensureIndexColumnsLocked(t.Context(), d.getWriter(), "idx_messages_usage_session_covering", []string{"different"}, "CREATE INDEX invalid SQL")
	require.Error(t, err)
	var present bool
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = 'idx_messages_usage_session_covering')`).Scan(&present))
	assert.True(t, present)
}

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

func removeMessageUsageCounters(t *testing.T, d *DB) {
	t.Helper()
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;
 CREATE INDEX idx_messages_usage_session_covering ON messages(session_id, ordinal, timestamp, role, model, provider_id, claude_message_id, claude_request_id, token_usage, source_uuid) WHERE token_usage != '' AND model != '' AND model != '<synthetic>';`)
	require.NoError(t, err)
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
	removeMessageUsageCounters(t, d)
	require.NoError(t, d.Close())
	fixture, err := os.ReadFile(path)
	require.NoError(t, err)
	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	assert.True(t, d.SearchRepairPending())
	assert.Equal(t, []storedUsageCounters{{0, 0}}, readStoredUsageCounters(t, d, "s"))
	var pending bool
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&pending))
	require.True(t, pending)
	var columns int
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT count(*) FROM pragma_index_info('idx_messages_usage_session_covering')`).Scan(&columns))
	assert.Equal(t, 10, columns)
	_, err = OpenReadOnly(t.Context(), path)
	require.ErrorIs(t, err, ErrMessageUsageCountersPending)
	require.NoError(t, d.Close())
	for _, query := range []string{"daily", "session"} {
		t.Run(query, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.db")
			require.NoError(t, os.WriteFile(path, fixture, 0600))
			plainPath := filepath.Join(t.TempDir(), "archive.db")
			require.NoError(t, os.WriteFile(plainPath, fixture, 0600))
			plain, err := Open(t.Context(), plainPath)
			require.NoError(t, err)
			defer plain.Close()
			d, err := OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
			require.NoError(t, err)
			defer d.Close()
			var pending bool
			var columns int
			require.NoError(t, d.lockWriterCtx(t.Context()))
			read := func(db *DB) (any, error) {
				switch query {
				case "daily":
					return db.GetDailyUsage(t.Context(), UsageFilter{})
				default:
					return db.GetSessionUsage(t.Context(), "s", true)
				}
			}
			type result struct {
				value any
				err   error
			}
			done := make(chan result, 1)
			go func() { value, err := read(d); done <- result{value, err} }()
			select {
			case got := <-done:
				d.mu.Unlock()
				require.FailNow(t, "usage returned before fill", "%v", got.err)
			case <-time.After(50 * time.Millisecond): //nolint:kennlint // deadlinetest: writer lock holds the fill
			}
			d.mu.Unlock()
			got := <-done
			require.NoError(t, got.err)
			want, err := read(plain)
			require.NoError(t, err)
			assert.Equal(t, want, got.value)
			assert.Equal(t, []storedUsageCounters{{250, 3}}, readStoredUsageCounters(t, d, "s"))
			require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&pending))
			assert.False(t, pending)
			require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT count(*) FROM pragma_index_info('idx_messages_usage_session_covering')`).Scan(&columns))
			assert.Equal(t, 12, columns)
		})
	}
}

func TestMessageUsageCounterFillFailureReopenAndRepeatedFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	m := asstMsg("s", 0, "reply")
	m.TokenUsage = jsontext.Value(`{"output_tokens":42}`)
	insertMessages(t, d, m)
	removeMessageUsageCounters(t, d)
	var err error
	require.NoError(t, d.Close())
	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `CREATE TRIGGER fail_counter_fill BEFORE UPDATE OF usage_output_tokens ON messages BEGIN SELECT RAISE(ABORT, 'injected fill failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, d.finishUsageCounters(t.Context()), "injected fill failure")
	var stillPending bool
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM stats WHERE key='message_usage_counters_pending')`).Scan(&stillPending))
	assert.True(t, stillPending)
	_, err = d.getWriter().Exec(t.Context(), `DROP TRIGGER fail_counter_fill`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	assert.True(t, d.SearchRepairPending())
	require.NoError(t, d.Close())
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

func TestPendingMessageUsageCountersPreservesDataVersion(t *testing.T) {
	for _, archiveOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("archiveOnly=%t", archiveOnly), func(t *testing.T) {
			d := testDB(t)
			removeMessageUsageCounters(t, d)
			version := CurrentDataVersion() - 1
			if archiveOnly {
				version += archiveOnlyVersionBase
				_, err := d.getWriter().Exec(t.Context(), `INSERT OR REPLACE INTO archive_metadata(key, value) VALUES ('archive_only', '1')`)
				require.NoError(t, err)
			}
			_, err := d.getWriter().Exec(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", version))
			require.NoError(t, err)
			pending, err := addMessageUsageCounters(t.Context(), d.getWriter(), nil, true)
			require.NoError(t, err)
			require.True(t, pending)
			var raw int
			require.NoError(t, d.getReader().QueryRow(t.Context(), `PRAGMA user_version`).Scan(&raw))
			decoded, only := DecodeUserVersion(raw)
			assert.Equal(t, CurrentDataVersion()-1, decoded)
			assert.Equal(t, archiveOnly, only)
			require.NoError(t, d.setDataVersion(t.Context()))
			require.NoError(t, d.getReader().QueryRow(t.Context(), `PRAGMA user_version`).Scan(&raw))
			assert.NotZero(t, raw&usageCountersPendingVersion)
			_, only = DecodeUserVersion(raw)
			assert.Equal(t, archiveOnly, only)
			d.usageCountersPending.Store(true)
			require.NoError(t, d.finishUsageCounters(t.Context()))
			require.NoError(t, d.getReader().QueryRow(t.Context(), `PRAGMA user_version`).Scan(&raw))
			assert.Zero(t, raw&usageCountersPendingVersion)
		})
	}
}
