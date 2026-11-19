package db

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestSessionStatsFactsDeletionKeepsCountSnapshot(t *testing.T) {
	d := testDB(t)
	insertSessionFixture(t, d, sessionFixture{id: "snapshot", agent: "codex", project: "project-a", startedAt: "2026-06-16T10:00:00Z", userMsgs: 1, messageCount: 2})
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "snapshot", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-16T10:00:00Z"}, {SessionID: "snapshot", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-16T10:00:30Z", ToolCalls: []ToolCall{{ToolName: "Read", Category: "Read"}}}}))
	writer, err := OpenIsolated(t.Context(), d.Path())
	require.NoError(t, err)
	defer writer.Close()
	d.rawReader().SetMaxOpenConns(1)
	entered, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release()
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	loads := 0
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(interface {
			RegisterAuthorizer(func(int, string, string, string) int)
		}).RegisterAuthorizer(func(op int, table, column, _ string) int {
			if op == sqlite3.SQLITE_READ && table == "sessions" && column == "agent" {
				loads++
				if loads == 2 {
					close(entered)
					select {
					case <-resume:
					case <-t.Context().Done():
					}
				}
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	results := make(chan *SessionStats, 1)
	errors := make(chan error, 1)
	go func() {
		got, e := d.GetSessionStats(t.Context(), StatsFilter{Since: "2026-06-01", Until: "2026-07-01", Timezone: "UTC"})
		results <- got
		errors <- e
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "inclusive session load did not start")
	}
	require.NoError(t, writer.DeleteSession(t.Context(), "snapshot"))
	release()
	var got *SessionStats
	select {
	case got = <-results:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "report did not finish")
	}
	require.NoError(t, <-errors)
	require.InDelta(t, 1.0, got.Distributions.ToolsPerTurn.ScopeAll.Mean, 1e-9)
}

func TestSessionStatsFactsReadOnlyBeforeJournalUpgrade(t *testing.T) {
	d := testDB(t)
	seedStatsFacts(t, d)
	want, err := d.GetSessionStats(t.Context(), factsFilter)
	require.NoError(t, err)
	path := d.Path()
	require.NoError(t, d.Close())
	legacy, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), usageChangeJournalTriggerDropsSQL+`DROP TABLE usage_session_changes; DELETE FROM archive_metadata WHERE key='usage_change_revision';`)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())
	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	defer reader.Close()
	got, err := reader.GetSessionStats(t.Context(), factsFilter)
	require.NoError(t, err)
	equalStatsFacts(t, want, got)
}
