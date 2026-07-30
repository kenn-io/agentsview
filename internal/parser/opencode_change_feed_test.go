package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeCoverageIterationErrorPreservesTimeout(t *testing.T) {
	assert.ErrorIs(t,
		openCodeCoverageIterationError(nil, context.DeadlineExceeded),
		context.DeadlineExceeded,
	)
	rowsErr := errors.New("rows failed")
	assert.ErrorIs(t,
		openCodeCoverageIterationError(rowsErr, context.DeadlineExceeded),
		rowsErr,
	)
}

func TestOpenCodeChangeFeedUsesFixedHighWaterAndRowBound(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	insertOpenCodeEvents(t, writer, 1, 3)

	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	assert.Empty(t, baseline.SessionIDs)
	assert.Equal(t, int64(3), baseline.Next.LastRowID)

	insertOpenCodeEvents(t, writer, 4, OpenCodeCoverageMaxRows+47)
	first, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	require.NoError(t, first.Validate())
	assert.Equal(t, OpenCodeCoverageMaxRows, first.Rows)
	assert.True(t, first.More)
	fixedHighWater := first.Next.HighWaterRowID

	insertOpenCodeEvents(t, writer, OpenCodeCoverageMaxRows+48, OpenCodeCoverageMaxRows+48)
	second, err := ReadOpenCodeCoverage(t.Context(), dbPath, first.Next)
	require.NoError(t, err)
	assert.False(t, second.More)
	assert.Zero(t, second.Next.HighWaterRowID)
	assert.Equal(t, fixedHighWater, second.Next.LastRowID)

	third, err := ReadOpenCodeCoverage(t.Context(), dbPath, second.Next)
	require.NoError(t, err)
	assert.Equal(t, 1, third.Rows)
	assert.Equal(t, int64(OpenCodeCoverageMaxRows+48), third.Next.LastRowID)
}

func TestOpenCodeChangeFeedContinuationValidatesHighWaterAnchor(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	insertOpenCodeEvents(t, writer, 1, OpenCodeCoverageMaxRows+1)
	first, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	require.True(t, first.More)
	_, err = writer.Exec(
		"UPDATE event SET id = ? WHERE rowid = ?",
		"evt_replaced_tail", first.Next.HighWaterRowID,
	)
	require.NoError(t, err)

	continuation, err := ReadOpenCodeCoverage(t.Context(), dbPath, first.Next)
	require.NoError(t, err)
	assert.True(t, continuation.AuditRequired)
	assert.True(t, continuation.Next.AuditLatched)
}

func TestOpenCodeChangeFeedRejectsOversizedPayloadBeforeAdvancing(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_big', 1, 'session.updated.1', ?)",
		eventID(1),
		strings.Repeat("x", OpenCodeCoverageMaxPayloadBytes+1),
	)
	require.NoError(t, err)

	batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.True(t, batch.AuditRequired)
	assert.True(t, batch.Next.AuditLatched)
	assert.Zero(t, batch.Rows)
	assert.Zero(t, batch.Next.LastRowID)
}

func TestOpenCodeChangeFeedLatchesUnknownDurableEvent(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_unknown', 1, 'future.event.1', '{}')",
		eventID(1),
	)
	require.NoError(t, err)

	batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.True(t, batch.AuditRequired)
	assert.True(t, batch.Next.AuditLatched)

	repeated, err := ReadOpenCodeCoverage(t.Context(), dbPath, batch.Next)
	require.NoError(t, err)
	assert.False(t, repeated.AuditRequired)
	assert.Zero(t, repeated.Rows)
}

func TestOpenCodeChangeFeedAuditsMalformedAcceptedPayloads(t *testing.T) {
	for _, eventType := range []string{
		"session.created.1",
		"session.updated.1",
		"session.deleted.1",
		"message.updated.1",
		"message.removed.1",
		"message.part.updated.1",
		"message.part.removed.1",
	} {
		t.Run(eventType, func(t *testing.T) {
			dbPath, writer := newOpenCodeEventDB(t)
			baseline, err := ReadOpenCodeCoverage(
				t.Context(), dbPath, OpenCodeCoverageState{},
			)
			require.NoError(t, err)
			_, err = writer.Exec(
				"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_bad', 'ses_bad', 1, ?, '{}')",
				eventType,
			)
			require.NoError(t, err)

			batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
			require.NoError(t, err)
			assert.True(t, batch.AuditRequired)
			assert.True(t, batch.Next.AuditLatched)
		})
	}
}

func TestOpenCodeChangeFeedDefersSettlementUntilFixedHighWaterDrains(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	insertOpenCodeEvents(t, writer, 1, OpenCodeCoverageMaxRows)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_stream', ?, 'message.part.updated.1', ?)",
		eventID(OpenCodeCoverageMaxRows+1), OpenCodeCoverageMaxRows+1,
		partUpdatedPayload("ses_stream"),
	)
	require.NoError(t, err)

	first, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.True(t, first.More)
	assert.Empty(t, first.SessionIDs)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_stream', ?, 'session.updated.1', ?)",
		eventID(OpenCodeCoverageMaxRows+2), OpenCodeCoverageMaxRows+2,
		sessionUpdatedPayload("ses_stream"),
	)
	require.NoError(t, err)

	second, err := ReadOpenCodeCoverage(t.Context(), dbPath, first.Next)
	require.NoError(t, err)
	assert.Equal(t, []string{"ses_stream"}, second.SessionIDs)

	third, err := ReadOpenCodeCoverage(t.Context(), dbPath, second.Next)
	require.NoError(t, err)
	assert.Equal(t, []string{"ses_stream"}, third.SessionIDs)
}

func TestOpenCodeChangeFeedEmitsInterruptedStreamAtHighWater(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	_, err = writer.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_pending', 'ses_pending', 1, 'message.part.updated.1', ?)",
		partUpdatedPayload("ses_pending"),
	)
	require.NoError(t, err)

	batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.Equal(t, []string{"ses_pending"}, batch.SessionIDs)
	assert.Empty(t, batch.Next.PendingIDs)
	assert.Equal(t, int64(1), batch.Next.LastRowID)
}

func TestOpenCodeChangeFeedTracksDeletionExistenceAcrossLaterEvents(t *testing.T) {
	t.Run("message event preserves deletion", func(t *testing.T) {
		dbPath, writer := newOpenCodeEventDB(t)
		baseline, err := ReadOpenCodeCoverage(
			t.Context(), dbPath, OpenCodeCoverageState{},
		)
		require.NoError(t, err)
		_, err = writer.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_delete', 'ses_mixed', 1, 'session.deleted.1', ?)",
			`{"sessionID":"ses_mixed","info":{"id":"ses_mixed"}}`,
		)
		require.NoError(t, err)
		_, err = writer.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_part', 'ses_mixed', 2, 'message.part.updated.1', ?)",
			partUpdatedPayload("ses_mixed"),
		)
		require.NoError(t, err)

		batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
		require.NoError(t, err)
		assert.Equal(t, []string{"ses_mixed"}, batch.RemovedIDs)
		assert.Empty(t, batch.SessionIDs)
	})

	t.Run("session event revives deletion", func(t *testing.T) {
		dbPath, writer := newOpenCodeEventDB(t)
		baseline, err := ReadOpenCodeCoverage(
			t.Context(), dbPath, OpenCodeCoverageState{},
		)
		require.NoError(t, err)
		_, err = writer.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_delete', 'ses_mixed', 1, 'session.deleted.1', ?)",
			`{"sessionID":"ses_mixed","info":{"id":"ses_mixed"}}`,
		)
		require.NoError(t, err)
		_, err = writer.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES('evt_update', 'ses_mixed', 2, 'session.updated.1', ?)",
			sessionUpdatedPayload("ses_mixed"),
		)
		require.NoError(t, err)

		batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
		require.NoError(t, err)
		assert.Empty(t, batch.RemovedIDs)
		assert.Equal(t, []string{"ses_mixed"}, batch.SessionIDs)
	})
}

func TestOpenCodeChangeFeedArchiveCardinalityIndependence(t *testing.T) {
	for _, count := range []int{10, 100000} {
		t.Run(fmt.Sprintf("events=%d", count), func(t *testing.T) {
			dbPath, writer := newOpenCodeEventDB(t)
			insertOpenCodeEventsTx(t, writer, 1, count)
			baseline, err := ReadOpenCodeCoverage(
				t.Context(), dbPath, OpenCodeCoverageState{},
			)
			require.NoError(t, err)
			assert.Zero(t, baseline.Rows)
			_, err = writer.Exec(
				"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_changed', ?, 'session.updated.1', ?)",
				eventID(count+1), count+1, sessionUpdatedPayload("ses_changed"),
			)
			require.NoError(t, err)

			changed, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
			require.NoError(t, err)
			assert.Equal(t, 1, changed.Rows)
			assert.Equal(t, []string{"ses_changed"}, changed.SessionIDs)
		})
	}
}

func TestOpenCodeChangeFeedUsesInsertionOrderForNonMonotonicTextIDs(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	for _, event := range []struct {
		id, session string
	}{
		{id: "evt_zzzz", session: "ses_first"},
		{id: "evt_aaaa", session: "ses_second"},
	} {
		_, err = writer.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, ?, 1, 'session.updated.1', ?)",
			event.id, event.session, sessionUpdatedPayload(event.session),
		)
		require.NoError(t, err)
	}

	batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.Equal(t, []string{"ses_first", "ses_second"}, batch.SessionIDs)
	assert.Equal(t, int64(2), batch.Next.LastRowID)
}

func TestOpenCodeChangeFeedAuditsReplacedContainerWithPreservedEndpoint(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	insertOpenCodeEvents(t, writer, 1, 1)
	baseline, err := ReadOpenCodeCoverage(t.Context(), dbPath, OpenCodeCoverageState{})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	replaceOpenCodeEventDBPreservingEndpoint(t, dbPath, "ses_replaced")

	batch, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	assert.True(t, batch.AuditRequired)
	assert.True(t, batch.Next.AuditLatched)
	assert.Equal(t, baseline.Next.LastRowID, batch.Next.HighWaterRowID)
	assert.Equal(t, baseline.Next.LastEventID, batch.Next.HighWaterEventID)
}

func TestOpenCodeChangeFeedDoesNotAuditRoutineWALCheckpoint(t *testing.T) {
	dbPath, writer := newOpenCodeEventDB(t)
	_, err := writer.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	baseline, err := ReadOpenCodeCoverage(
		t.Context(), dbPath, OpenCodeCoverageState{},
	)
	require.NoError(t, err)
	insertOpenCodeEvents(t, writer, 1, 1)

	changed, err := ReadOpenCodeCoverage(t.Context(), dbPath, baseline.Next)
	require.NoError(t, err)
	require.False(t, changed.AuditRequired)
	require.Equal(t, []string{"ses_stream"}, changed.SessionIDs)
	_, err = writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)

	afterCheckpoint, err := ReadOpenCodeCoverage(
		t.Context(), dbPath, changed.Next,
	)
	require.NoError(t, err)
	assert.False(t, afterCheckpoint.AuditRequired)
	assert.Zero(t, afterCheckpoint.Rows)
}

func replaceOpenCodeEventDBPreservingEndpoint(
	t *testing.T, dbPath, sessionID string,
) {
	t.Helper()
	replacement := dbPath + ".replacement"
	replacementDB, err := sql.Open("sqlite3", replacement)
	require.NoError(t, err)
	_, err = replacementDB.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	_, err = replacementDB.Exec(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, ?, 1, 'session.updated.1', ?)",
		eventID(1), sessionID, sessionUpdatedPayload(sessionID),
	)
	require.NoError(t, err)
	_, err = replacementDB.Exec(
		"CREATE TABLE replacement_padding(value BLOB); INSERT INTO replacement_padding VALUES(zeroblob(8192))",
	)
	require.NoError(t, err)
	require.NoError(t, replacementDB.Close())
	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Rename(replacement, dbPath))
}

func newOpenCodeEventDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`CREATE TABLE event (
		id TEXT PRIMARY KEY,
		aggregate_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		data TEXT NOT NULL
	)`)
	require.NoError(t, err)
	return dbPath, db
}

func insertOpenCodeEvents(t *testing.T, db *sql.DB, first, last int) {
	t.Helper()
	for id := first; id <= last; id++ {
		_, err := db.Exec(
			"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, ?, ?, 'session.updated.1', ?)",
			eventID(id), "ses_stream", id, sessionUpdatedPayload("ses_stream"),
		)
		require.NoError(t, err)
	}
}

func insertOpenCodeEventsTx(t *testing.T, db *sql.DB, first, last int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	stmt, err := tx.Prepare(
		"INSERT INTO event(id, aggregate_id, seq, type, data) VALUES(?, 'ses_history', ?, 'session.updated.1', ?)",
	)
	require.NoError(t, err)
	for id := first; id <= last; id++ {
		_, err = stmt.Exec(eventID(id), id, sessionUpdatedPayload("ses_history"))
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())
}

func eventID(id int) string {
	return fmt.Sprintf("evt_%08d", id)
}

func sessionUpdatedPayload(sessionID string) string {
	return fmt.Sprintf(
		`{"sessionID":%q,"info":{"id":%q,"projectID":"global"}}`,
		sessionID, sessionID,
	)
}

func partUpdatedPayload(sessionID string) string {
	return fmt.Sprintf(
		`{"sessionID":%q,"part":{"id":"prt_1","messageID":"msg_1","sessionID":%q},"time":1}`,
		sessionID, sessionID,
	)
}
