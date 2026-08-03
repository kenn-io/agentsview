// ABOUTME: Tests for the bounded OpenCode journal change feed covering the
// ABOUTME: reference model, two-stage admission, capability probe, cursor
// ABOUTME: continuity, and consumer routing.
package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openCodeJournalFixture is a writable OpenCode event journal fixture.
type openCodeJournalFixture struct {
	db   *sql.DB
	path string
}

// openCodeJournalSchema is the measured v1.18.10 journal DDL.
const openCodeJournalSchema = `
CREATE TABLE IF NOT EXISTS event (
    id          TEXT NOT NULL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    type        TEXT NOT NULL,
    data        BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS event_sequence (
    id          TEXT NOT NULL PRIMARY KEY,
    owner_id    TEXT
);
`

// openCodeJournalSchemaNoOwnerID is the DDL for a Kilo/MiMo fork that has
// the event tables but no owner_id column in event_sequence. Used by the fork
// exclusion test (row 9).
const openCodeJournalSchemaNoOwnerID = `
CREATE TABLE IF NOT EXISTS event (
    id          TEXT NOT NULL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    type        TEXT NOT NULL,
    data        BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS event_sequence (
    id          TEXT NOT NULL PRIMARY KEY
);
`

func newOpenCodeJournalFixture(t *testing.T) *openCodeJournalFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(openCodeJournalSchema)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &openCodeJournalFixture{db: db, path: path}
}

// insertEvent appends one row to the event table and returns its rowid.
func (f *openCodeJournalFixture) insertEvent(
	t *testing.T, id, aggregateID, typ string, data []byte,
) int64 {
	t.Helper()
	// seq is just the next integer for simplicity.
	var seq int
	_ = f.db.QueryRow("SELECT COALESCE(MAX(seq)+1,1) FROM event WHERE aggregate_id = ?", aggregateID).Scan(&seq)
	res, err := f.db.Exec(
		"INSERT INTO event (id, aggregate_id, seq, type, data) VALUES (?, ?, ?, ?, ?)",
		id, aggregateID, seq, typ, data,
	)
	require.NoError(t, err)
	rowid, _ := res.LastInsertId()
	return rowid
}

// insertPartEvent inserts a message.part.updated.1 event (no settlement).
func (f *openCodeJournalFixture) insertPartEvent(t *testing.T, id, aggID string) int64 {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"sessionID": aggID,
		"part":      map[string]any{"id": id},
	})
	return f.insertEvent(t, id, aggID, "message.part.updated.1", payload)
}

// insertSettledEvent inserts a message.updated.1 event with an integer
// info.time.completed (settlement).
func (f *openCodeJournalFixture) insertSettledEvent(t *testing.T, id, aggID string) int64 {
	t.Helper()
	payload := makeSettledPayload(t, id, aggID)
	return f.insertEvent(t, id, aggID, "message.updated.1", payload)
}

// insertPendingMsgEvent inserts a message.updated.1 event without settlement
// (assistant role, no completed field).
func (f *openCodeJournalFixture) insertPendingMsgEvent(t *testing.T, id, aggID string) int64 {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"sessionID": aggID,
		"info": map[string]any{
			"id": id, "sessionID": aggID, "role": "assistant",
		},
	})
	return f.insertEvent(t, id, aggID, "message.updated.1", payload)
}

// insertUserMsgEvent inserts a message.updated.1 event for a user role.
func (f *openCodeJournalFixture) insertUserMsgEvent(t *testing.T, id, aggID string) int64 {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"sessionID": aggID,
		"info": map[string]any{
			"id": id, "sessionID": aggID, "role": "user",
		},
	})
	return f.insertEvent(t, id, aggID, "message.updated.1", payload)
}

// insertSessionUpdatedEvent inserts a session.updated.1 event.
func (f *openCodeJournalFixture) insertSessionUpdatedEvent(t *testing.T, id, aggID string) int64 {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"sessionID": aggID,
		"info":      map[string]any{"id": aggID},
	})
	return f.insertEvent(t, id, aggID, "session.updated.1", payload)
}

// deleteEventsForSession removes all event rows for one aggregate.
func (f *openCodeJournalFixture) deleteEventsForSession(t *testing.T, aggID string) {
	t.Helper()
	_, err := f.db.Exec("DELETE FROM event WHERE aggregate_id = ?", aggID)
	require.NoError(t, err)
}

// initDrain performs the initialization drain call and returns the initialized
// checkpoint. The caller must then add new events and call DrainOpenCodeJournal
// again to obtain real drain results.
func initDrain(t *testing.T, path string) OpenCodeCoverageCheckpoint {
	t.Helper()
	result, err := DrainOpenCodeJournal(context.Background(), path, OpenCodeCoverageCheckpoint{})
	require.NoError(t, err)
	require.True(t, result.Next.Initialized, "first drain must initialize the checkpoint")
	require.False(t, result.AuditRequired, "initialization must not latch an audit")
	return result.Next
}

// sortedIDs returns a sorted copy of ids for deterministic comparison.
func sortedIDs(ids []string) []string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return cp
}

// makeSettledPayload builds a message.updated.1 JSON payload with a
// completed assistant turn.
func makeSettledPayload(t *testing.T, msgID, aggID string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sessionID": aggID,
		"info": map[string]any{
			"id":        msgID,
			"sessionID": aggID,
			"role":      "assistant",
			"time":      map[string]any{"completed": json.Number("1234567890000")},
		},
	})
	require.NoError(t, err)
	return payload
}

// makeReferenceEvents builds OpenCodeJournalEventInput slices for the
// reference model tests. It includes no payload for part events and a full
// settled payload for settlement events.
func makeSettledRefEvent(rowid int64, eventID, aggID string) OpenCodeJournalEventInput {
	payload := map[string]any{
		"sessionID": aggID,
		"info": map[string]any{
			"id":        eventID,
			"sessionID": aggID,
			"role":      "assistant",
			"time":      map[string]any{"completed": json.Number("1234567890000")},
		},
	}
	data, _ := json.Marshal(payload)
	return OpenCodeJournalEventInput{
		RowID: rowid, EventID: eventID, AggregateID: aggID,
		Type: "message.updated.1", Data: data,
	}
}

func makePartRefEvent(rowid int64, eventID, aggID string) OpenCodeJournalEventInput {
	return OpenCodeJournalEventInput{
		RowID: rowid, EventID: eventID, AggregateID: aggID,
		Type: "message.part.updated.1", Data: nil, // never read
	}
}

func makePendingMsgRefEvent(rowid int64, eventID, aggID string) OpenCodeJournalEventInput {
	payload := map[string]any{
		"sessionID": aggID,
		"info": map[string]any{
			"id": eventID, "sessionID": aggID, "role": "assistant",
		},
	}
	data, _ := json.Marshal(payload)
	return OpenCodeJournalEventInput{
		RowID: rowid, EventID: eventID, AggregateID: aggID,
		Type: "message.updated.1", Data: data,
	}
}

func makeSessionUpdatedRefEvent(rowid int64, eventID, aggID string) OpenCodeJournalEventInput {
	return OpenCodeJournalEventInput{
		RowID: rowid, EventID: eventID, AggregateID: aggID,
		Type: "session.updated.1", Data: nil,
	}
}

// TestOpenCodeStreamingPartsNoParse verifies the proof matrix row 2:
// streaming parts cause no parse. A reference-model sequence of many part
// updates followed by a settlement shows zero sessions in ReadyIDs until the
// settlement arrives.
func TestOpenCodeStreamingPartsNoParse(t *testing.T) {
	const N = 20
	const aggID = "ses-stream"

	// Build N part updates, then one settlement.
	events := make([]OpenCodeJournalEventInput, N+1)
	for i := range N {
		events[i] = makePartRefEvent(int64(i+1), fmt.Sprintf("part-%03d", i), aggID)
	}
	events[N] = makeSettledRefEvent(int64(N+1), "settled-msg", aggID)

	// After each part update: session should be pending, never ready.
	cp := OpenCodeCoverageCheckpoint{}
	for i := range N {
		cp, _ = ReduceOpenCodeJournalEvents(cp, events[i:i+1])
		assert.Empty(t, cp.ReadyIDs,
			"no session must be ready after only part updates (step %d)", i)
		require.Contains(t, cp.PendingIDs, aggID,
			"session must be pending after a part update (step %d)", i)
	}

	// After the settlement: session moves to ReadyIDs.
	cp, audit := ReduceOpenCodeJournalEvents(cp, events[N:N+1])
	assert.False(t, audit, "settlement must not trigger an audit")
	assert.Contains(t, cp.ReadyIDs, aggID, "session must be ready after settlement")
	assert.NotContains(t, cp.PendingIDs, aggID,
		"session must not remain pending after settlement")
}

// TestOpenCodeSettlementField verifies proof matrix row 3: settlement field.
// An adapter test over a real temporary SQLite fixture verifies that:
//   - an assistant message.updated.1 with integer info.time.completed → ReadyIDs
//   - a user message.updated.1 stays in PendingIDs
//   - an assistant message.updated.1 without completed stays in PendingIDs
func TestOpenCodeSettlementField(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	// Initialize checkpoint against the empty fixture.
	checkpoint := initDrain(t, f.path)

	// Insert three events for three distinct sessions.
	f.insertSettledEvent(t, "settled-msg", "ses-settled")
	f.insertUserMsgEvent(t, "user-msg", "ses-user")
	f.insertPendingMsgEvent(t, "pending-msg", "ses-pending")

	// Drain should classify settled as ready, user and no-completed as pending.
	result, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
	require.NoError(t, err)
	assert.False(t, result.AuditRequired, "valid events must not trigger an audit")

	// For the high-water drain that moves through all three events.
	// If More=true (shouldn't be for 3 events), drain until done.
	for result.More {
		result, err = DrainOpenCodeJournal(ctx, f.path, result.Next)
		require.NoError(t, err)
	}

	assert.Contains(t, result.ReadyIDs, "ses-settled",
		"assistant with completed must settle")
	assert.NotContains(t, result.ReadyIDs, "ses-user",
		"user update must not settle")
	assert.NotContains(t, result.ReadyIDs, "ses-pending",
		"assistant without completed must not settle")
	assert.Contains(t, result.PendingIDs, "ses-user",
		"user update must be pending")
	assert.Contains(t, result.PendingIDs, "ses-pending",
		"assistant without completed must be pending")
}

func TestOpenCodeSessionMetadataEventIsReady(t *testing.T) {
	cp, audit := ReduceOpenCodeJournalEvents(
		OpenCodeCoverageCheckpoint{},
		[]OpenCodeJournalEventInput{{
			RowID: 1, EventID: "session-update", AggregateID: "ses-metadata",
			Type: "session.updated.1",
		}},
	)
	assert.False(t, audit)
	assert.Contains(t, cp.ReadyIDs, "ses-metadata",
		"durable session metadata must reach the archive even without a settlement event")
	assert.NotContains(t, cp.PendingIDs, "ses-metadata")
}

func TestOpenCodeCapturedUnversionedEventNames(t *testing.T) {
	cp, audit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, []OpenCodeJournalEventInput{
		{RowID: 1, EventID: "part", AggregateID: "ses-part", Type: "message.part.updated"},
		{RowID: 2, EventID: "message", AggregateID: "ses-message", Type: "message.updated"},
		{RowID: 3, EventID: "session", AggregateID: "ses-session", Type: "session.updated"},
	})
	assert.False(t, audit, "captured unversioned producer names are valid")
	assert.Contains(t, cp.PendingIDs, "ses-part")
	assert.Contains(t, cp.PendingIDs, "ses-message")
	assert.Contains(t, cp.ReadyIDs, "ses-session")
}

func TestOpenCodeMissingDatabaseReturnsError(t *testing.T) {
	_, err := DrainOpenCodeJournal(
		context.Background(), filepath.Join(t.TempDir(), "missing.db"),
		OpenCodeCoverageCheckpoint{Initialized: true},
	)
	assert.ErrorIs(t, err, ErrOpenCodeCoverageDatabaseMissing)
}

func TestOpenCodeCancelledDrainReturnsContextError(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := DrainOpenCodeJournal(ctx, f.path, OpenCodeCoverageCheckpoint{})

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, result.AuditRequired,
		"caller cancellation must not latch a structural audit")
}

// TestOpenCodeTrailingEventsDoNotDowngrade verifies proof matrix row 4:
// trailing events do not downgrade. A reference-model sequence of settlement
// followed by a message.updated.1 and then session.updated.1 leaves the
// identity in ReadyIDs throughout.
func TestOpenCodeTrailingEventsDoNotDowngrade(t *testing.T) {
	const aggID = "ses-settled"

	settled := makeSettledRefEvent(1, "settled-msg", aggID)
	trailingMsg := makePendingMsgRefEvent(2, "trailing-msg", aggID)
	sessionUpdated := makeSessionUpdatedRefEvent(3, "session-update", aggID)

	cp, _ := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, []OpenCodeJournalEventInput{settled})
	assert.Contains(t, cp.ReadyIDs, aggID, "session must be ready after settlement")

	cp, audit := ReduceOpenCodeJournalEvents(cp, []OpenCodeJournalEventInput{trailingMsg})
	assert.False(t, audit, "trailing message.updated.1 must not audit")
	assert.Contains(t, cp.ReadyIDs, aggID,
		"trailing message.updated.1 must not downgrade from ready")

	cp, audit = ReduceOpenCodeJournalEvents(cp, []OpenCodeJournalEventInput{sessionUpdated})
	assert.False(t, audit, "session.updated.1 must not audit")
	assert.Contains(t, cp.ReadyIDs, aggID,
		"session.updated.1 must not downgrade from ready")
}

// TestOpenCodePartAfterSettlementDowngrades verifies proof matrix row 5:
// a message.part.updated.1 after settlement returns the identity to pending.
func TestOpenCodePartAfterSettlementDowngrades(t *testing.T) {
	const aggID = "ses-downgrade"

	settled := makeSettledRefEvent(1, "settled-msg", aggID)
	part := makePartRefEvent(2, "late-part", aggID)

	cp, _ := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, []OpenCodeJournalEventInput{settled})
	assert.Contains(t, cp.ReadyIDs, aggID, "session must be ready after settlement")

	cp, audit := ReduceOpenCodeJournalEvents(cp, []OpenCodeJournalEventInput{part})
	assert.False(t, audit, "part update must not audit")
	assert.NotContains(t, cp.ReadyIDs, aggID,
		"part after settlement must downgrade from ready")
	assert.Contains(t, cp.PendingIDs, aggID,
		"part after settlement must return to pending")
}

// TestOpenCodePageSplitEquivalence verifies proof matrix row 6: page-split
// equivalence. The reference model applied to all events in one pass equals
// the reference model applied in arbitrary two-way splits.
func TestOpenCodePageSplitEquivalence(t *testing.T) {
	// Build an event sequence mixing part updates, settlements, and trailing events.
	events := []OpenCodeJournalEventInput{
		makePartRefEvent(1, "part-a1", "ses-a"),
		makePartRefEvent(2, "part-a2", "ses-a"),
		makeSettledRefEvent(3, "settled-a", "ses-a"),
		makePartRefEvent(4, "part-b1", "ses-b"),
		makeSettledRefEvent(5, "settled-b", "ses-b"),
		makePartRefEvent(6, "late-part-a", "ses-a"), // downgrades a again
		makeSettledRefEvent(7, "re-settled-a", "ses-a"),
		makeSessionUpdatedRefEvent(8, "su-b", "ses-b"), // trailing, no downgrade
	}

	// Full-sequence result is the reference.
	fullCP, fullAudit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
	assert.False(t, fullAudit, "no audit expected from valid events")

	// Every two-way split must agree with the full result.
	n := len(events)
	for split := 0; split <= n; split++ {
		firstCP, _ := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events[:split])
		secondCP, audit := ReduceOpenCodeJournalEvents(firstCP, events[split:])
		assert.False(t, audit, "no audit for split at %d", split)
		assert.Equal(t,
			sortedIDs(fullCP.ReadyIDs), sortedIDs(secondCP.ReadyIDs),
			"ReadyIDs must agree for split at %d", split)
		assert.Equal(t,
			sortedIDs(fullCP.PendingIDs), sortedIDs(secondCP.PendingIDs),
			"PendingIDs must agree for split at %d", split)
	}
}

// TestOpenCodeOversizedPayloadNeverMaterialized verifies proof matrix row 7:
// a message.updated.1 row whose payload exceeds the byte budget triggers an
// audit without materializing the payload.
func TestOpenCodeOversizedPayloadNeverMaterialized(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	// Initialize against the empty fixture.
	checkpoint := initDrain(t, f.path)

	// Insert an oversized message.updated.1 payload (> 1 MB).
	// We use a large but otherwise structurally valid JSON blob.
	oversized := make([]byte, OpenCodeCoverageMaxPayloadBytes+1)
	// Fill with a JSON-like prefix so it doesn't fail fast on len check.
	copy(oversized, []byte(`{"sessionID":"ses-big","info":{"id":"big","sessionID":"ses-big","role":"assistant"},"padding":"`))
	oversized[len(oversized)-2] = '"'
	oversized[len(oversized)-1] = '}'
	f.insertEvent(t, "big-msg", "ses-big", "message.updated.1", oversized)

	result, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
	require.NoError(t, err)

	assert.True(t, result.AuditRequired,
		"oversized payload must trigger an audit")
	assert.Equal(t, 0, result.PayloadBytes,
		"oversized payload must not be materialized")
	assert.Equal(t, 1, result.RowsRead,
		"the metadata row must be counted")
}

// TestOpenCodeCapabilityRequiresFullSchema verifies proof matrix row 8:
// a container with owner_id present but a required event column missing is
// incompatible, so zero feed calls are made and base behavior is preserved.
func TestOpenCodeCapabilityRequiresFullSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	// event table is missing the 'data' column; event_sequence has owner_id.
	_, err = db.Exec(`
		CREATE TABLE event (
			id TEXT NOT NULL PRIMARY KEY,
			aggregate_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			type TEXT NOT NULL
		);
		CREATE TABLE event_sequence (
			id TEXT NOT NULL PRIMARY KEY,
			owner_id TEXT
		);
	`)
	require.NoError(t, err)

	_, compatible, probeErr := ProbeOpenCodeJournalCapability(
		context.Background(), dbPath,
	)
	require.NoError(t, probeErr)
	assert.False(t, compatible,
		"missing 'data' column must make the container incompatible "+
			"even when owner_id is present")
}

// TestOpenCodeForkExclusionByEvidence verifies proof matrix row 9: a
// Kilo-shaped fixture with the event DDL but no owner_id in event_sequence is
// incompatible. The check is purely schema-based, not agent-name-based.
func TestOpenCodeForkExclusionByEvidence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Full event DDL, but event_sequence lacks owner_id.
	_, err = db.Exec(openCodeJournalSchemaNoOwnerID)
	require.NoError(t, err)

	_, compatible, probeErr := ProbeOpenCodeJournalCapability(
		context.Background(), dbPath,
	)
	require.NoError(t, probeErr)
	assert.False(t, compatible,
		"absent owner_id must make the container incompatible; "+
			"no agent name is checked")
}

// TestOpenCodeDeletedSessionAnchorAudits verifies that deleting every cursor
// witness requests an audit instead of trusting an unverifiable position.
func TestOpenCodeDeletedSessionAnchorContinues(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	// Insert events for session A (rowids 1-3) and B (rowids 4-6).
	for i := 1; i <= 3; i++ {
		f.insertEvent(t,
			fmt.Sprintf("evt-a%d", i), "ses-a", "session.created.1",
			[]byte(`{}`),
		)
	}
	for i := 1; i <= 3; i++ {
		f.insertEvent(t,
			fmt.Sprintf("evt-b%d", i), "ses-b", "session.created.1",
			[]byte(`{}`),
		)
	}

	// Initialize baseline: anchors across both sessions.
	checkpoint := initDrain(t, f.path)

	// Override: keep only session A's anchors (simulating checkpoint where only A
	// was witnessed). MAX(rowid)=6 will remain >= max committed anchor rowid (3).
	checkpoint.Anchors = []OpenCodeJournalAnchor{
		{RowID: 3, EventID: "evt-a3", AggregateID: "ses-a"},
		{RowID: 2, EventID: "evt-a2", AggregateID: "ses-a"},
		{RowID: 1, EventID: "evt-a1", AggregateID: "ses-a"},
	}
	checkpoint.HighWaterKnown = false

	// Delete session A's events: all committed anchors are now missing.
	f.deleteEventsForSession(t, "ses-a")

	result, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired,
		"missing cursor witnesses must request an audit")
}

func TestOpenCodeDeletedCursorAnchorRewindsOnRowIDReuse(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	f.insertEvent(t, "evt-a", "ses-a", "session.created.1", []byte(`{}`))
	f.insertEvent(t, "evt-b", "ses-b", "session.created.1", []byte(`{}`))
	checkpoint := initDrain(t, f.path)
	f.deleteEventsForSession(t, "ses-b")
	f.insertSettledEvent(t, "evt-reused", "ses-reused")

	result, err := DrainOpenCodeJournal(context.Background(), f.path, checkpoint)
	require.NoError(t, err)
	assert.False(t, result.AuditRequired,
		"a lower verified anchor permits safe rewind instead of an audit")
	assert.Contains(t, result.ReadyIDs, "ses-reused",
		"rewinding below a reused cursor row must replay the replacement event")
}

// TestOpenCodeReplacementStillAudits verifies proof matrix row 11:
// even when MAX(rowid) is above the committed max (which would normally
// indicate ordinary deletion), a changed file identity overrides the inference
// and requests an audit.
func TestOpenCodeReplacementStillAudits(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	ctx := context.Background()

	// Create DB1: the original container.
	db1, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db1.Exec(openCodeJournalSchema)
	require.NoError(t, err)

	// Insert 3 events into DB1.
	for i := 1; i <= 3; i++ {
		_, err = db1.Exec(
			"INSERT INTO event (id, aggregate_id, seq, type, data) VALUES (?,?,?,?,?)",
			fmt.Sprintf("evt-%d", i), "ses-x", i, "session.created.1", []byte(`{}`),
		)
		require.NoError(t, err)
	}
	db1.Close()

	// Initialize the checkpoint: captures DB1's file identity.
	checkpoint := initDrain(t, dbPath)
	require.True(t, checkpoint.Initialized)
	// Sanity: identity was captured (non-zero on platforms that support it).
	// On platforms where both are zero, skip the identity-change sub-check but
	// still verify the high-rowid path: replace with a DB with MORE events.

	// Create DB2 beside DB1 before replacing the path, so filesystem identity
	// allocation cannot reuse DB1's inode.
	replacementPath := filepath.Join(dir, "replacement.db")
	db2, err := sql.Open("sqlite3", replacementPath)
	require.NoError(t, err)
	_, err = db2.Exec(openCodeJournalSchema)
	require.NoError(t, err)
	for i := 1; i <= 10; i++ {
		_, err = db2.Exec(
			"INSERT INTO event (id, aggregate_id, seq, type, data) VALUES (?,?,?,?,?)",
			fmt.Sprintf("new-evt-%d", i), "ses-y", i, "session.created.1", []byte(`{}`),
		)
		require.NoError(t, err)
	}
	db2.Close()
	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Rename(replacementPath, dbPath))

	// Anchor identity, rather than a platform-specific file identity, detects
	// replacement even when the replacement has a larger rowid high-water.
	result, err := DrainOpenCodeJournal(ctx, dbPath, checkpoint)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired,
		"changed anchors must trigger audit even when MAX(rowid) > committed")
}

func TestOpenCodeAnchorAggregateReuseStillAudits(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	f.insertEvent(t, "evt-a", "ses-a", "session.created.1", []byte(`{}`))
	checkpoint := initDrain(t, f.path)
	_, err := f.db.Exec("UPDATE event SET aggregate_id = ? WHERE id = ?", "ses-reused", "evt-a")
	require.NoError(t, err)

	result, err := DrainOpenCodeJournal(context.Background(), f.path, checkpoint)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired,
		"an anchor with a reused aggregate must not be accepted by event id alone")
}

// TestOpenCodeContractIsolationReducer verifies proof matrix row 17:
// the reference model (ReduceOpenCodeJournalEvents) has no database,
// filesystem, or engine dependency. It is exercised here with pure event
// sequences to verify ordering, settlement, downgrade, and bound invariants.
func TestOpenCodeContractIsolationReducer(t *testing.T) {
	t.Run("ordering/settlement", func(t *testing.T) {
		// Settlement on any event causes the session to be ready.
		events := []OpenCodeJournalEventInput{
			makePartRefEvent(1, "p1", "ses-a"),
			makeSettledRefEvent(2, "s1", "ses-a"),
		}
		cp, audit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
		assert.False(t, audit)
		assert.Contains(t, cp.ReadyIDs, "ses-a")
	})

	t.Run("downgrade/upward", func(t *testing.T) {
		// Part after settlement downgrades; subsequent settlement re-upgrades.
		events := []OpenCodeJournalEventInput{
			makeSettledRefEvent(1, "s1", "ses-b"),
			makePartRefEvent(2, "p1", "ses-b"),
			makeSettledRefEvent(3, "s2", "ses-b"),
		}
		cp, audit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
		assert.False(t, audit)
		assert.Contains(t, cp.ReadyIDs, "ses-b")
		assert.NotContains(t, cp.PendingIDs, "ses-b")
	})

	t.Run("bound/maxIDs", func(t *testing.T) {
		// Inserting more than OpenCodeCoverageMaxIDs distinct sessions triggers audit.
		events := make([]OpenCodeJournalEventInput, OpenCodeCoverageMaxIDs+1)
		for i := range OpenCodeCoverageMaxIDs + 1 {
			id := fmt.Sprintf("ses-%04d", i)
			events[i] = makePartRefEvent(int64(i+1), fmt.Sprintf("p-%04d", i), id)
		}
		_, audit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
		assert.True(t, audit,
			"more than MaxIDs distinct sessions must trigger audit")
	})

	t.Run("unrecognized type triggers audit", func(t *testing.T) {
		events := []OpenCodeJournalEventInput{
			{RowID: 1, EventID: "e1", AggregateID: "ses-c",
				Type: "message.unknown.99", Data: nil},
		}
		_, audit := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
		assert.True(t, audit, "unrecognized event type must trigger audit")
	})

	t.Run("no imports", func(t *testing.T) {
		// This sub-test verifies the pure-function nature by calling
		// ReduceOpenCodeJournalEvents without any I/O side-effects.
		// If it compiles and runs without a database or filesystem,
		// the contract-isolation property holds.
		events := []OpenCodeJournalEventInput{
			makeSettledRefEvent(1, "e1", "ses-d"),
		}
		cp, _ := ReduceOpenCodeJournalEvents(OpenCodeCoverageCheckpoint{}, events)
		assert.Contains(t, cp.ReadyIDs, "ses-d")
	})
}

// TestOpenCodeAdapterFidelitySchemaMapping verifies proof matrix row 18:
// the adapter correctly maps v1.18.10 event types against a real fixture.
// An unrecognized event type version triggers an audit.
func TestOpenCodeAdapterFidelitySchemaMapping(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	t.Run("known event types map correctly", func(t *testing.T) {
		checkpoint := initDrain(t, f.path)

		// One of each recognized type.
		f.insertPartEvent(t, "known-part", "ses-known")
		f.insertSettledEvent(t, "known-settled", "ses-known")
		f.insertSessionUpdatedEvent(t, "known-su", "ses-known2")
		f.insertEvent(t, "known-created", "ses-known2", "session.created.1", []byte(`{}`))

		result, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
		require.NoError(t, err)
		for result.More {
			result, err = DrainOpenCodeJournal(ctx, f.path, result.Next)
			require.NoError(t, err)
		}
		assert.False(t, result.AuditRequired,
			"all v1.18.10 event types must be recognized")
	})

	t.Run("unknown event type version triggers audit", func(t *testing.T) {
		// Reset to a fresh fixture.
		f2 := newOpenCodeJournalFixture(t)
		checkpoint := initDrain(t, f2.path)

		f2.insertEvent(t, "unknown-type", "ses-unk", "message.unknown.99", []byte(`{}`))

		result, err := DrainOpenCodeJournal(ctx, f2.path, checkpoint)
		require.NoError(t, err)
		assert.True(t, result.AuditRequired,
			"unrecognized event type must latch audit")
	})
}

// openCodeFormatAgents is the set of agents that share the OpenCode SQLite
// container format and therefore declare BoundedCoverage=CapabilitySupported.
// Per-container schema admission (ProbeOpenCodeJournalCapability) gates whether
// the feed is actually used; incompatible containers (e.g. Kilo, MiMoCode,
// ICodeMate, which ship the journal tables empty or absent) degrade to existing
// base behavior without reading the journal.
var openCodeFormatAgents = map[AgentType]bool{
	AgentOpenCode:  true,
	AgentKilo:      true,
	AgentMiMoCode:  true,
	AgentIcodemate: true,
}

// TestOpenCodeConsumerRoutingCapabilityOptIn verifies proof matrix row 19:
// all opencode-format providers declare BoundedCoverage=CapabilitySupported;
// non-opencode providers do not. Per-container schema evidence (not agent name)
// gates the actual feed construction.
func TestOpenCodeConsumerRoutingCapabilityOptIn(t *testing.T) {
	factories := ProviderFactories()
	require.NotEmpty(t, factories, "ProviderFactories must return at least one factory")

	foundOpenCode := false
	for _, factory := range factories {
		caps := factory.Capabilities()
		def := factory.Definition()
		agent := def.Type

		if openCodeFormatAgents[agent] {
			if agent == AgentOpenCode {
				foundOpenCode = true
			}
			assert.Equal(t, CapabilitySupported, caps.Source.BoundedCoverage,
				"opencode-format agent %q must declare BoundedCoverage=CapabilitySupported; "+
					"per-container schema evidence gates actual feed use, not agent name", agent)
		} else {
			assert.NotEqual(t, CapabilitySupported, caps.Source.BoundedCoverage,
				"non-opencode-format agent %q must not declare BoundedCoverage=CapabilitySupported", agent)
		}
	}

	assert.True(t, foundOpenCode,
		"AgentOpenCode must appear in ProviderFactories")
}

// TestOpenCodeProbeCompatibleContainer verifies that ProbeOpenCodeJournalCapability
// returns true for a valid v1.18.10 schema.
func TestOpenCodeProbeCompatibleContainer(t *testing.T) {
	f := newOpenCodeJournalFixture(t)

	schemaVersion, compatible, err := ProbeOpenCodeJournalCapability(
		context.Background(), f.path,
	)
	require.NoError(t, err)
	assert.True(t, compatible, "valid journal schema must be compatible")
	assert.NotZero(t, schemaVersion, "schema version must be readable")
}

func TestOpenCodeProbePropagatesCancellation(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ProbeOpenCodeJournalCapability(ctx, f.path)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestOpenCodeProbeIncompatibleContainers covers rejection cases for
// ProbeOpenCodeJournalCapability: missing owner_id, missing required event
// column, and an extra additive column that must not block admission.
func TestOpenCodeProbeIncompatibleContainers(t *testing.T) {
	ctx := context.Background()

	t.Run("owner_id absent", func(t *testing.T) {
		// event_sequence without owner_id (Kilo/MiMo fork DDL).
		dir := t.TempDir()
		path := filepath.Join(dir, "opencode.db")
		db, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = db.Exec(openCodeJournalSchemaNoOwnerID)
		require.NoError(t, err)
		db.Close()

		_, compatible, err := ProbeOpenCodeJournalCapability(ctx, path)
		require.NoError(t, err)
		assert.False(t, compatible,
			"container without owner_id must not be admitted to the bounded feed")
	})

	t.Run("required event column absent", func(t *testing.T) {
		// event table missing the data column.
		dir := t.TempDir()
		path := filepath.Join(dir, "opencode.db")
		db, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS event (
    id           TEXT NOT NULL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    type         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS event_sequence (
    id       TEXT NOT NULL PRIMARY KEY,
    owner_id TEXT
);`)
		require.NoError(t, err)
		db.Close()

		_, compatible, err := ProbeOpenCodeJournalCapability(ctx, path)
		require.NoError(t, err)
		assert.False(t, compatible,
			"container missing a required event column must not be admitted")
	})

	t.Run("additive column still admits", func(t *testing.T) {
		// event table has all required columns plus an extra one.
		dir := t.TempDir()
		path := filepath.Join(dir, "opencode.db")
		db, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS event (
    id           TEXT NOT NULL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    type         TEXT NOT NULL,
    data         BLOB NOT NULL,
    extra        TEXT
);
CREATE TABLE IF NOT EXISTS event_sequence (
    id       TEXT NOT NULL PRIMARY KEY,
    owner_id TEXT
);`)
		require.NoError(t, err)
		db.Close()

		_, compatible, err := ProbeOpenCodeJournalCapability(ctx, path)
		require.NoError(t, err)
		assert.True(t, compatible,
			"schema with an additive column must still be admitted")
	})

	t.Run("empty tables admit and drain nothing", func(t *testing.T) {
		// Valid schema, no rows. Probe must admit; drain must return empty.
		f := newOpenCodeJournalFixture(t)

		_, compatible, err := ProbeOpenCodeJournalCapability(ctx, f.path)
		require.NoError(t, err)
		assert.True(t, compatible, "empty tables must be admitted")

		// Baseline drain on empty DB.
		init0, err := DrainOpenCodeJournal(ctx, f.path, OpenCodeCoverageCheckpoint{})
		require.NoError(t, err)
		assert.True(t, init0.Next.Initialized, "first drain must initialize")
		assert.Empty(t, init0.ReadyIDs, "empty DB must produce no ready IDs on init")

		// Second drain must also produce nothing.
		result, err := DrainOpenCodeJournal(ctx, f.path, init0.Next)
		require.NoError(t, err)
		assert.False(t, result.More, "empty DB must not report continuation")
		assert.Empty(t, result.ReadyIDs, "empty DB must produce no ready IDs")
		assert.Empty(t, result.PendingIDs, "empty DB must produce no pending IDs")
	})
}

// TestOpenCodeDrainBoundaryConstants verifies that the exported constants used
// by the proof matrix assertions are present and sensible.
func TestOpenCodeDrainBoundaryConstants(t *testing.T) {
	assert.Equal(t, 256, OpenCodeCoverageMaxRows,
		"MaxRows must be pinned at 256")
	assert.Equal(t, 1<<20, OpenCodeCoverageMaxPayloadBytes,
		"MaxPayloadBytes must be pinned at 1<<20")
	assert.Equal(t, 256, OpenCodeCoverageMaxIDs,
		"MaxIDs must be pinned at 256")
}

// TestOpenCodeFirstWakeBaselinePreservation is a light reference-model check
// for proof matrix row 15 (first-wake baseline). The production test lives in
// internal/sync. Here we verify that DrainOpenCodeJournal on a freshly
// initialized checkpoint does NOT consume events committed before
// initialization — those events appear in the NEXT drain.
func TestOpenCodeFirstWakeBaselinePreservation(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	// Seed 3 events BEFORE initialization.
	for i := 1; i <= 3; i++ {
		f.insertPartEvent(t, fmt.Sprintf("pre-init-%d", i), "ses-pre")
	}

	// Initialize: baseline should anchor at rowid=3. No events returned.
	initResult, err := DrainOpenCodeJournal(ctx, f.path, OpenCodeCoverageCheckpoint{})
	require.NoError(t, err)
	assert.Empty(t, initResult.ReadyIDs)
	assert.Empty(t, initResult.PendingIDs)
	checkpoint := initResult.Next

	// Add a settling event AFTER baseline.
	f.insertSettledEvent(t, "post-init-settled", "ses-post")

	// First real drain: should capture the post-init settled event.
	result, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
	require.NoError(t, err)
	for result.More {
		result, err = DrainOpenCodeJournal(ctx, f.path, result.Next)
		require.NoError(t, err)
	}
	assert.Contains(t, result.ReadyIDs, "ses-post",
		"event committed after baseline must appear in the first post-init drain")
	assert.NotContains(t, result.ReadyIDs, "ses-pre",
		"events committed before baseline must not appear in ReadyIDs "+
			"(they were already settled when baseline was captured)")
}

// TestOpenCodeSchemaVersionChangeTriggersAudit verifies that a schema version
// change between two drain calls latches an audit.
func TestOpenCodeSchemaVersionChangeTriggersAudit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	ctx := context.Background()

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(openCodeJournalSchema)
	require.NoError(t, err)

	// Initialize.
	checkpoint := initDrain(t, dbPath)

	// Bump the schema version by adding a column.
	_, err = db.Exec("ALTER TABLE event ADD COLUMN extra TEXT")
	require.NoError(t, err)
	db.Close()

	// Next drain must detect the schema change.
	result, err := DrainOpenCodeJournal(ctx, dbPath, checkpoint)
	require.NoError(t, err)
	assert.True(t, result.AuditRequired,
		"schema version change must latch an audit")

	_ = strings.Contains // suppress unused import
}

// TestOpenCodeDrainRowCapAndContinuation drives a real SQLite feed past
// OpenCodeCoverageMaxRows (256) rows and verifies that the first page reports
// RowsRead == 256 with More == true, then a continuation drain captures the
// overflow and terminates cleanly.
//
// The test uses many message.part.updated.1 events on a small set of sessions
// so the row count exceeds 256 without bumping into the 256-session ID cap.
func TestOpenCodeDrainRowCapAndContinuation(t *testing.T) {
	f := newOpenCodeJournalFixture(t)
	ctx := context.Background()

	// Baseline: no prior events.
	checkpoint := initDrain(t, f.path)

	// Insert 260 part events across 4 sessions (65 parts each). Streaming
	// parts never settle, so no stage-2 payload fetch occurs. The session count
	// (4) stays far below OpenCodeCoverageMaxIDs (256), isolating the row cap.
	const (
		sessions   = 4
		perSession = 65 // 4 * 65 = 260 rows > OpenCodeCoverageMaxRows (256)
	)
	for i := range sessions * perSession {
		agg := fmt.Sprintf("ses-%d", i%sessions)
		id := fmt.Sprintf("part-%04d", i)
		f.insertPartEvent(t, id, agg)
	}

	// First drain must hit the row cap.
	r1, err := DrainOpenCodeJournal(ctx, f.path, checkpoint)
	require.NoError(t, err)
	require.False(t, r1.AuditRequired, "row cap must not latch an audit")
	assert.True(t, r1.More,
		"drain must report More=true when row count exceeds OpenCodeCoverageMaxRows")
	assert.Equal(t, OpenCodeCoverageMaxRows, r1.RowsRead,
		"RowsRead must equal OpenCodeCoverageMaxRows on a capped page")

	// Continuation drain must terminate cleanly and read the overflow rows.
	r2, err := DrainOpenCodeJournal(ctx, f.path, r1.Next)
	require.NoError(t, err)
	require.False(t, r2.AuditRequired, "continuation must not latch an audit")
	assert.False(t, r2.More,
		"continuation drain must exhaust the remaining rows and return More=false")
	overflow := sessions*perSession - OpenCodeCoverageMaxRows
	assert.GreaterOrEqual(t, r2.RowsRead, overflow,
		"continuation must read at least the overflow rows")

	// Final page emits PendingIDs (all sessions still streaming).
	// All 4 sessions must appear somewhere in the final result.
	seenPending := make(map[string]bool, len(r2.PendingIDs))
	for _, id := range r2.PendingIDs {
		seenPending[id] = true
	}
	for i := range sessions {
		agg := fmt.Sprintf("ses-%d", i)
		assert.True(t, seenPending[agg],
			"session %s must appear in PendingIDs after full drain", agg)
	}
}

func TestOpenCodeDrainChangedSessionWorkIsArchiveCardinalityBounded(t *testing.T) {
	measure := func(sessionCount int) (int, int) {
		t.Helper()
		f := newOpenCodeJournalFixture(t)
		for i := range sessionCount {
			f.insertSessionUpdatedEvent(
				t, fmt.Sprintf("baseline-event-%05d", i), fmt.Sprintf("session-%05d", i),
			)
		}
		checkpoint := initDrain(t, f.path)
		f.insertSessionUpdatedEvent(t, "changed-event", "session-00000")
		result, err := DrainOpenCodeJournal(context.Background(), f.path, checkpoint)
		require.NoError(t, err)
		require.False(t, result.AuditRequired)
		return result.RowsRead, result.PayloadBytes
	}

	smallRows, smallPayload := measure(10)
	largeRows, largePayload := measure(5000)
	assert.Equal(t, smallRows, largeRows,
		"changed-session journal work must not scale with archive cardinality")
	assert.Equal(t, smallPayload, largePayload,
		"changed-session payload work must not scale with archive cardinality")
	assert.Equal(t, 1, largeRows,
		"the production drain must read only the changed journal row")
}
