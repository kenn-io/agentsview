//go:build !(windows && arm64)

package duckdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncFullPushCreatesExpectedRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
	assertDuckDBCount(t, syncer.DB(), "messages", 3)
	assertDuckDBCount(t, syncer.DB(), "tool_calls", 1)
	assertDuckDBCount(t, syncer.DB(), "tool_result_events", 1)
	assertDuckDBCount(t, syncer.DB(), "usage_events", 1)
	assertDuckDBCount(t, syncer.DB(), "secret_findings", 1)
	assertDuckDBCount(t, syncer.DB(), "model_pricing", 1)
	assertDuckDBCount(t, syncer.DB(), "starred_sessions", 1)
	assertDuckDBCount(t, syncer.DB(), "pinned_messages", 1)

	var firstMessage string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		fixture.alphaID,
	).Scan(&firstMessage))
	assert.Equal(t, "alpha first", firstMessage)
}

func TestSyncPushContinuesAfterSessionError(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-bad", "alpha", "bad first",
				"2026-01-10T00:00:00.000Z", 1,
			),
			Messages: []db.Message{
				syncMessage(
					"duck-bad", 0, "user", "bad first",
					"not-a-timestamp",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
			IdentityObservation: export.ProjectIdentityObservation{
				SessionID: "duck-bad", Project: "alpha", Machine: "local",
				RootPath: "/repo/alpha", ObservedAt: time.Now().UTC(),
			},
		},
		{
			Session: syncSession(
				"duck-good", "alpha", "good first",
				"2026-01-11T00:00:00.000Z", 1,
			),
			Messages: []db.Message{
				syncMessage(
					"duck-good", 0, "user", "good first",
					"2026-01-11T00:00:00.000Z",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
			IdentityObservation: export.ProjectIdentityObservation{
				SessionID: "duck-good", Project: "alpha", Machine: "local",
				RootPath: "/repo/alpha", ObservedAt: time.Now().UTC(),
			},
		},
	})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	var progress []PushProgress

	result, err := syncer.Push(ctx, true, func(p PushProgress) {
		progress = append(progress, p)
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, 1, result.MessagesPushed)
	assert.Equal(t, 1, result.Errors)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", "duck-good", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", "duck-bad", 0)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", "duck-good", 1)
	assertDuckDBCount(t, syncer.DB(), "source_project_identity_observations", 0)
	assertDuckDBCount(t, syncer.DB(), "source_session_project_identity_snapshots", 0)
	require.NotEmpty(t, progress)
	last := progress[len(progress)-1]
	assert.Equal(t, 2, last.SessionsDone)
	assert.Equal(t, 2, last.SessionsTotal)
	assert.Equal(t, 1, last.MessagesDone)
	assert.Equal(t, 1, last.Errors)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)

	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			"duck-bad", "alpha", "bad repaired",
			"2026-01-10T00:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				"duck-bad", 0, "user", "bad repaired",
				"2026-01-10T00:00:00.000Z",
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Errors)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", "duck-bad", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", "duck-bad", 1)
	watermark, err = local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.NotEmpty(t, watermark)
}

func TestSyncPushSkipsCurationRefreshAfterSessionError(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-bad-curation"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "bad curation",
			"2026-01-10T00:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				sessionID, 0, "user", "bad curation",
				"not-a-timestamp",
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	ok, err := local.StarSession(sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	note := "bad pin"
	_, err = local.PinMessage(sessionID, msgs[0].ID, &note)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Errors)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", sessionID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "pinned_messages", "session_id = ?", sessionID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "starred_sessions", "session_id = ?", sessionID, 0)
}

func TestPushSessionBatchReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	var result PushResult
	var pushed []db.Session

	err := syncer.pushSessionBatch(
		ctx,
		[]db.Session{syncSession(
			"duck-canceled", "alpha", "canceled",
			"2026-01-10T00:00:00.000Z", 1,
		)},
		0, 1, &result, &pushed, nil,
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, result.Errors)
	assert.Empty(t, pushed)
}

func TestPushSessionBatchLogsAbandonedSessionsAfterContextCancel(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := newLocalDB(t)
	sessions := make([]db.Session, 0, 3)
	writes := make([]db.SessionBatchWrite, 0, 3)
	for i := range 3 {
		sessionID := fmt.Sprintf("duck-cancel-fallback-%d", i)
		sess := syncSession(
			sessionID, "alpha", "cancel fallback",
			"2026-01-10T00:00:00.000Z", 1,
		)
		sessions = append(sessions, sess)
		writes = append(writes, db.SessionBatchWrite{
			Session: sess,
			Messages: []db.Message{
				syncMessage(
					sessionID, 0, "user", "cancel fallback",
					"not-a-timestamp",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	var result PushResult
	var pushed []db.Session
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLog) })

	err = syncer.pushSessionBatch(
		ctx, sessions, 0, len(sessions), &result, &pushed,
		func(p PushProgress) {
			if p.SessionsDone == 1 {
				cancel()
			}
		},
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 3, result.Errors)
	assert.Empty(t, pushed)
	gotLogs := logs.String()
	assert.Equal(t, 1, strings.Count(gotLogs, "skipping session"))
	assert.Contains(t, gotLogs, "abandoning 2 sessions")
}

func TestPushSessionBatchBacksOffAndRetriesBatchAfterTimeout(t *testing.T) {
	ctx := context.Background()
	sessions := []db.Session{
		syncSession(
			"duck-timeout-batch-1", "alpha", "timeout one",
			"2026-01-10T00:00:00.000Z", 1,
		),
		syncSession(
			"duck-timeout-batch-2", "alpha", "timeout two",
			"2026-01-10T00:01:00.000Z", 2,
		),
	}
	var result PushResult
	var pushed []db.Session
	timeoutErr := fmt.Errorf(
		"IO Error: Failed to send message: IO Error: Timeout was reached error for HTTP POST to '<url>'",
	)
	tryCalls := 0
	waitCalls := 0
	pushSingleCalls := 0

	err := pushSessionBatchWith(
		ctx, sessions, 0, len(sessions), &result, &pushed, nil,
		func(_ context.Context, got []db.Session) ([]int, error) {
			tryCalls++
			assert.Equal(t, sessions, got)
			if tryCalls == 1 {
				return nil, timeoutErr
			}
			return []int{1, 2}, nil
		},
		func(context.Context, db.Session) (int, error) {
			pushSingleCalls++
			return 0, fmt.Errorf("individual retry should not run")
		},
		func(context.Context) error {
			waitCalls++
			return nil
		},
	)
	require.NoError(t, err)

	assert.Equal(t, 2, tryCalls)
	assert.Equal(t, 1, waitCalls)
	assert.Equal(t, 0, pushSingleCalls)
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	assert.Equal(t, sessions, pushed)
}

func TestPushSessionBatchReturnsRepeatedTimeoutWithoutIndividualRetry(t *testing.T) {
	ctx := context.Background()
	sessions := []db.Session{
		syncSession(
			"duck-timeout-batch-repeat", "alpha", "timeout",
			"2026-01-10T00:00:00.000Z", 1,
		),
	}
	var result PushResult
	var pushed []db.Session
	timeoutErr := fmt.Errorf(
		"IO Error: Failed to send message: IO Error: Timeout was reached error for HTTP POST to '<url>'",
	)
	tryCalls := 0
	pushSingleCalls := 0

	err := pushSessionBatchWith(
		ctx, sessions, 0, len(sessions), &result, &pushed, nil,
		func(context.Context, []db.Session) ([]int, error) {
			tryCalls++
			return nil, timeoutErr
		},
		func(context.Context, db.Session) (int, error) {
			pushSingleCalls++
			return 0, fmt.Errorf("individual retry should not run")
		},
		func(context.Context) error { return nil },
	)
	require.Error(t, err)

	assert.True(t, isDuckRemoteMutationTimeoutError(err))
	assert.Equal(t, 2, tryCalls)
	assert.Equal(t, 0, pushSingleCalls)
	assert.Equal(t, 0, result.Errors)
	assert.Empty(t, pushed)
}

func TestSyncPushReportsSessionDiagnosticsByAgent(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	codexID := "duck-sync-codex"
	codexSession := syncSession(
		codexID, "gamma", "codex first",
		"2026-01-12T00:00:00.000Z", 1,
	)
	codexSession.Agent = "codex"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: codexSession,
		Messages: []db.Message{
			syncMessage(
				codexID, 0, "user", "codex first",
				"2026-01-12T00:00:00.000Z",
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	wantByAgent := map[string]int{"claude": 2, "codex": 1}
	assert.True(t, result.Diagnostics.Full)
	assert.Empty(t, result.Diagnostics.LastPushAt)
	assert.Equal(t, 3, result.Diagnostics.LocalSessions.Total)
	assert.Equal(t, wantByAgent, result.Diagnostics.LocalSessions.ByAgent)
	assert.Equal(t, 3, result.Diagnostics.CandidateSessions.Total)
	assert.Equal(t, wantByAgent, result.Diagnostics.CandidateSessions.ByAgent)
	assert.Equal(t, 0, result.Diagnostics.SkippedUnchangedSessions.Total)
	assert.Empty(t, result.Diagnostics.SkippedUnchangedSessions.ByAgent)
	assert.Equal(t, 3, result.Diagnostics.PushedSessions.Total)
	assert.Equal(t, wantByAgent, result.Diagnostics.PushedSessions.ByAgent)
	assert.NotEmpty(t, result.Diagnostics.Cutoff)
}

func TestSyncPushReportsProgressAcrossBatchBoundaries(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	count := duckSessionPushBatchSize + 1
	writes := make([]db.SessionBatchWrite, 0, count)
	for i := range count {
		sessionID := fmt.Sprintf("duck-batch-%03d", i)
		ts := fmt.Sprintf("2026-01-12T00:%02d:00.000Z", i%60)
		writes = append(writes, db.SessionBatchWrite{
			Session: syncSession(
				sessionID, "alpha", "batch first", ts, 1,
			),
			Messages: []db.Message{
				syncMessage(
					sessionID, 0, "user", "batch first", ts,
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	var progress []PushProgress

	result, err := syncer.Push(ctx, true, func(p PushProgress) {
		progress = append(progress, p)
	})
	require.NoError(t, err)

	assert.Equal(t, count, result.SessionsPushed)
	assert.Equal(t, count, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	require.Len(t, progress, count)
	assert.Equal(t, duckSessionPushBatchSize, progress[duckSessionPushBatchSize-1].SessionsDone)
	assert.Equal(t, count, progress[duckSessionPushBatchSize].SessionsDone)
	assert.Equal(t, count, progress[duckSessionPushBatchSize].SessionsTotal)
	assert.Equal(t, count, progress[duckSessionPushBatchSize].MessagesDone)
}

func TestDuckValueLiteralFormatsTimestampWithoutZone(t *testing.T) {
	got, err := duckValueLiteral(time.Date(
		2026, time.January, 10, 3, 4, 5, 123456789, time.UTC,
	))
	require.NoError(t, err)

	assert.Equal(t, "TIMESTAMP '2026-01-10 03:04:05.123456'", got)
}

func TestDuckSQLWithArgsExecutesQuotedMultilineString(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)
	want := "first line\nquoted ' value\ncontains $$ delimiter text"

	stmt, err := duckSQLWithArgs(`SELECT ?`, want)
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, want, got)
}

func TestDuckSQLWithArgsStripsNULFromStringLiteral(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)

	stmt, err := duckSQLWithArgs(`SELECT ?`, "before\x00after")
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, "beforeafter", got)
}

func TestDuckSQLWithArgsExecutesStringPointer(t *testing.T) {
	ctx := context.Background()
	duck := openTestDuckDB(t)
	want := "pinned note\nquoted ' value"

	stmt, err := duckSQLWithArgs(`SELECT ?`, &want)
	require.NoError(t, err)

	var got string
	require.NoError(t, duck.QueryRowContext(ctx, stmt).Scan(&got))
	assert.Equal(t, want, got)
}

func TestExecDuckRemoteMutationBatchCoalescesSuccessfulBatch(t *testing.T) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx, `DELETE FROM remote_test WHERE id = ?`, 2)
	require.NoError(t, err)
	var calls []string

	err = execDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			calls = append(calls, sqlText)
			return nil
		},
		"test batch",
		batch,
		true,
	)
	require.NoError(t, err)

	require.Len(t, calls, 1)
	assert.Equal(t, `BEGIN TRANSACTION;
INSERT INTO remote_test VALUES (1);
DELETE FROM remote_test WHERE id = 2;
COMMIT`, calls[0])
}

func TestExecDuckRemoteMutationBatchCoalescedFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	var calls []string

	err = execDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			calls = append(calls, sqlText)
			if sqlText == "ROLLBACK" {
				return nil
			}
			return fmt.Errorf("batch failed")
		},
		"test batch",
		batch,
		true,
	)

	require.ErrorContains(t, err, "batch failed")
	require.Len(t, calls, 2)
	assert.Contains(t, calls[0], "BEGIN TRANSACTION")
	assert.Equal(t, "ROLLBACK", calls[1])
}

func TestExecDuckRemoteMutationBatchCoalescedTimeoutSkipsRollback(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	var calls []string

	err = execDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			calls = append(calls, sqlText)
			return fmt.Errorf(
				"IO Error: Failed to send message: IO Error: Timeout was reached error for HTTP POST to '<url>'",
			)
		},
		"test batch",
		batch,
		true,
	)

	require.Error(t, err)
	assert.True(t, isDuckRemoteMutationTimeoutError(err))
	assert.Equal(t, []string{batch.transactionSQL()}, calls)
}

func TestExecDuckRemoteMutationBatchCoalescedRollbackFailureIsWrapped(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)

	err = execDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			if sqlText == "ROLLBACK" {
				return fmt.Errorf("rollback failed")
			}
			return fmt.Errorf("batch failed")
		},
		"test batch",
		batch,
		true,
	)

	require.ErrorContains(t, err, "batch failed")
	assert.ErrorContains(t, err, "rollback test batch")
	assert.ErrorContains(t, err, "rollback failed")
}

func TestDuckRemoteMutationDefaultBudgetFitsQuackTimeout(t *testing.T) {
	assert.GreaterOrEqual(t, duckRemoteMutationCoalesceMaxBytes, 2<<20)
	assert.LessOrEqual(t, duckRemoteMutationCoalesceMaxBytes, 4<<20)
}

func TestDuckRemoteMutationBatchByteAccountingMatchesRenderedTransaction(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, "first",
	)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, "second",
	)
	require.NoError(t, err)

	assert.Equal(t, len(batch.transactionSQL()), batch.transactionBytes())
}

func TestDuckRemoteMutationBatchCoalescesAdjacentInsertStatements(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx,
		`DELETE FROM remote_test WHERE session_id = ?`, "session-1",
	)
	require.NoError(t, err)
	for i := range 3 {
		_, err = batch.ExecContext(ctx,
			`INSERT INTO remote_test (session_id, ordinal) VALUES (?, ?)`,
			"session-1", i,
		)
		require.NoError(t, err)
	}

	sqlText := batch.transactionSQL()

	assert.Equal(t, 1, strings.Count(sqlText, "INSERT INTO remote_test"))
	assert.Contains(t, sqlText,
		"INSERT INTO remote_test (session_id, ordinal) VALUES ("+
			"$agentsview_")
	assert.Contains(t, sqlText, ", 0), (")
	assert.Contains(t, sqlText, ", 1), (")
	assert.Contains(t, sqlText, ", 2);")
	assert.Less(t, batch.transactionBytes(), len(strings.Join(batch.statements, ";\n")))
}

func TestDuckRemoteMutationBatchInsertCoalescingSplitsByRowLimit(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	for i := range 257 {
		_, err := batch.ExecContext(ctx,
			`INSERT INTO remote_test (session_id, ordinal) VALUES (?, ?)`,
			"session-1", i,
		)
		require.NoError(t, err)
	}

	sqlText := batch.transactionSQL()

	assert.Equal(t, 2, strings.Count(sqlText, "INSERT INTO remote_test"))
}

func TestDuckRemoteMutationBatchCoalescedInsertMatchesPerRowExecution(
	t *testing.T,
) {
	ctx := context.Background()
	coalescedDB := openTestDuckDB(t)
	perRowDB := openTestDuckDB(t)
	for _, conn := range []*sql.DB{coalescedDB, perRowDB} {
		_, err := conn.ExecContext(ctx,
			`CREATE TABLE remote_test (
				session_id VARCHAR,
				ordinal INTEGER,
				content VARCHAR
			)`,
		)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			`INSERT INTO remote_test VALUES ('session-1', 99, 'stale')`,
		)
		require.NoError(t, err)
	}
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx,
		`DELETE FROM remote_test WHERE session_id = ?`, "session-1",
	)
	require.NoError(t, err)
	for _, row := range []struct {
		ordinal int
		content string
	}{
		{ordinal: 0, content: "first"},
		{ordinal: 1, content: "second"},
		{ordinal: 2, content: "third"},
	} {
		_, err = batch.ExecContext(ctx,
			`INSERT INTO remote_test (session_id, ordinal, content) VALUES (?, ?, ?)`,
			"session-1", row.ordinal, row.content,
		)
		require.NoError(t, err)
	}

	_, err = coalescedDB.ExecContext(ctx, batch.transactionSQL())
	require.NoError(t, err)
	require.NoError(t, execDuckRemoteMutationBatch(
		ctx,
		func(ctx context.Context, sqlText string) error {
			_, err := perRowDB.ExecContext(ctx, sqlText)
			return err
		},
		"per-row equivalence",
		batch,
		false,
	))

	assert.Equal(
		t,
		readRemoteTestRows(t, ctx, perRowDB),
		readRemoteTestRows(t, ctx, coalescedDB),
	)
}

type remoteTestRow struct {
	SessionID string
	Ordinal   int
	Content   string
}

func readRemoteTestRows(
	t *testing.T, ctx context.Context, conn *sql.DB,
) []remoteTestRow {
	t.Helper()
	rows, err := conn.QueryContext(ctx,
		`SELECT session_id, ordinal, content
		 FROM remote_test
		 ORDER BY session_id, ordinal, content`,
	)
	require.NoError(t, err)
	defer rows.Close()
	var out []remoteTestRow
	for rows.Next() {
		var row remoteTestRow
		require.NoError(t, rows.Scan(
			&row.SessionID, &row.Ordinal, &row.Content,
		))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestDuckRemoteMutationBatchFallbackKeepsPerRowInsertAttribution(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 2)
	require.NoError(t, err)
	var coalescedCalls []string
	var statementCalls []string

	err = execDuckRemoteMutationBatchWithStatementFallback(
		ctx,
		func(_ context.Context, sqlText string) error {
			coalescedCalls = append(coalescedCalls, sqlText)
			if sqlText == "ROLLBACK" {
				return nil
			}
			return fmt.Errorf("coalesced failure")
		},
		func(_ context.Context, sqlText string) error {
			statementCalls = append(statementCalls, sqlText)
			if sqlText == "INSERT INTO remote_test VALUES (2)" {
				return fmt.Errorf("poison row")
			}
			return nil
		},
		"test session",
		batch,
	)
	require.Error(t, err)

	require.NotEmpty(t, coalescedCalls)
	assert.Equal(t, 1, strings.Count(coalescedCalls[0], "INSERT INTO remote_test"))
	assert.Contains(t, err.Error(), "execute test session statement 2/2")
	assert.Equal(t, []string{
		"BEGIN TRANSACTION",
		"INSERT INTO remote_test VALUES (1)",
		"INSERT INTO remote_test VALUES (2)",
		"ROLLBACK",
	}, statementCalls)
}

func TestDuckRemoteMutationBatchCombinedTransactionBytesUsesCachedRendering(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	for i := range 50 {
		_, err := batch.ExecContext(ctx,
			`INSERT INTO remote_test VALUES (?, ?)`, i, strings.Repeat("x", 32),
		)
		require.NoError(t, err)
	}
	next := &duckRemoteMutationBatch{}
	_, err := next.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 51, strings.Repeat("y", 32),
	)
	require.NoError(t, err)

	require.Positive(t, batch.transactionBytes())
	require.Positive(t, next.transactionBytes())
	allocs := testing.AllocsPerRun(100, func() {
		_ = batch.combinedTransactionBytes(next)
	})

	assert.Zero(t, allocs)
}

func TestDuckRemoteMutationBatchAppendBatchPreservesRenderedCache(
	t *testing.T,
) {
	ctx := context.Background()
	current := &duckRemoteMutationBatch{}
	first := &duckRemoteMutationBatch{}
	_, err := first.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, strings.Repeat("a", 32),
	)
	require.NoError(t, err)
	second := &duckRemoteMutationBatch{}
	_, err = second.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, strings.Repeat("b", 32),
	)
	require.NoError(t, err)
	third := &duckRemoteMutationBatch{}
	_, err = third.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 3, strings.Repeat("c", 32),
	)
	require.NoError(t, err)

	current.appendBatch(first)
	require.Positive(t, current.combinedTransactionBytes(second))
	current.appendBatch(second)
	require.Positive(t, third.transactionBytes())
	allocs := testing.AllocsPerRun(100, func() {
		_ = current.combinedTransactionBytes(third)
	})

	assert.Zero(t, allocs)
}

func TestDuckRemoteMutationBatchAppendPreservesRenderedCache(t *testing.T) {
	ctx := context.Background()
	current := &duckRemoteMutationBatch{}
	_, err := current.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, strings.Repeat("x", 32),
	)
	require.NoError(t, err)
	next := &duckRemoteMutationBatch{}
	_, err = next.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, strings.Repeat("y", 32),
	)
	require.NoError(t, err)

	require.Positive(t, current.combinedTransactionBytes(next))
	require.True(t, current.renderedValid)
	current.appendBatch(next)

	assert.True(t, current.renderedValid)
	assert.Len(t, current.rendered(), 2)
}

func TestSplitDuckRemoteSimpleInsertAllocatesOnlyReturnedPrefix(
	t *testing.T,
) {
	literal, err := duckRemoteStringLiteral(strings.Repeat("x", 4096))
	require.NoError(t, err)
	stmt := "INSERT INTO remote_test VALUES (" + literal + ")"
	prefix, tuple, ok := splitDuckRemoteSimpleInsert(stmt)
	require.True(t, ok)
	assert.Equal(t, "INSERT INTO remote_test VALUES ", prefix)
	assert.Contains(t, tuple, strings.Repeat("x", 128))

	allocs := testing.AllocsPerRun(100, func() {
		_, _, _ = splitDuckRemoteSimpleInsert(stmt)
	})

	assert.LessOrEqual(t, allocs, 1.0)
}

func TestAppendDuckRemoteMutationBatchFlushesBeforeByteBudget(t *testing.T) {
	ctx := context.Background()
	first := &duckRemoteMutationBatch{}
	_, err := first.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, "first",
	)
	require.NoError(t, err)
	second := &duckRemoteMutationBatch{}
	_, err = second.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, "second",
	)
	require.NoError(t, err)
	maxBytes := first.combinedTransactionBytes(second) - 1
	current := &duckRemoteMutationBatch{}
	var calls []string
	exec := func(_ context.Context, sqlText string) error {
		calls = append(calls, sqlText)
		return nil
	}

	current, err = appendDuckRemoteMutationBatch(
		ctx, exec, "test batch", current, first, maxBytes,
	)
	require.NoError(t, err)
	current, err = appendDuckRemoteMutationBatch(
		ctx, exec, "test batch", current, second, maxBytes,
	)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0], "$")
	assert.Contains(t, calls[0], "first")
	assert.NotContains(t, calls[0], "second")

	require.NoError(t, execDuckRemoteMutationBatch(
		ctx, exec, "test batch", current, true,
	))
	require.Len(t, calls, 2)
	assert.Contains(t, calls[1], "second")
}

func TestExecDuckRemoteMutationBatchOversizeUsesStatementModeTransaction(
	t *testing.T,
) {
	ctx := context.Background()
	oversize := &duckRemoteMutationBatch{}
	_, err := oversize.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, "aaaaa",
	)
	require.NoError(t, err)
	_, err = oversize.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, "bbbbb",
	)
	require.NoError(t, err)
	_, err = oversize.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 3, "ccccc",
	)
	require.NoError(t, err)
	oneStatement := &duckRemoteMutationBatch{statements: oversize.statements[:1]}
	maxBytes := oneStatement.transactionBytes()
	var coalescedCalls []string
	var statementCalls []string

	err = execDuckRemoteMutationBatchOversizeWithStatementFallback(
		ctx,
		func(_ context.Context, sqlText string) error {
			coalescedCalls = append(coalescedCalls, sqlText)
			return nil
		},
		func(_ context.Context, sqlText string) error {
			statementCalls = append(statementCalls, sqlText)
			return nil
		},
		"oversize session",
		oversize,
		maxBytes,
	)
	require.NoError(t, err)

	assert.Empty(t, coalescedCalls)
	require.Len(t, statementCalls, 3)
	assert.Equal(t, "BEGIN TRANSACTION", statementCalls[0])
	assert.Contains(t, statementCalls[1], "INSERT INTO remote_test VALUES (1, $")
	assert.Contains(t, statementCalls[1], "), (2, $")
	assert.Contains(t, statementCalls[1], "), (3, $")
	assert.Equal(t, "COMMIT", statementCalls[2])
}

func TestAppendDuckRemoteMutationBatchRejectsOversizeSessionAfterFlush(
	t *testing.T,
) {
	ctx := context.Background()
	current := &duckRemoteMutationBatch{}
	_, err := current.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 1, "current",
	)
	require.NoError(t, err)
	oversize := &duckRemoteMutationBatch{}
	_, err = oversize.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 2, "bbbbb",
	)
	require.NoError(t, err)
	_, err = oversize.ExecContext(ctx,
		`INSERT INTO remote_test VALUES (?, ?)`, 3, "ccccc",
	)
	require.NoError(t, err)
	oneStatement := &duckRemoteMutationBatch{statements: oversize.statements[:1]}
	maxBytes := oneStatement.transactionBytes()
	var calls []string

	next, err := appendDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			calls = append(calls, sqlText)
			return nil
		},
		"oversize session",
		current,
		oversize,
		maxBytes,
	)
	require.ErrorContains(t, err, "exceeds remote mutation coalesce budget")

	assert.Equal(t, 0, next.Len())
	require.Len(t, calls, 1)
	assert.Equal(t, current.transactionSQL(), calls[0])
}

func TestExecDuckRemoteMutationBatchStatementModePinpointsFailure(t *testing.T) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx, `DELETE FROM remote_test WHERE id = ?`, 2)
	require.NoError(t, err)
	var calls []string

	err = execDuckRemoteMutationBatch(
		ctx,
		func(_ context.Context, sqlText string) error {
			calls = append(calls, sqlText)
			if strings.HasPrefix(sqlText, "DELETE") {
				return fmt.Errorf("delete failed")
			}
			return nil
		},
		"test session",
		batch,
		false,
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "execute test session statement 2/2")
	assert.Equal(t, []string{
		"BEGIN TRANSACTION",
		"INSERT INTO remote_test VALUES (1)",
		"DELETE FROM remote_test WHERE id = 2",
		"ROLLBACK",
	}, calls)
}

func TestExecDuckRemoteMutationBatchFallbackLocalizesNonStaleFailure(t *testing.T) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx, `DELETE FROM remote_test WHERE id = ?`, 2)
	require.NoError(t, err)
	var coalescedCalls []string
	var statementCalls []string

	err = execDuckRemoteMutationBatchWithStatementFallback(
		ctx,
		func(_ context.Context, sqlText string) error {
			coalescedCalls = append(coalescedCalls, sqlText)
			if sqlText == "ROLLBACK" {
				return nil
			}
			return fmt.Errorf("coalesced failure")
		},
		func(_ context.Context, sqlText string) error {
			statementCalls = append(statementCalls, sqlText)
			if strings.HasPrefix(sqlText, "DELETE") {
				return fmt.Errorf("delete failed")
			}
			return nil
		},
		"test session",
		batch,
	)
	require.Error(t, err)

	assert.Equal(t, []string{batch.transactionSQL(), "ROLLBACK"}, coalescedCalls)
	assert.Contains(t, err.Error(), "execute test session statement 2/2")
	assert.Equal(t, []string{
		"BEGIN TRANSACTION",
		"INSERT INTO remote_test VALUES (1)",
		"DELETE FROM remote_test WHERE id = 2",
		"ROLLBACK",
	}, statementCalls)
}

func TestExecDuckRemoteMutationBatchFallbackDoesNotRetryStaleFailure(t *testing.T) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	var statementCalls []string

	err = execDuckRemoteMutationBatchWithStatementFallback(
		ctx,
		func(_ context.Context, sqlText string) error {
			if sqlText == "ROLLBACK" {
				return nil
			}
			return fmt.Errorf("Invalid Input Error: Invalid connection id")
		},
		func(_ context.Context, sqlText string) error {
			statementCalls = append(statementCalls, sqlText)
			return nil
		},
		"test session",
		batch,
	)
	require.ErrorContains(t, err, "Invalid connection id")
	assert.Empty(t, statementCalls)
}

func TestExecDuckRemoteMutationBatchFallbackDoesNotRetryTimeout(
	t *testing.T,
) {
	ctx := context.Background()
	batch := &duckRemoteMutationBatch{}
	_, err := batch.ExecContext(ctx, `INSERT INTO remote_test VALUES (?)`, 1)
	require.NoError(t, err)
	var coalescedCalls []string
	var statementCalls []string

	err = execDuckRemoteMutationBatchWithStatementFallback(
		ctx,
		func(_ context.Context, sqlText string) error {
			coalescedCalls = append(coalescedCalls, sqlText)
			return fmt.Errorf(
				"IO Error: Failed to send message: IO Error: Timeout was reached error for HTTP POST to '<url>'",
			)
		},
		func(_ context.Context, sqlText string) error {
			statementCalls = append(statementCalls, sqlText)
			return nil
		},
		"test session",
		batch,
	)
	require.Error(t, err)

	assert.True(t, isDuckRemoteMutationTimeoutError(err))
	assert.Equal(t, []string{batch.transactionSQL()}, coalescedCalls)
	assert.Empty(t, statementCalls)
}

func TestDuckValueLiteralFormatsNullableNumericPointers(t *testing.T) {
	score := 88
	fileSize := int64(4096)
	contextPressure := 0.875

	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "int pointer", in: &score, want: "88"},
		{name: "int64 pointer", in: &fileSize, want: "4096"},
		{name: "float64 pointer", in: &contextPressure, want: "0.875"},
		{name: "nil int pointer", in: (*int)(nil), want: "NULL"},
		{name: "nil int64 pointer", in: (*int64)(nil), want: "NULL"},
		{name: "nil float64 pointer", in: (*float64)(nil), want: "NULL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := duckValueLiteral(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSessionFingerprintsStoreDigestOnly(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	session, err := local.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, session)

	got, err := syncer.sessionFingerprints(ctx, []db.Session{*session})
	require.NoError(t, err)

	fp := got[fixture.alphaID]
	assert.Len(t, fp, 64)
	assert.False(t, strings.Contains(fp, "alpha first"))
	assert.False(t, strings.Contains(fp, "secret token sk-duckdb"))
	assert.False(t, strings.Contains(fp, "duck result"))
	assert.False(t, strings.Contains(fp, "pin alpha"))
}

func TestDuckSessionFingerprintFieldsDiffer(t *testing.T) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}
	encode := func(s db.Session) string {
		data, err := json.Marshal(duckSessionFingerprintFields(s, "laptop"))
		require.NoError(t, err)
		return string(data)
	}
	fp1 := encode(base)

	tests := []struct {
		name   string
		modify func(s db.Session) db.Session
	}{
		{
			name: "display name change",
			modify: func(s db.Session) db.Session {
				name := "new name"
				s.DisplayName = &name
				return s
			},
		},
		{
			name: "session_name change",
			modify: func(s db.Session) db.Session {
				n := "agent-provided-title"
				s.SessionName = &n
				return s
			},
		},
		{
			name: "transcript revision change",
			modify: func(s db.Session) db.Session {
				revision := "1"
				s.TranscriptRevision = &revision
				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEqual(t, fp1, encode(tt.modify(base)))
		})
	}
}

func TestTranscriptRevisionBackfillForcesOneFullDuckDBPush(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{
		SyncStateTarget: "target-a",
	})
	backfillKey := "duckdb_transcript_revision_backfill_v1:target-a"

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	msgs[0].Content = "revised transcript"
	require.NoError(t, local.ReplaceSessionMessages(fixture.alphaID, msgs))
	localSession, err := local.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, localSession)
	require.NotNil(t, localSession.TranscriptRevision)
	require.NoError(t, local.SetSyncState(
		backfillKey, "",
	))

	backfill, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, backfill.Diagnostics.Full)
	assert.Equal(t, 2, backfill.SessionsPushed)

	var revision string
	require.NoError(t, syncer.DB().QueryRowContext(
		ctx,
		`SELECT transcript_revision FROM sessions WHERE id = ?`,
		fixture.alphaID,
	).Scan(&revision))
	assert.Equal(t, *localSession.TranscriptRevision, revision)

	done, err := local.GetSyncState(backfillKey)
	require.NoError(t, err)
	assert.Equal(t, "1", done)

	incremental, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, incremental.Diagnostics.Full)
}

func TestWriteSyncFingerprintsNormalizesRetainedLegacyValues(t *testing.T) {
	local := newLocalDB(t)
	legacy := `{"Messages":[{"content":"secret token sk-legacy"}]}`

	require.NoError(t, writeSyncFingerprints(
		local,
		lastPushBoundaryStateKey,
		"2026-01-10T00:00:00.000Z",
		nil,
		map[string]string{"unchanged": legacy},
		nil,
	))

	raw, err := local.GetSyncState(lastPushBoundaryStateKey)
	require.NoError(t, err)
	require.NotContains(t, raw, "secret token sk-legacy")
	var state syncState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	fp := state.Fingerprints["unchanged"]
	require.Len(t, fp, 64)
	assert.NotEqual(t, legacy, fp)
}

func TestSyncUsesFallbackPricingWhenLocalPricingIsEmpty(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-fallback-pricing"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "pricing", "2026-01-10T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "assistant", "pricing", "2026-01-10T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	var count int
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_pricing WHERE model_pattern = ?`,
		"claude-sonnet-4-6",
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestSyncModelPricingPreservesExistingMirrorRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-preserve-pricing"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "pricing", "2026-01-10T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "assistant", "pricing", "2026-01-10T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('other-machine-model', 1, 2, 3, 4, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	var input, output float64
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_per_mtok, output_per_mtok
		 FROM model_pricing WHERE model_pattern = ?`,
		"other-machine-model",
	).Scan(&input, &output))
	assert.Equal(t, 1.0, input)
	assert.Equal(t, 2.0, output)
}

func TestSyncScrubsLegacyProjectIdentityGitRemoteCredentials(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	observedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	rawRemote := "https://" + "user:token@" + "github.com/acme/app.git"
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:       "alpha",
			Machine:       "laptop",
			RootPath:      "/tmp/app",
			GitRemote:     rawRemote,
			GitRemoteName: "origin",
			ObservedAt:    observedAt,
		},
	))
	target := filepath.Join(t.TempDir(), "legacy-project-identity.duckdb")
	legacy := newTestSync(t, target, local, SyncOptions{})
	require.NoError(t, legacy.EnsureSchema(ctx))
	_, err := legacy.DB().ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		projectIdentityRemoteScrubMetadataKey,
	)
	require.NoError(t, err)
	_, err = legacy.DB().ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "laptop", "/tmp/app", rawRemote, "origin",
		"", "", observedAt, "", "", "",
	)
	require.NoError(t, err)
	_, err = legacy.DB().ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "laptop", "/tmp/app", "", "",
		"app", "/tmp/app", observedAt.Add(-time.Hour), "", "", "",
	)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())

	syncer := newTestSync(t, target, local, SyncOptions{})

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	var remotes []string
	rows, err := syncer.DB().QueryContext(ctx, `
		SELECT git_remote
		FROM source_project_identity_observations
		WHERE source_archive_id != ''
		  AND project = ? AND machine = ? AND root_path = ?
		ORDER BY git_remote`,
		"alpha", "laptop", "/tmp/app",
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var remote string
		require.NoError(t, rows.Scan(&remote))
		remotes = append(remotes, remote)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"https://github.com/acme/app.git"}, remotes)
}

func TestSyncMirrorsSessionProjectIdentitySnapshotsByArchiveGeneration(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "snapshot-session", Project: "app", Machine: "laptop", Agent: "codex",
	}))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "snapshot-session", Project: "app", Machine: "laptop",
			RootPath: "/workspace/app", GitRemote: "https://github.com/acme/app.git",
			GitRemoteName: "origin", RepositoryPath: "/workspace/app/.git",
			WorktreeRootPath:     "/workspace/app",
			WorktreeRelationship: export.WorktreeMain,
			CheckoutState:        export.CheckoutDetached,
			RemoteResolution:     export.ProjectResolutionResolved,
			ObservedAt:           time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		},
	))
	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err)
	generation, err := local.GetDatabaseID(ctx)
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))

	var gotArchive, gotGeneration, gotSession, gotRemote string
	var gotRelationship export.WorktreeRelationship
	var gotCheckout export.CheckoutState
	err = syncer.DB().QueryRowContext(ctx, `
		SELECT source_archive_id, source_database_generation,
			source_session_id, git_remote, worktree_relationship, checkout_state
		FROM source_session_project_identity_snapshots
		WHERE source_session_id = ?`, "snapshot-session",
	).Scan(
		&gotArchive, &gotGeneration, &gotSession, &gotRemote,
		&gotRelationship, &gotCheckout,
	)
	require.NoError(t, err)
	assert.Equal(t, archiveID, gotArchive)
	assert.Equal(t, generation, gotGeneration)
	assert.Equal(t, "snapshot-session", gotSession)
	assert.Equal(t, "https://github.com/acme/app.git", gotRemote)
	assert.Equal(t, export.WorktreeMain, gotRelationship)
	assert.Equal(t, export.CheckoutDetached, gotCheckout)

	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE source_session_project_identity_snapshots
		SET git_remote = 'sentinel'
		WHERE source_session_id = ?`, "snapshot-session")
	require.NoError(t, err)
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = ?`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "sentinel", gotRemote,
		"unchanged local revision should skip mirror publication")
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, true))
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = ?`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "https://github.com/acme/app.git", gotRemote,
		"forced publication should rebuild mirror identity rows")

	require.NoError(t, local.DeleteSession("snapshot-session"))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	assertDuckDBCountWhere(t, syncer.DB(),
		"source_session_project_identity_snapshots",
		"source_archive_id = ?", archiveID, 0,
	)
}

func TestSyncPreservesAmbiguousIdentityAlongsideResolvedRemote(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			GitRemote:        "https://example.com/acme/app.git",
			RemoteResolution: export.ProjectResolutionResolved,
		},
	))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			RemoteResolution:     export.ProjectResolutionAmbiguous,
			RemoteCandidateCount: 2,
		},
	))

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, true))

	got, err := NewStoreFromDB(syncer.DB()).BuildProjectIdentityMap(
		ctx, []string{"app"},
	)
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous, got["app"].Resolution)
	assert.Nil(t, got["app"].Identity)
}

func TestFilteredThenUnfilteredIdentityPublicationIncludesExcludedProject(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	for _, project := range []string{"alpha", "beta"} {
		sessionID := "identity-" + project
		require.NoError(t, local.UpsertSession(db.Session{
			ID: sessionID, Project: project, Machine: "laptop", Agent: "codex",
		}))
		require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: sessionID, Project: project, Machine: "laptop",
				RootPath:         "/workspace/" + project,
				GitRemote:        "https://example.com/" + project + ".git",
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
			},
		))
	}
	target := filepath.Join(t.TempDir(), "identity-filter.duckdb")
	filtered := newTestSync(t, target, local, SyncOptions{
		Projects: []string{"alpha"},
	})
	require.NoError(t, filtered.EnsureSchema(ctx))
	require.NoError(t, filtered.syncProjectIdentityObservations(ctx, false))
	_, err := filtered.DB().ExecContext(ctx, `
		UPDATE source_project_identity_observations
		SET git_remote_name = 'sentinel'
		WHERE project = 'alpha'`)
	require.NoError(t, err)
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-beta", Project: "beta", Machine: "laptop",
			RootPath:         "/workspace/beta",
			GitRemote:        "https://example.com/beta.git",
			GitRemoteName:    "upstream",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC),
		},
	))
	require.NoError(t, filtered.syncProjectIdentityObservations(ctx, false))
	var alphaRemoteName string
	require.NoError(t, filtered.DB().QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'alpha'`).Scan(&alphaRemoteName))
	assert.Equal(t, "sentinel", alphaRemoteName,
		"out-of-scope changes must not republish the filtered identity scope")
	require.NoError(t, filtered.Close())

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	require.NoError(t, unfiltered.EnsureSchema(ctx))
	require.NoError(t, unfiltered.syncProjectIdentityObservations(ctx, false))
	assertDuckDBCountWhere(t, unfiltered.DB(),
		"source_session_project_identity_snapshots",
		"project = ?", "beta", 1,
	)
}

func TestIdentityPublicationUpdatesOnlyChangedRowsAndAppliesTombstones(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	observedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	for _, project := range []string{"alpha", "beta"} {
		sessionID := "identity-" + project
		require.NoError(t, local.UpsertSession(db.Session{
			ID: sessionID, Project: project, Machine: "laptop", Agent: "codex",
		}))
		require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: sessionID, Project: project, Machine: "laptop",
				RootPath:         "/workspace/" + project,
				GitRemote:        "https://example.com/" + project + ".git",
				GitRemoteName:    "origin",
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       observedAt,
			},
		))
	}
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "identity-gamma", Project: "gamma", Machine: "laptop", Agent: "codex",
	}))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-gamma", Project: "gamma", Machine: "laptop",
			RootPath:         "/workspace/gamma",
			RemoteResolution: export.ProjectResolutionUnknown,
			ObservedAt:       observedAt,
		},
	))

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	_, err := syncer.DB().ExecContext(ctx, `
		UPDATE source_project_identity_observations
		SET git_remote_name = 'sentinel'
		WHERE project = 'beta'`)
	require.NoError(t, err)

	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-alpha", Project: "alpha", Machine: "laptop",
			RootPath:         "/workspace/alpha",
			GitRemote:        "https://example.com/alpha.git",
			GitRemoteName:    "upstream",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       observedAt.Add(time.Hour),
		},
	))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-gamma", Project: "gamma", Machine: "laptop",
			RootPath:         "/workspace/gamma",
			GitRemote:        "https://example.com/gamma.git",
			GitRemoteName:    "origin",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       observedAt.Add(time.Hour),
		},
	))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))

	var alphaRemoteName, betaRemoteName string
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'alpha'`).Scan(&alphaRemoteName))
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'beta'`).Scan(&betaRemoteName))
	assert.Equal(t, "upstream", alphaRemoteName)
	assert.Equal(t, "sentinel", betaRemoteName,
		"incremental publication must not rewrite unchanged rows")
	var gammaFallbacks, gammaRemotes int
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_project_identity_observations
		WHERE project = 'gamma' AND git_remote = ''`).Scan(&gammaFallbacks))
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_project_identity_observations
		WHERE project = 'gamma' AND git_remote = ?`,
		"https://example.com/gamma.git").Scan(&gammaRemotes))
	assert.Zero(t, gammaFallbacks)
	assert.Equal(t, 1, gammaRemotes)

	require.NoError(t, local.DeleteSession("identity-alpha"))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	assertDuckDBCountWhere(t, syncer.DB(),
		"source_session_project_identity_snapshots",
		"source_session_id = ?", "identity-alpha", 0,
	)
}

func TestSyncModelPricingSkipsUnchangedMirrorRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         3,
		OutputPerMTok:        15,
		CacheCreationPerMTok: 1,
		CacheReadPerMTok:     0.5,
	}}))
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, syncer.syncModelPricing(ctx))
	_, err := syncer.DB().ExecContext(ctx,
		`UPDATE model_pricing SET updated_at = ? WHERE model_pattern = ?`,
		"kept", "claude-test",
	)
	require.NoError(t, err)

	require.NoError(t, syncer.syncModelPricing(ctx))

	var updatedAt string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT updated_at FROM model_pricing WHERE model_pattern = ?`,
		"claude-test",
	).Scan(&updatedAt))
	assert.Equal(t, "kept", updatedAt)
}

func TestSyncIncrementalSkipsUnchangedAndPushesChangedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assert.Equal(t, 0, second.MessagesPushed)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha changed", modifiedAt)
	third, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, third.SessionsPushed)
	assert.Equal(t, 1, third.MessagesPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		fixture.alphaID,
	).Scan(&content))
	assert.Equal(t, "alpha changed", content)
}

func TestSyncIncrementalPushesSameLengthContentChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-same-length"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "first", "2026-01-18T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "abcde", "2026-01-18T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, sessionID, "vwxyz", modifiedAt)
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		sessionID,
	).Scan(&content))
	assert.Equal(t, "vwxyz", content)
}

func TestSyncIncrementalRechecksLastPushBoundary(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	boundary := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha boundary", boundary)
	require.NoError(t, local.SetSyncState(lastPushStateKey, boundary))
	require.NoError(t, local.SetSyncState(lastPushBoundaryStateKey, ""))

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		fixture.alphaID,
	).Scan(&content))
	assert.Equal(t, "alpha boundary", content)
}

func TestSyncResetTargetForcesFullPushWhenWatermarkExists(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	_, err = syncer.DB().ExecContext(ctx, `DELETE FROM sessions`)
	require.NoError(t, err)

	reset, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, reset.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
}

func TestSyncResetTargetIgnoresOtherMachineRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	insertOtherMachineDuckSession(t, syncer.DB())
	require.NoError(t, local.SetSyncState(
		lastPushStateKey,
		time.Now().UTC().Format(localSyncTimestampLayout),
	))

	reset, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, reset.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "machine = ?", "test-machine", 2)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "machine = ?", "other-machine", 1)
}

func TestSyncFullPushRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	require.NoError(t, local.SoftDeleteSession(fixture.betaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, second.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCount(t, syncer.DB(), "messages", 2)
}

func TestSyncFullPushPreservesOtherMachineRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	insertOtherMachineDuckSession(t, syncer.DB())

	second, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, second.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "starred_sessions", "session_id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "pinned_messages", "session_id = ?", "other-session", 1)
}

func TestSyncIncrementalPushRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	require.NoError(t, local.SoftDeleteSession(fixture.betaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", fixture.betaID, 0)
}

func TestSyncIncrementalUpdatesPinsWithoutSessionChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "updated duck pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)

	var got string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		fixture.alphaID, msgs[0].ID,
	).Scan(&got))
	assert.Equal(t, note, got)
}

func TestClearSessionTablesRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	tx, err := syncer.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, clearSessionTables(ctx, tx))
	require.NoError(t, tx.Rollback())

	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
	assertDuckDBCount(t, syncer.DB(), "messages", 3)
	assertDuckDBCount(t, syncer.DB(), "usage_events", 1)
}

func clearSessionTables(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages`,
		`DELETE FROM secret_findings`,
		`DELETE FROM tool_result_events`,
		`DELETE FROM tool_calls`,
		`DELETE FROM usage_events`,
		`DELETE FROM messages`,
		`DELETE FROM sessions`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("clearing duckdb full-push session table: %w", err)
		}
	}
	return nil
}

func TestSyncProjectFiltersMatchPushScope(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)

	include := newInMemoryTestSync(t, local, SyncOptions{Projects: []string{"alpha"}})
	result, err := include.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assertDuckDBCount(t, include.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, include.DB(), "sessions", "project = ?", "alpha", 1)

	exclude := newInMemoryTestSync(t, local, SyncOptions{ExcludeProjects: []string{"alpha"}})
	result, err = exclude.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assertDuckDBCount(t, exclude.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, exclude.DB(), "sessions", "project = ?", "beta", 1)
}

func TestSyncFilteredFullClearsGlobalWatermarkForLaterUnfilteredPush(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	ok, err := local.StarSession(fixture.betaID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))
	require.NoError(t, local.SetSyncState(lastPushBoundaryStateKey, `{"fingerprints":{"stale":"stale"}}`))

	target := filepath.Join(t.TempDir(), "filtered.duckdb")
	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	first, err := filtered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	assertDuckDBCountWhere(t, filtered.DB(), "sessions", "id = ?", fixture.alphaID, 1)
	assertDuckDBCountWhere(t, filtered.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCount(t, filtered.DB(), "starred_sessions", 1)
	assertDuckDBCountWhere(t, filtered.DB(), "starred_sessions", "session_id = ?", fixture.betaID, 0)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)
	fingerprints, err := readSyncFingerprintsWithKey(local, lastPushBoundaryStateKey)
	require.NoError(t, err)
	assert.Contains(t, fingerprints, fixture.alphaID)
	assert.NotContains(t, fingerprints, "stale")
	require.NoError(t, filtered.Close())

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	second, err := unfiltered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, second.SessionsPushed)
	assertDuckDBCountWhere(t, unfiltered.DB(), "sessions", "id = ?", fixture.betaID, 1)
}

func TestSyncFilteredFullPushKeepsNextFilteredPushIncremental(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assert.Equal(t, 0, second.MessagesPushed)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)
}

func TestSyncFilteredIncrementalPersistsFingerprintsWithoutAdvancingWatermark(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "filtered-watermark.duckdb")

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	first, err := unfiltered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	require.NoError(t, unfiltered.Close())

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	require.NotEmpty(t, watermark)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha filtered change", modifiedAt)

	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	second, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)

	got, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Equal(t, watermark, got)

	third, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, third.SessionsPushed)
}

func TestSyncFilteredIncrementalUpdatesPinsWithoutSessionChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	note := "filtered updated pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)

	var got string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		fixture.alphaID, msgs[0].ID,
	).Scan(&got))
	assert.Equal(t, note, got)
}

func TestSyncFilteredIncrementalRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))

	require.NoError(t, local.SoftDeleteSession(fixture.alphaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.alphaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_calls", "session_id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_result_events", "session_id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "starred_sessions", "session_id = ?", fixture.alphaID, 0)
}

func TestSyncFilteredPushPreservesOutOfScopeStarredSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	ok, err := local.StarSession(fixture.betaID)
	require.NoError(t, err)
	require.True(t, ok)
	target := filepath.Join(t.TempDir(), "filtered-stars.duckdb")

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	first, err := unfiltered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	assertDuckDBCount(t, unfiltered.DB(), "starred_sessions", 2)
	require.NoError(t, unfiltered.Close())

	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	second, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assertDuckDBCount(t, filtered.DB(), "starred_sessions", 2)
	assertDuckDBCountWhere(t, filtered.DB(), "starred_sessions", "session_id = ?", fixture.betaID, 1)
	assert.Equal(t, 0, second.SessionsPushed)
}

func TestSyncStatusCountsDuckDBRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	insertOtherMachineDuckSession(t, syncer.DB())

	status, err := syncer.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-machine", status.Machine)
	assert.NotEmpty(t, status.LastPushAt)
	assert.Equal(t, 2, status.DuckDBSessions)
	assert.Equal(t, 3, status.DuckDBMessages)
}

func TestReadStatusFromConfigCountsMachineScopedDuckDBRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "status.duckdb")
	syncer := newTestSync(t, target, local, SyncOptions{})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	insertOtherMachineDuckSession(t, syncer.DB())
	require.NoError(t, syncer.Close())

	status, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        target,
		MachineName: "test-machine",
	}, "2026-06-30T12:00:00.000Z")
	require.NoError(t, err)
	assert.Equal(t, "test-machine", status.Machine)
	assert.Equal(t, "2026-06-30T12:00:00.000Z", status.LastPushAt)
	assert.Equal(t, 2, status.DuckDBSessions)
	assert.Equal(t, 3, status.DuckDBMessages)
}

type syncFixture struct {
	alphaID string
	betaID  string
}

func newLocalDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.OpenTestDB(t)
}

func newTestSync(
	t *testing.T, path string, local *db.DB, opts SyncOptions,
) *Sync {
	t.Helper()
	syncer, err := New(path, local, "test-machine", opts)
	require.NoError(t, err)
	require.NoError(t, local.SetSyncState(
		syncer.transcriptRevisionBackfillKey(), "1",
	))
	t.Cleanup(func() {
		require.NoError(t, syncer.Close())
	})
	return syncer
}

func newInMemoryTestSync(t *testing.T, local *db.DB, opts SyncOptions) *Sync {
	t.Helper()
	return newTestSync(t, ":memory:", local, opts)
}

// TestDuckGetAnalyticsSkillsIgnoresCrossSessionDuplicateIDs guards the
// skill join: DuckDB mirrors SQLite row IDs from many machines, so
// messages.id is not globally unique. A tool call must join only to a
// message in its own session, not another session's row with the same id.
func TestDuckGetAnalyticsSkillsIgnoresCrossSessionDuplicateIDs(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	duck := syncer.DB()
	store := NewStoreFromDB(duck)

	// Both sessions mirror a message with id 100; only sess-a has the
	// skill call. The join must not also match sess-b's message.
	insertDuckSkillCollision(t, duck, "sess-a", 100,
		"2026-02-03 10:00:00", "deploy")
	insertDuckSkillCollision(t, duck, "sess-b", 100,
		"2026-02-25 10:00:00", "")

	resp, err := store.GetAnalyticsSkills(ctx, db.AnalyticsFilter{
		From: "2026-02-01", To: "2026-02-28", Timezone: "UTC",
	}, "week")
	require.NoError(t, err, "GetAnalyticsSkills")
	require.Len(t, resp.BySkill, 1, "BySkill")
	assert.Equal(t, "deploy", resp.BySkill[0].SkillName)
	assert.Equal(t, 1, resp.BySkill[0].CallCount,
		"cross-session id collision must not double-count")
	assert.Equal(t, "2026-02-03T10:00:00Z", resp.BySkill[0].LastUsedAt,
		"timestamp comes from the call's own session")

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if c := e.BySkill["deploy"]; c > 0 {
			trend[e.Date] += c
		}
	}
	assert.Equal(t, map[string]int{"2026-02-02": 1}, trend,
		"no bucket from the colliding session's message")
}

// insertDuckSkillCollision raw-inserts a session and a message with an
// explicit (non-unique) id. A skill tool call is added only when skill
// is non-empty.
func insertDuckSkillCollision(
	t *testing.T, duck *sql.DB, sessionID string, msgID int, ts, skill string,
) {
	t.Helper()
	ctx := context.Background()
	_, err := duck.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, message_count,
			user_message_count, relationship_type, started_at, created_at
		) VALUES (?, 'alpha', 'local', 'claude', 1, 1, 'root',
			CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP))`,
		sessionID, ts, ts)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content, timestamp)
		VALUES (?, ?, 0, 'assistant', 'msg', CAST(? AS TIMESTAMP))`,
		msgID, sessionID, ts)
	require.NoError(t, err)
	if skill == "" {
		return
	}
	_, err = duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category,
			call_index, tool_use_id, skill_name
		) VALUES (?, ?, ?, 'Skill', 'Skill', 0, ?, ?)`,
		msgID, msgID, sessionID, sessionID+"-tu", skill)
	require.NoError(t, err)
}

func insertOtherMachineDuckSession(t *testing.T, duck *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := duck.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent,
			message_count, user_message_count, relationship_type, created_at
		) VALUES (
			'other-session', 'alpha', 'other-machine', 'claude',
			1, 1, '', current_timestamp
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp
		) VALUES (
			2, 'other-session', 0, 'assistant', 'from other machine',
			current_timestamp
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category,
			call_index, tool_use_id, input_json
		) VALUES (
			9001, 2, 'other-session', 'wrong-session-tool', 'other',
			0, 'other-tool-use', '{"cmd":"wrong-session-tool"}'
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO starred_sessions (session_id, created_at)
		VALUES ('other-session', current_timestamp)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			id, session_id, message_id, ordinal, note, created_at
		) VALUES (
			9001, 'other-session', 2, 0, 'other pin', current_timestamp
		)`)
	require.NoError(t, err)
}

func seedDuckDBSyncFixture(t *testing.T, local *db.DB) syncFixture {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         3,
		OutputPerMTok:        15,
		CacheCreationPerMTok: 1,
		CacheReadPerMTok:     0.5,
	}}))
	alphaID := "duck-sync-alpha"
	betaID := "duck-sync-beta"
	alphaSecret := "secret token sk-duckdb"
	callIndex := 0
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession(alphaID, "alpha", "alpha first", "2026-01-10T00:00:00.000Z", 2),
			Messages: []db.Message{
				syncMessage(alphaID, 0, "user", "alpha first", "2026-01-10T00:00:00.000Z"),
				syncMessage(alphaID, 1, "assistant", alphaSecret, "2026-01-10T00:01:00.000Z",
					db.ToolCall{
						ToolName:  "search",
						Category:  "search",
						SkillName: "duck-search",
						ToolUseID: "tool-alpha",
						InputJSON: `{"query":"duck"}`,
						ResultEvents: []db.ToolResultEvent{{
							Source:        "tool",
							Status:        "complete",
							Content:       "duck result",
							Timestamp:     "2026-01-10T00:01:30.000Z",
							EventIndex:    0,
							ContentLength: len("duck result"),
						}},
					}),
			},
			UsageEvents: []db.UsageEvent{{
				Source:       "hermes",
				Model:        "claude-test",
				InputTokens:  10,
				OutputTokens: 5,
				OccurredAt:   "2026-01-10T00:02:00.000Z",
				DedupKey:     "alpha-usage",
			}},
			Findings: []db.SecretFinding{{
				SessionID:      alphaID,
				RuleName:       "test_secret",
				Confidence:     "definite",
				LocationKind:   "message",
				MessageOrdinal: 1,
				CallIndex:      &callIndex,
				MatchStart:     len("secret token "),
				MatchEnd:       len(alphaSecret),
				MatchIndex:     0,
				RedactedMatch:  "sk-duckdb...",
				RulesVersion:   "test-rules",
			}},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(betaID, "beta", "beta first", "2026-01-11T00:00:00.000Z", 1),
			Messages: []db.Message{
				syncMessage(betaID, 0, "user", "beta first", "2026-01-11T00:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	ok, err := local.StarSession(alphaID)
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(ctx, alphaID)
	require.NoError(t, err)
	note := "pin alpha"
	_, err = local.PinMessage(alphaID, msgs[0].ID, &note)
	require.NoError(t, err)
	return syncFixture{alphaID: alphaID, betaID: betaID}
}

func syncSession(id, project, first, ts string, messageCount int) db.Session {
	firstValue := first
	startedAt := ts
	endedAt := ts
	localModifiedAt := ts
	transcriptRevision := "1"
	return db.Session{
		ID:                 id,
		Project:            project,
		Machine:            "local",
		Agent:              "claude",
		FirstMessage:       &firstValue,
		StartedAt:          &startedAt,
		EndedAt:            &endedAt,
		CreatedAt:          ts,
		LocalModifiedAt:    &localModifiedAt,
		TranscriptRevision: &transcriptRevision,
		MessageCount:       messageCount,
		UserMessageCount:   1,
		RelationshipType:   "root",
		Outcome:            "success",
		OutcomeConfidence:  "high",
		EndedWithRole:      "assistant",
		DataVersion:        1,
	}
}

func syncMessage(
	sessionID string, ordinal int, role, content, ts string,
	calls ...db.ToolCall,
) db.Message {
	msg := db.Message{
		SessionID:        sessionID,
		Ordinal:          ordinal,
		Role:             role,
		Content:          content,
		Timestamp:        ts,
		ContentLength:    len(content),
		HasToolUse:       len(calls) > 0,
		ToolCalls:        calls,
		Model:            "claude-test",
		TokenUsage:       []byte(`{"input_tokens":1,"output_tokens":2}`),
		ContextTokens:    1,
		OutputTokens:     2,
		HasContextTokens: true,
		HasOutputTokens:  true,
	}
	return msg
}

func updateSession(t *testing.T, local *db.DB, sessionID, content, modifiedAt string) {
	t.Helper()
	sess := syncSession(sessionID, "alpha", content, "2026-01-10T00:00:00.000Z", 1)
	localModifiedAt := modifiedAt
	sess.LocalModifiedAt = &localModifiedAt
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", content, modifiedAt)},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

func assertDuckDBCount(t *testing.T, conn *sql.DB, table string, want int) {
	t.Helper()
	var got int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&got))
	assert.Equal(t, want, got, table)
}

func assertDuckDBCountWhere(
	t *testing.T, conn *sql.DB, table, where string, arg any, want int,
) {
	t.Helper()
	var got int
	require.NoError(t, conn.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE `+where, arg,
	).Scan(&got))
	assert.Equal(t, want, got, table)
}

func TestSyncResultDurationIsSet(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Greater(t, result.Duration, time.Duration(0))
}
