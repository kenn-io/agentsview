//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wesm/agentsview/internal/db"
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	if _, err := pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// Local SQLite DB.
	localDB, err := db.Open(
		filepath.Join(t.TempDir(), "local.db"),
	)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
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
	if err := localDB.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

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
	if err := localDB.InsertMessages(msgs); err != nil {
		t.Fatalf("InsertMessages (first set): %v", err)
	}

	// First push.
	_, err = sync.Push(ctx, false, nil)
	if err != nil {
		t.Fatalf("Push (first): %v", err)
	}

	// Verify PG reflects system ordinals {0,4,5}.
	checkIsSystem(t, pg, sessID, firstSet, 7)

	// Switch to {1,2,6} — same sum(ordinal)=9, same sum(ordinal²)=41,
	// but the string fingerprint differs ("0,4,5" vs "1,2,6").
	// Replace local messages with updated is_system flags.
	secondSet := map[int]bool{1: true, 2: true, 6: true}
	for i := range 7 {
		msgs[i].IsSystem = secondSet[i]
	}
	if err := localDB.ReplaceSessionMessages(sessID, msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages (second set): %v", err)
	}

	// Force re-evaluation by clearing both the watermark and the cached
	// session-level boundary fingerprints. The session-level fingerprint
	// does not include is_system flags (only metadata like MessageCount),
	// so the boundary cache must be cleared for the incremental push to
	// reach pushMessages and compare the message-level string fingerprint.
	if err := localDB.SetSyncState("last_push_at", ""); err != nil {
		t.Fatalf("clearing last_push_at: %v", err)
	}
	if err := localDB.SetSyncState(lastPushBoundaryStateKey, ""); err != nil {
		t.Fatalf("clearing boundary state: %v", err)
	}

	// Second push — must NOT skip due to fingerprint match.
	_, err = sync.Push(ctx, false, nil)
	if err != nil {
		t.Fatalf("Push (second): %v", err)
	}

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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	if _, err := pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
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
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if err := sync.pushSession(ctx, tx, s); err != nil {
			_ = tx.Rollback()
			t.Fatalf("pushSession: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	pushOnce(sess)

	var got *string
	if err := pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got == nil || *got != "tool_call_pending" {
		t.Fatalf("got %v, want tool_call_pending", got)
	}

	// Update to NULL and verify ON CONFLICT clears it.
	sess.TerminationStatus = nil
	pushOnce(sess)

	if err := pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got); err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if got != nil {
		t.Fatalf("got %q, want NULL", *got)
	}
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
	if err != nil {
		t.Fatalf("querying PG messages: %v", err)
	}
	defer rows.Close()
	seen := make(map[int]bool, wantTotal)
	for rows.Next() {
		var ordinal int
		var isSystem bool
		if err := rows.Scan(&ordinal, &isSystem); err != nil {
			t.Fatalf("scanning row: %v", err)
		}
		seen[ordinal] = true
		want := wantSystem[ordinal]
		if isSystem != want {
			t.Errorf("ordinal %d: is_system=%v, want %v",
				ordinal, isSystem, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}
	if len(seen) != wantTotal {
		t.Errorf("PG has %d message rows for session %s, want %d",
			len(seen), sessID, wantTotal)
	}
	// Verify every expected ordinal was present (no gaps or substitutions).
	for i := range wantTotal {
		if !seen[i] {
			t.Errorf("ordinal %d missing from PG messages", i)
		}
	}
}
