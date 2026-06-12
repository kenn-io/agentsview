//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushSystemFingerprintCollisionRegression verifies that the fast-path
// in pushMessages correctly detects a change when the is_system flags are
// reclassified between two ordinal sets that previously collided under the
// two-component (SUM, SUM-of-squares) fingerprint: {0,4,5} and {1,2,6}
// both produce sum=9, sumSq=41.
//
// Steps:
//  1. Push a session with 7 messages where ordinals {0,4,5} are system.
//  2. Without changing content lengths, reclassify to {1,2,6} as system.
//  3. Push again with full=false.
//  4. Confirm PG now reflects the updated is_system values.
func TestPushSystemFingerprintCollisionRegression(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_sysfingerprint_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	// Local SQLite DB.
	localDB, err := db.Open(
		filepath.Join(t.TempDir(), "local.db"),
	)
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:      pg,
		local:   localDB,
		machine: "test-machine",
		schema:  schema,
		// Mark schema done so Push skips EnsureSchema.
		schemaDone: true,
	}

	const sessID = "fp-collision-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "test-machine",
		Agent:        "claude",
		MessageCount: 7,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	// First set: system ordinals {0,4,5}.
	firstSet := map[int]bool{0: true, 4: true, 5: true}
	msgs := make([]db.Message, 7)
	for i := range 7 {
		msgs[i] = db.Message{
			SessionID:     sessID,
			Ordinal:       i,
			Role:          "user",
			Content:       "x",
			ContentLength: 1,
			IsSystem:      firstSet[i],
		}
	}
	require.NoError(t, localDB.InsertMessages(msgs), "InsertMessages (first set)")

	// First push.
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (first)")

	// Verify PG reflects system ordinals {0,4,5}.
	checkIsSystem(t, pg, sessID, firstSet, 7)

	// Switch to {1,2,6} — same sum(ordinal)=9, same sum(ordinal²)=41,
	// but the string fingerprint differs ("0,4,5" vs "1,2,6").
	// Replace local messages with updated is_system flags.
	secondSet := map[int]bool{1: true, 2: true, 6: true}
	for i := range 7 {
		msgs[i].IsSystem = secondSet[i]
	}
	require.NoError(t, localDB.ReplaceSessionMessages(sessID, msgs),
		"ReplaceSessionMessages (second set)")

	// Force re-evaluation by clearing both the watermark and the cached
	// session-level boundary fingerprints. The session-level fingerprint
	// does not include is_system flags (only metadata like MessageCount),
	// so the boundary cache must be cleared for the incremental push to
	// reach pushMessages and compare the message-level string fingerprint.
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	// Second push — must NOT skip due to fingerprint match.
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (second)")

	// Verify PG now reflects updated system ordinals {1,2,6}.
	checkIsSystem(t, pg, sessID, secondSet, 7)
}

// TestPushSessionTerminationStatus verifies that pushSession round-trips
// the termination_status column to PG: a non-nil value writes the string,
// and a subsequent push with nil clears the column back to NULL via the
// ON CONFLICT DO UPDATE path.
func TestPushSessionTerminationStatus(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_termstatus_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	pending := "tool_call_pending"
	sess := db.Session{
		ID:               "term-test-1",
		Project:          "p",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		// CreatedAt must be parseable by ParseSQLiteTimestamp;
		// PG's NOT NULL on created_at would otherwise reject NULL.
		CreatedAt:         "2024-01-01T00:00:00Z",
		TerminationStatus: &pending,
	}

	pushOnce := func(s db.Session) {
		t.Helper()
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err, "BeginTx")
		if err := sync.pushSession(ctx, tx, s); err != nil {
			_ = tx.Rollback()
			t.Fatalf("pushSession: %v", err)
		}
		require.NoError(t, tx.Commit(), "Commit")
	}

	pushOnce(sess)

	var got *string
	require.NoError(t, pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got), "read back")
	require.NotNil(t, got)
	assert.Equal(t, "tool_call_pending", *got)

	// Update to NULL and verify ON CONFLICT clears it.
	sess.TerminationStatus = nil
	pushOnce(sess)

	require.NoError(t, pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got), "read back 2")
	assert.Nil(t, got)
}

// TestPushSyncsUsageEventsForZeroMessageSession verifies that a session
// carrying token/cost accounting as a usage_event but no transcript
// messages still has its usage_event pushed to PG. This is the shape of a
// hermes state.db-only session: parseHermesStateSession emits a single
// usage_event (model + tokens) with MessageCount 0. The session row (and
// its aggregate token columns) pushes via pushSession, but pushMessages
// must not skip usage_event syncing just because the message count is 0 --
// otherwise the dashboard shows tokens with a $0 cost.
func TestPushSyncsUsageEventsForZeroMessageSession(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_zeromsg_usage_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "hermes:zero-msg-001"
	started := "2026-05-26T10:00:00Z"
	sess := db.Session{
		ID:                   sessID,
		Project:              "hermes-proj",
		Machine:              "test-machine",
		Agent:                "hermes",
		MessageCount:         0,
		StartedAt:            &started,
		CreatedAt:            started,
		TotalOutputTokens:    500000,
		HasTotalOutputTokens: true,
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	// gpt-5.5 usage event with NULL cost so it is priced from the catalog.
	require.NoError(t, localDB.ReplaceSessionUsageEvents(sessID, []db.UsageEvent{{
		SessionID:    sessID,
		Source:       "session",
		Model:        "gpt-5.5",
		InputTokens:  1000000,
		OutputTokens: 500000,
		CostUSD:      nil,
		OccurredAt:   started,
		DedupKey:     "session:" + sessID,
	}}), "ReplaceSessionUsageEvents")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	// The usage_event must reach PG even though the session has no messages.
	var pgUsageCount int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM usage_events WHERE session_id = $1`,
		sessID,
	).Scan(&pgUsageCount), "count pg usage_events")
	assert.Equal(t, 1, pgUsageCount,
		"usage_event for a zero-message session was not pushed")

	// And the read side prices it from the gpt-5.5 catalog rate:
	// input 5/Mtok, output 30/Mtok -> 1.0*5 + 0.5*30 = 20.
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-05-26",
		To:       "2026-05-26",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.InDelta(t, 20.0, result.Totals.TotalCost, 1e-9,
		"gpt-5.5 usage should be priced from the catalog")
}

// checkIsSystem asserts that PG contains exactly wantTotal rows for the
// session with ordinals 0..wantTotal-1, and that each row's is_system
// matches wantSystem. Tracking the exact ordinal set prevents false
// positives from wrong-but-equal-count row sets.
func checkIsSystem(
	t *testing.T,
	pg *sql.DB,
	sessID string,
	wantSystem map[int]bool,
	wantTotal int,
) {
	t.Helper()
	rows, err := pg.Query(
		`SELECT ordinal, is_system FROM messages
		 WHERE session_id = $1 ORDER BY ordinal`,
		sessID,
	)
	require.NoError(t, err, "querying PG messages")
	defer rows.Close()
	seen := make(map[int]bool, wantTotal)
	for rows.Next() {
		var ordinal int
		var isSystem bool
		require.NoError(t, rows.Scan(&ordinal, &isSystem), "scanning row")
		seen[ordinal] = true
		want := wantSystem[ordinal]
		assert.Equal(t, want, isSystem, "ordinal %d is_system", ordinal)
	}
	require.NoError(t, rows.Err(), "rows error")
	assert.Len(t, seen, wantTotal,
		"PG has %d message rows for session %s, want %d",
		len(seen), sessID, wantTotal)
	// Verify every expected ordinal was present (no gaps or substitutions).
	for i := range wantTotal {
		assert.True(t, seen[i], "ordinal %d missing from PG messages", i)
	}
}

// TestPushMessagesSanitizesNULBytes verifies that a message whose
// model and source fields carry NUL bytes (observed in production:
// the Antigravity gen_metadata heuristic persisted a raw protobuf
// fragment as the model name) pushes to PG without the whole-session
// rollback caused by SQLSTATE 22021 (invalid byte sequence for
// encoding "UTF8": 0x00). Model and source fields come from
// third-party session files, so the push boundary must be defensive
// regardless of any single parser fix.
func TestPushMessagesSanitizesNULBytes(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_nul_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(
		filepath.Join(t.TempDir(), "local.db"),
	)
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	// The exact 10-byte protobuf fragment persisted as a model
	// name by the pre-fix Antigravity parser (hex
	// 080020022A0201024001, contains 0x00).
	badModel := "\x08\x00\x20\x02\x2a\x02\x01\x02\x40\x01"

	const sessID = "nul-bytes-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "test-machine",
		Agent:        "antigravity",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
		Cwd:          "/tmp/with\x00nul",
		GitBranch:    "main\x00",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	msgs := []db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "thinking summary",
		ContentLength: 16,
		Model:         badModel,
		SourceUUID:    "uuid\x00tail",
	}}
	require.NoError(t, localDB.InsertMessages(msgs), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.Zero(t, res.Errors, "push should report no failed sessions")

	var model, sourceUUID string
	err = pg.QueryRow(
		`SELECT model, source_uuid FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&model, &sourceUUID)
	require.NoError(t, err, "querying pushed message")
	assert.Equal(t, sanitizePG(badModel), model, "model stripped of NUL")
	assert.NotContains(t, model, "\x00")
	assert.Equal(t, "uuidtail", sourceUUID, "source_uuid stripped of NUL")

	// Second push with sync state cleared: the local token
	// fingerprint (sanitized, see db.SanitizeUTF8) must match the
	// PG-readback fingerprint despite the NUL bytes still stored
	// locally, so the metadata fast path skips the rewrite. ctid
	// changes on DELETE+reinsert, so an unchanged ctid proves the
	// row was left alone.
	var ctidBefore string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidBefore), "reading ctid before second push")

	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t,
		localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (second)")
	assert.Zero(t, res.Errors, "second push should report no failures")

	var ctidAfter string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidAfter), "reading ctid after second push")
	assert.Equal(t, ctidBefore, ctidAfter,
		"fast path should skip rewriting a NUL-field session")
}
