//go:build !(windows && arm64)

package duckdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
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

// TestPushIncrementalReplacesOnlyChangedSessions is the core incremental
// contract: after an initial push, only a session whose content actually
// changed is re-pushed on the next call.
func TestPushIncrementalReplacesOnlyChangedSessions(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 3)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	appendMessage(t, local, "sess-2")
	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)
	assert.LessOrEqual(t, res.Diagnostics.CandidateSessions.Total, 2)
	assertMirrorMessageCount(t, path, "sess-2", 3)
}

// TestPushBoundaryEqualSessionIsNotLost regression-tests the inclusive
// mirror window: an update whose sync_marker equals the stored cutoff must
// still be selected and pushed when its fingerprint differs, not skipped
// forever because the window boundary was treated as exclusive.
func TestPushBoundaryEqualSessionIsNotLost(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)

	setSessionSignalsTo(t, local, "sess-1", probe.LastPushCutoff)
	mutateSessionContent(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)
}

// TestPushFutureMarkerSessionStillReceivesLaterChanges is the FINDING 3
// regression: sync_marker is the MAX of all timestamp signals, so one
// future-dated signal (a clock-skewed file_mtime here) pushes the marker
// past any wall-clock cutoff. With the old upper-bounded window
// [cutoff, now], such a session fell outside every incremental window
// until wall time caught up, so later real content changes stayed
// unmirrored. The window is now [cutoff, +inf): the session is a perpetual
// candidate whose changes propagate immediately (and whose unchanged
// pushes are cheaply fingerprint-skipped).
func TestPushFutureMarkerSessionStillReceivesLaterChanges(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	futureMtime := time.Now().Add(90 * 24 * time.Hour).UnixNano()
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE sessions SET file_mtime = ? WHERE id = ?`,
			futureMtime, "sess-1",
		)
		return err
	}))
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	mutateSessionContent(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"a future-dated sync_marker must not mask later content changes")

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var content string
	require.NoError(t, conn.QueryRow(
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		"sess-1",
	).Scan(&content))
	assert.Equal(t, "mutated content", content)
}

// TestPushIncrementalMirrorsUsageOnlyChange regression-tests the usage-only
// gap: ReplaceSessionUsageEvents rewrites usage_events without any session
// file change, so unless it bumps local_modified_at (and via the trigger,
// sync_marker) the session never becomes an incremental candidate and the
// rewrite stays permanently absent from the mirror.
func TestPushIncrementalMirrorsUsageOnlyChange(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 2)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	// Usage-only rewrite: no message or session-file change.
	cost := 1.25
	require.NoError(t, local.ReplaceSessionUsageEvents("sess-2", []db.UsageEvent{{
		SessionID:    "sess-2",
		Source:       "session",
		Model:        "model-x",
		InputTokens:  10,
		OutputTokens: 5,
		CostUSD:      &cost,
		OccurredAt:   "2026-02-01T00:02:30.000Z",
	}}))

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"a usage-only rewrite must re-select the session for the mirror")

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var model string
	var costUSD float64
	require.NoError(t, conn.QueryRow(
		`SELECT model, cost_usd FROM usage_events WHERE session_id = ?`,
		"sess-2",
	).Scan(&model, &costUSD))
	assert.Equal(t, "model-x", model)
	assert.InDelta(t, 1.25, costUSD, 1e-9)
}

func TestPushAppliesDeletionJournalDelta(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 2)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	require.NoError(t, local.DeleteSession("sess-1"))
	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions)
	assertMirrorSessionAbsent(t, path, "sess-1")
}

// TestPushRebuildTriggers verifies every probe condition that forces a
// rebuild instead of an incremental push, and that the rebuilt mirror is
// coherent afterward.
func TestPushRebuildTriggers(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		mangle        func(t *testing.T, path string)
		wantReasonHas string
	}{
		{
			name: "schema version too old",
			mangle: func(t *testing.T, path string) {
				setMirrorMetadataValue(t, path, schemaVersionMetadataKey, "2")
			},
			wantReasonHas: "schema version",
		},
		{
			name: "schema version too new",
			mangle: func(t *testing.T, path string) {
				setMirrorMetadataValue(t, path, schemaVersionMetadataKey, "99")
			},
			wantReasonHas: "schema version",
		},
		{
			name: "data version drift",
			mangle: func(t *testing.T, path string) {
				setMirrorMetadataValue(t, path, dataVersionMetadataKey, "999999")
			},
			wantReasonHas: "data version",
		},
		{
			name: "scope drift",
			mangle: func(t *testing.T, path string) {
				setMirrorMetadataValue(t, path, pushScopeMetadataKey, `{"projects":["other"]}`)
			},
			wantReasonHas: "scope changed",
		},
		{
			name: "deleted mirror file",
			mangle: func(t *testing.T, path string) {
				require.NoError(t, os.Remove(path))
			},
			wantReasonHas: "missing file",
		},
		{
			name: "dropped mirror table with sentinel intact",
			mangle: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(`DROP TABLE tool_result_events`)
				require.NoError(t, err)
			},
			wantReasonHas: "shape issue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)

			tt.mangle(t, path)

			res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)
			assert.True(t, res.Diagnostics.Full)
			assert.Contains(t, res.Diagnostics.RebuildReason, tt.wantReasonHas)
			assertMirrorMessageCount(t, path, "sess-1", 2)
		})
	}
}

// TestPushRefusesToReplaceUnrecognizedExistingFile is the fail-closed
// overwrite guard (see ensureReplaceableMirror): a rebuild may only replace
// an existing file positively identified as an agentsview DuckDB mirror. A
// SQLite database (what [duckdb].path pointed at the primary sessions.db
// would look like), an arbitrary file, or a foreign DuckDB database with
// none of our tables must make the push fail with the file left untouched,
// for both an incremental request that degrades to a rebuild and an
// explicit --full push.
func TestPushRefusesToReplaceUnrecognizedExistingFile(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{
			name: "sqlite database file",
			write: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(
					path, []byte("SQLite format 3\x00not a duckdb mirror"), 0o644,
				))
			},
		},
		{
			name: "foreign duckdb database",
			write: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(`CREATE TABLE unrelated (x INTEGER)`)
				require.NoError(t, err)
			},
		},
		{
			name: "foreign duckdb database with generic sessions table",
			write: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(`CREATE TABLE sessions (id TEXT)`)
				require.NoError(t, err)
			},
		},
		{
			// A stale sidecar marker must not weaken openable-file
			// recognition: for a file the probe can actually inspect, the
			// in-database sentinel is the only recognition signal.
			name: "foreign duckdb database with stale marker",
			write: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(`CREATE TABLE unrelated (x INTEGER)`)
				require.NoError(t, err)
				require.NoError(t, writeMirrorMarker(path, "m"))
			},
		},
		{
			name: "foreign duckdb database with sync_metadata but no agentsview key",
			write: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(
					`CREATE TABLE sync_metadata (key TEXT PRIMARY KEY, value TEXT)`,
				)
				require.NoError(t, err)
				_, err = conn.Exec(
					`INSERT INTO sync_metadata (key, value) VALUES ('other_tool', '1')`,
				)
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, full := range []bool{false, true} {
				t.Run(fmt.Sprintf("full_%v", full), func(t *testing.T) {
					local, path := newPushFixture(t, 1)
					tt.write(t, path)
					before, err := os.ReadFile(path)
					require.NoError(t, err)

					_, err = Push(ctx, path, local, "m", SyncOptions{}, full, nil)
					require.Error(t, err)
					assert.Contains(t, err.Error(),
						"not an agentsview duckdb mirror")

					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, after,
						"a refused push must leave the existing file byte-identical")
				})
			}
		})
	}
}

// TestPushWritesMirrorMarkerAfterRebuild verifies a successful rebuild
// leaves the sidecar ownership marker next to the mirror (one JSON line;
// existence is the recognition signal, content is informational).
func TestPushWritesMirrorMarkerAfterRebuild(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)

	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(MirrorMarkerPath(path))
	require.NoError(t, err)
	var content map[string]any
	require.NoError(t, json.Unmarshal(data, &content))
	assert.Equal(t, float64(SchemaVersion), content["schema_version"])
	assert.Equal(t, "m", content["machine"])
	assert.NotEmpty(t, content["written_at"])
}

// TestPushHealsMissingMirrorMarkerOnIncrementalPush covers the pre-marker
// upgrade path: a mirror created by an agentsview version without the
// sidecar marker regains one at the end of its next unlocked incremental
// push, so later locked probes recognize it again.
func TestPushHealsMissingMirrorMarkerOnIncrementalPush(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.NoError(t, os.Remove(MirrorMarkerPath(path)))

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.FileExists(t, MirrorMarkerPath(path))
}

// TestPushFailsClosedWhenLiveMirrorHasNoMarker is the FINDING 1 regression:
// a file some DuckDB process is holding open cannot be inspected, so before
// the sidecar marker existed it was recognized on faith and a rebuild would
// atomically overwrite it — even when the path was misconfigured to point at
// a foreign served database. The same-process double-open rejection (a live
// handle in this process, exactly what a serving Store looks like) is the
// in-process-reproducible stand-in for a cross-process lock conflict: both
// probe as Uninspectable and both must fail closed, file untouched, when no
// marker exists next to the file.
// Byte reads bracket the live-handle window instead of overlapping it: the
// "before" bytes are captured before Open and the "after" bytes only after
// Close, because Windows sharing semantics reject reading a file another
// DuckDB handle has open (POSIX allows it). The push under test runs while
// the handle is held either way, which is the scenario being pinned.
func TestPushFailsClosedWhenLiveMirrorHasNoMarker(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.NoError(t, os.Remove(MirrorMarkerPath(path)))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	held, err := Open(path)
	require.NoError(t, err)

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no agentsview marker")
	assert.Contains(t, err.Error(), MirrorMarkerPath(path))

	require.NoError(t, held.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"a refused push must leave the existing file byte-identical")
	assertMirrorMessageCount(t, path, "sess-1", 2)
}

// TestPushFailsClosedWhenLiveMirrorMarkerIdentityMismatches closes the
// stale-marker residual edge of FINDING 1: after the mirror is manually
// replaced by an unrelated DuckDB database that a live process then holds
// open, the leftover marker still sits next to the path but records the
// OLD mirror's filesystem identity. The push must fail closed with the
// identity-mismatch error and leave the foreign file byte-identical, not
// atomically overwrite it. Byte reads bracket the live-handle window for
// Windows sharing semantics (see TestPushFailsClosedWhenLiveMirrorHasNoMarker).
func TestPushFailsClosedWhenLiveMirrorMarkerIdentityMismatches(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.FileExists(t, MirrorMarkerPath(path))

	// Manually swap a foreign DuckDB database into the exact mirror path.
	// It is created next to the still-live mirror, so the two files have
	// distinct filesystem identities even on inode-recycling filesystems.
	foreignPath := filepath.Join(filepath.Dir(path), "foreign.duckdb")
	foreign, err := Open(foreignPath)
	require.NoError(t, err)
	_, err = foreign.Exec(`CREATE TABLE unrelated (x INTEGER)`)
	require.NoError(t, err)
	require.NoError(t, foreign.Close())
	require.NoError(t, os.Rename(foreignPath, path))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	held, err := Open(path)
	require.NoError(t, err)

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the mirror this marker")
	assert.Contains(t, err.Error(), MirrorMarkerPath(path))

	require.NoError(t, held.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"a refused push must leave the foreign file byte-identical")
}

// TestPushRebuildRebindsMarkerToSwappedFile pins the marker's identity
// binding across a rebuild: the swap renames a brand-new file over the
// mirror path, so the recorded identity from the previous push no longer
// describes the file at the path, and the freshly written marker must
// record the post-swap file's identity instead.
func TestPushRebuildRebindsMarkerToSwappedFile(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	first := readMarkerIdentity(t, path)

	_, err = Push(ctx, path, local, "m", SyncOptions{}, true, nil)
	require.NoError(t, err)

	second := readMarkerIdentity(t, path)
	current, err := fileIdentityForPath(path)
	require.NoError(t, err)
	assert.Equal(t, current, second,
		"the marker must record the post-swap file's identity")
	assert.NotEqual(t, first, second,
		"a rebuild swaps in a new file, so the recorded identity must change")
}

// TestPushRewritesStaleMarkerIdentityOnIncrementalPush pins the
// self-healing side of the identity binding: a successful unlocked push
// proves the file at the path is the mirror it just wrote, so a marker
// whose recorded identity disagrees (or records none) is rewritten to
// match instead of being left to fail the next locked probe.
func TestPushRewritesStaleMarkerIdentityOnIncrementalPush(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	stale := readMarker(t, path)
	stale.FileIdentity = fileIdentity{A: 1, B: 2, C: 3}
	data, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(MirrorMarkerPath(path), data, 0o644))

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)

	current, err := fileIdentityForPath(path)
	require.NoError(t, err)
	assert.Equal(t, current, readMarkerIdentity(t, path),
		"an unlocked push must rewrite a marker whose identity went stale")
}

func readMarker(t *testing.T, path string) mirrorMarkerContent {
	t.Helper()
	data, err := os.ReadFile(MirrorMarkerPath(path))
	require.NoError(t, err)
	var content mirrorMarkerContent
	require.NoError(t, json.Unmarshal(data, &content))
	return content
}

func readMarkerIdentity(t *testing.T, path string) fileIdentity {
	t.Helper()
	identity := readMarker(t, path).FileIdentity
	require.False(t, identity.isZero(),
		"every push-written marker must record a file identity")
	return identity
}

// TestPushRebuildsOverLiveMirrorWithMarker is the recognized side of the
// FINDING 1 boundary: with the sidecar marker present, a mirror held open by
// a live DuckDB handle (the push-under-serve case) still rebuilds via
// temp-file-plus-rename exactly as before the marker gate existed, and the
// rebuild refreshes the marker.
func TestPushRebuildsOverLiveMirrorWithMarker(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.FileExists(t, MirrorMarkerPath(path))

	held, err := Open(path)
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full,
		"a held mirror cannot be updated incrementally; push must rebuild")
	assert.FileExists(t, MirrorMarkerPath(path))

	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 2)
}

// TestPushIncrementalMirrorsSubagentLinkBackfill is the FINDING 4
// regression: LinkSubagentSessions rewrites a session's parent_session_id
// and relationship_type without any session file changing, so unless it
// bumps a sync_marker signal the linked session never re-enters the
// incremental window and the mirror keeps the stale relationship until the
// next full rebuild.
func TestPushIncrementalMirrorsSubagentLinkBackfill(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	ts := "2026-02-01T00:01:00.000Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession("parent-1", "alpha", "parent", ts, 2),
			Messages: []db.Message{
				syncMessage("parent-1", 0, "user", "spawn a subagent", ts),
				syncMessage("parent-1", 1, "assistant", "[Task: subagent]", ts,
					db.ToolCall{
						SessionID: "parent-1",
						ToolName:  "Task",
						Category:  "Task",
						ToolUseID: "toolu_child",
					}),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("child-1", "alpha", "child", ts, 1),
			Messages: []db.Message{
				syncMessage("child-1", 0, "user", "child work", ts),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assertMirrorSessionRelationship(t, path, "child-1", "", "root")

	// The linkage is discovered later, with the session files untouched.
	require.NoError(t, local.SetToolCallSubagentSession(
		"parent-1", "toolu_child", "child-1"))
	require.NoError(t, local.LinkSubagentSessions())

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full,
		"the follow-up push must be incremental")
	assertMirrorSessionRelationship(t, path, "child-1", "parent-1", "subagent")
}

func assertMirrorSessionRelationship(
	t *testing.T, path, sessionID, wantParent, wantRelationship string,
) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var parent sql.NullString
	var relationship string
	require.NoError(t, conn.QueryRow(
		`SELECT parent_session_id, relationship_type
		 FROM sessions WHERE id = ?`, sessionID,
	).Scan(&parent, &relationship))
	assert.Equal(t, wantParent, parent.String, "mirror parent_session_id")
	assert.Equal(t, wantRelationship, relationship, "mirror relationship_type")
}

// TestPushDefersLockedRebuildForAutomaticPushes is the FINDING 3
// regression: a watch-mode (automatic) push that hits a recognized mirror
// held by a live DuckDB process must return a successful deferred no-op —
// not rebuild the whole archive on every changed batch — and must not
// advance the mirror's push cutoff, so the next unlocked push catches up
// on everything that changed while the mirror was held.
func TestPushDefersLockedRebuildForAutomaticPushes(t *testing.T) {
	ctx := context.Background()
	watchOpts := SyncOptions{Automatic: true}
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", watchOpts, false, nil)
	require.NoError(t, err)
	baseline, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	require.True(t, baseline.ShapeOK)

	appendMessage(t, local, "sess-1")
	held, err := Open(path)
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "m", watchOpts, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Deferred, "the locked push must defer")
	assert.Contains(t, res.Diagnostics.DeferredReason, "locked by a serving process")
	assert.False(t, res.Diagnostics.Full, "a deferred push must not rebuild")
	assert.Zero(t, res.SessionsPushed, "a deferred push must not write sessions")

	require.NoError(t, held.Close())
	after, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, baseline.LastPushCutoff, after.LastPushCutoff,
		"a deferred push must not advance the mirror's push cutoff")
	assertMirrorMessageCount(t, path, "sess-1", 2)

	catchUp, err := Push(ctx, path, local, "m", watchOpts, false, nil)
	require.NoError(t, err)
	assert.False(t, catchUp.Diagnostics.Deferred)
	assert.False(t, catchUp.Diagnostics.Full,
		"the unlocked catch-up push must be incremental, not a rebuild")
	assert.Equal(t, 1, catchUp.SessionsPushed,
		"the catch-up push must pick up the change made while the mirror was held")
	assertMirrorMessageCount(t, path, "sess-1", 3)
}

// TestDeferLockedRebuildConditions pins exactly when an automatic push may
// defer: only with the opt-in set, no --full request, and a
// marker-recognized mirror held by a live DuckDB process. Everything else
// keeps today's behavior — full rebuilds still rebuild, an unrecognized
// locked file still falls through to the fail-closed overwrite guard, and
// an inspectable mirror never defers.
func TestDeferLockedRebuildConditions(t *testing.T) {
	heldRecognized := MirrorProbe{
		FileExists: true, Uninspectable: true, LockConflict: true,
		RecognizedMirror: true,
	}
	tests := []struct {
		name  string
		opts  SyncOptions
		full  bool
		probe MirrorProbe
		want  bool
	}{
		{"opted-in locked recognized", SyncOptions{Automatic: true},
			false, heldRecognized, true},
		{"opted-in same-process hold", SyncOptions{Automatic: true},
			false, MirrorProbe{
				FileExists: true, Uninspectable: true, RecognizedMirror: true,
			}, true},
		{"no opt-in", SyncOptions{}, false, heldRecognized, false},
		{"full requested", SyncOptions{Automatic: true},
			true, heldRecognized, false},
		{"unrecognized locked file", SyncOptions{Automatic: true},
			false, MirrorProbe{FileExists: true, Uninspectable: true}, false},
		{"inspectable mirror", SyncOptions{Automatic: true},
			false, MirrorProbe{
				FileExists: true, ShapeOK: true, RecognizedMirror: true,
			}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, deferred := deferLockedRebuild(tt.opts, tt.full, tt.probe)
			assert.Equal(t, tt.want, deferred)
			assert.Equal(t, tt.want, result.Diagnostics.Deferred)
			if tt.want {
				assert.NotEmpty(t, result.Diagnostics.DeferredReason)
			}
		})
	}
}

// TestAutomaticIncrementalPushSkipsScopeCount pins the bounded-cost side of
// SyncOptions.Automatic: an automatic incremental push must not run the
// archive-scale CountSessionsForMirrorScope diagnostics COUNT, so
// Diagnostics.LocalSessionCount stays 0 while the same archive reports 1 on
// an explicit incremental push. The local *db.DB offers no query
// interception, so the zero assertion plus the !opts.Automatic gate in
// runIncrementalPush pins the contract.
func TestAutomaticIncrementalPushSkipsScopeCount(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	appendMessage(t, local, "sess-1")
	auto, err := Push(ctx, path, local, "m", SyncOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	require.False(t, auto.Diagnostics.Full,
		"fixture must exercise the incremental path")
	assert.Equal(t, 1, auto.SessionsPushed,
		"the automatic push must still push the changed session")
	assert.Zero(t, auto.Diagnostics.LocalSessionCount,
		"an automatic push must skip the archive-scale scope count")

	appendMessage(t, local, "sess-1")
	explicit, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.False(t, explicit.Diagnostics.Full,
		"fixture must exercise the incremental path")
	assert.Equal(t, 1, explicit.Diagnostics.LocalSessionCount,
		"an explicit incremental push still reports the scope count")
}

// TestPushFailsClosedWhenMirrorLosesSentinel pins the strict side of the
// recognition boundary: recognition requires the agentsview sentinel (the
// agentsview_schema_version row in sync_metadata), not just familiar table
// names. A once-valid mirror that lost its sentinel row, or its whole
// sync_metadata table, is indistinguishable from a foreign DuckDB database
// and must fail closed with the file untouched instead of being rebuilt
// over.
func TestPushFailsClosedWhenMirrorLosesSentinel(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mangle func(t *testing.T, path string)
	}{
		{
			name: "deleted schema version sentinel row",
			mangle: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(
					`DELETE FROM sync_metadata WHERE key = ?`, schemaVersionMetadataKey,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "dropped sync_metadata table",
			mangle: func(t *testing.T, path string) {
				conn, err := Open(path)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.Exec(`DROP TABLE sync_metadata`)
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)

			tt.mangle(t, path)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not an agentsview duckdb mirror")

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after,
				"a refused push must leave the existing file byte-identical")
		})
	}
}

// TestPushRebuildsOverOldSchemaVersionMirror pins the recognition boundary
// from the other side: a real agentsview mirror whose recorded schema
// version predates this build is still recognized (it carries the
// agentsview sentinel) and must rebuild normally rather than fail the
// overwrite guard.
func TestPushRebuildsOverOldSchemaVersionMirror(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	setMirrorMetadataValue(t, path, schemaVersionMetadataKey, "1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full)
	assert.Contains(t, res.Diagnostics.RebuildReason, "schema version")
	assertMirrorMessageCount(t, path, "sess-1", 2)
}

// TestPushRebuildReasonReportsFullFlag verifies an explicitly requested
// --full push records that as its RebuildReason even though the existing
// mirror would otherwise be valid for an incremental push.
func TestPushRebuildReasonReportsFullFlag(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "m", SyncOptions{}, true, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full)
	assert.Equal(t, "--full requested", res.Diagnostics.RebuildReason)
}

// TestPushRebuildsWhenMirrorDeletionCursorAheadOfLocal is the FIX2b
// regression: a mirror whose recorded deletion journal revision is higher
// than the local archive's current counter (the archive was rebuilt or
// replaced, e.g. by a resync, and its own counter no longer reaches that
// far) must trigger a rebuild instead of failing LoadSessionDeletionDelta's
// window validation with "invalid session deletion publication window".
func TestPushRebuildsWhenMirrorDeletionCursorAheadOfLocal(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	setMirrorMetadataValue(t, path, deletionRevisionMetadataKey, "5")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full)
	assert.Equal(t,
		"mirror deletion cursor ahead of archive; archive was rebuilt",
		res.Diagnostics.RebuildReason,
	)
	assertMirrorMessageCount(t, path, "sess-1", 2)
}

// TestPushRebuildsWhenMirrorBuiltFromDifferentArchive covers the
// source-database-id gate: a mirror records which SQLite archive generation
// built it, and pointing a push at a mirror built from a DIFFERENT archive —
// same machine, scope, schema/data versions, and a deletion revision that is
// not behind — must run a full rebuild. Without the gate the push takes the
// incremental path: sessions unique to the old archive persist forever, and
// the new archive's sessions whose sync_markers sit below the mirror's
// stored cutoff are never copied.
func TestPushRebuildsWhenMirrorBuiltFromDifferentArchive(t *testing.T) {
	ctx := context.Background()
	archiveA := newLocalDB(t)
	require.NoError(t, archiveA.SetDatabaseIDForTest(ctx, "archive-a"))
	tsA := "2026-02-01T00:00:00.000Z"
	_, err := archiveA.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("a-only-1", "alpha", "a1", tsA, 1),
			Messages:        []db.Message{syncMessage("a-only-1", 0, "user", "a1", tsA)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("a-only-2", "alpha", "a2", tsA, 1),
			Messages:        []db.Message{syncMessage("a-only-2", 0, "user", "a2", tsA)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	resA, err := Push(ctx, path, archiveA, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.True(t, resA.Diagnostics.Full, "first push against a fresh path is a rebuild")

	// An independent archive whose scope, machine, and versions all coincide
	// with what the mirror records, and whose deletion revision (0) is not
	// behind the mirror's. Only the database id differs.
	archiveB := newLocalDB(t)
	require.NoError(t, archiveB.SetDatabaseIDForTest(ctx, "archive-b"))
	tsB := "2026-02-02T00:00:00.000Z"
	_, err = archiveB.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("b-old", "alpha", "b old", tsB, 1),
			Messages:        []db.Message{syncMessage("b-old", 0, "user", "b old", tsB)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("b-new", "alpha", "b new", tsB, 1),
			Messages:        []db.Message{syncMessage("b-new", 0, "user", "b new", tsB)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)
	// Pin one B session's sync_marker strictly below the cutoff archive A's
	// push stored: an incremental push's window can never select it, so only
	// a full rebuild ever copies it into the mirror.
	setSessionSignalsTo(t, archiveB, "b-old", "2020-01-01T00:00:00.000Z")

	res, err := Push(ctx, path, archiveB, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full,
		"a mirror built from a different archive must be fully rebuilt")
	assert.Contains(t, res.Diagnostics.RebuildReason, "different archive",
		"the rebuild reason must name the source archive change")

	assertMirrorSessionAbsent(t, path, "a-only-1")
	assertMirrorSessionAbsent(t, path, "a-only-2")
	assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "b-old", 1)
	assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "b-new", 1)

	// The rebuilt mirror now records archive B's id, so the next push from
	// the same archive proceeds incrementally again.
	resAgain, err := Push(ctx, path, archiveB, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, resAgain.Diagnostics.Full,
		"a matching source database id must allow the incremental path")
}

// TestPushRebuildsWhenMachineNameChanges is the FIX1 regression: mirror rows
// are machine-stamped, and an incremental push only re-pushes sessions whose
// LOCAL content changed within the current window. A session that has not
// changed locally since the mirror's last push would otherwise stay
// permanently labeled with the OLD machine name even after the client's
// configured machine name (and so the mirror's LastPushMachine metadata)
// changes, stranding it under a machine filter that will never select it
// again (see readMachineStatus). The mirror's recorded LastPushMachine
// differing from the currently configured machine name must force a
// rebuild instead, so every session is re-pushed and relabeled.
func TestPushRebuildsWhenMachineNameChanges(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "machine-a", SyncOptions{}, false, nil)
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "machine-b", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Full)
	assert.Contains(t, res.Diagnostics.RebuildReason, "machine")
	assertMirrorMessageCount(t, path, "sess-1", 2)

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCountWhere(t, conn, "sessions", "machine = ?", "machine-b", 1)
	assertDuckDBCountWhere(t, conn, "sessions", "machine = ?", "machine-a", 0)
}

// TestPushDoesNotAdvanceStateOnError injects a session that fails to push
// and verifies the mirror's cutoff/last-push-at metadata are left exactly
// as they were: a partially failed incremental push must not let the
// failed session silently fall out of the next window.
func TestPushDoesNotAdvanceStateOnError(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	before, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	require.NotEmpty(t, before.LastPushCutoff)

	badID := "sess-bad"
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(badID, "alpha", "bad first", "2026-02-02T00:00:00.000Z", 1),
		Messages: []db.Message{
			syncMessage(badID, 0, "user", "bad first", "not-a-timestamp"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Errors)

	after, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, before.LastPushCutoff, after.LastPushCutoff)
	assert.Equal(t, before.LastPushAt, after.LastPushAt)
	assert.Equal(t, before.DeletionRevision, after.DeletionRevision)
	assertMirrorSessionAbsent(t, path, badID)
}

func TestSyncFullPushCreatesExpectedRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	path := filepath.Join(t.TempDir(), "full.duckdb")

	result, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCount(t, conn, "sessions", 2)
	assertDuckDBCount(t, conn, "messages", 3)
	assertDuckDBCount(t, conn, "tool_calls", 1)
	assertDuckDBCount(t, conn, "tool_result_events", 1)
	assertDuckDBCount(t, conn, "usage_events", 1)
	assertDuckDBCount(t, conn, "secret_findings", 1)
	assertDuckDBCount(t, conn, "model_pricing", 1)
	assertDuckDBCount(t, conn, "starred_sessions", 1)
	assertDuckDBCount(t, conn, "pinned_messages", 1)

	var firstMessage string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		fixture.alphaID,
	).Scan(&firstMessage))
	assert.Equal(t, "alpha first", firstMessage)
}

func TestPushSessionBatchReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	var result PushResult
	var pushed []db.Session

	err := syncer.pushSessionBatchForMode(
		ctx,
		[]db.Session{syncSession(
			"duck-canceled", "alpha", "canceled",
			"2026-01-10T00:00:00.000Z", 1,
		)},
		0, 1, &result, &pushed, nil, nil,
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

	err = syncer.pushSessionBatchForMode(
		ctx, sessions, 0, len(sessions), &result, &pushed,
		func(p PushProgress) {
			if p.SessionsDone == 1 {
				cancel()
			}
		}, nil,
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 3, result.Errors)
	assert.Empty(t, pushed)
	gotLogs := logs.String()
	assert.Equal(t, 1, strings.Count(gotLogs, "skipping session"))
	assert.Contains(t, gotLogs, "abandoning 2 sessions")
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
	path := filepath.Join(t.TempDir(), "diagnostics.duckdb")

	result, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)

	wantByAgent := map[string]int{"claude": 2, "codex": 1}
	assert.True(t, result.Diagnostics.Full)
	assert.Equal(t, 3, result.Diagnostics.LocalSessionCount)
	assert.Equal(t, 3, result.Diagnostics.CandidateSessions.Total)
	assert.Equal(t, wantByAgent, result.Diagnostics.CandidateSessions.ByAgent)
	assert.Equal(t, 0, result.Diagnostics.SkippedUnchangedSessions.Total)
	assert.Empty(t, result.Diagnostics.SkippedUnchangedSessions.ByAgent)
	assert.Equal(t, 3, result.Diagnostics.PushedSessions.Total)
	assert.Equal(t, wantByAgent, result.Diagnostics.PushedSessions.ByAgent)
	// A full/rebuild push has no incremental window, so Diagnostics.Cutoff
	// (which only pushChangedSessions populates) is left empty; the mirror's
	// own LastPushCutoff metadata is still written by writeRebuildMetadata.
	assert.Empty(t, result.Diagnostics.Cutoff)
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
	path := filepath.Join(t.TempDir(), "progress.duckdb")
	var progress []PushProgress

	result, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true,
		func(p PushProgress) {
			progress = append(progress, p)
		},
	)
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
			name: "agent label change",
			modify: func(s db.Session) db.Session {
				s.AgentLabel = "triage"
				return s
			},
		},
		{
			name: "entrypoint change",
			modify: func(s db.Session) db.Session {
				s.Entrypoint = "sdk-cli"
				return s
			},
		},
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

// TestDuckSessionFingerprintCoversEveryMirroredColumn enforces the
// invariant documented on duckSessionFingerprintFields: every session
// field upsertSession mirrors (via sessionInsertArgs) must also feed the
// fingerprint, or a change to that field would be skipped as "unchanged"
// by incremental pushes and the mirror's copy would stay stale until the
// next full rebuild. It perturbs each db.Session field by reflection: a
// perturbation that changes the insert args (the field is mirrored) must
// also change the fingerprint payload. Fields that do not change the
// insert args (sync bookkeeping like NextOrdinal, or JSON transport
// mirrors like QualitySignals) are ignored automatically, so adding a new
// mirrored column without fingerprinting it fails this test by name.
func TestDuckSessionFingerprintCoversEveryMirroredColumn(t *testing.T) {
	base := db.Session{CreatedAt: "2026-03-11T12:00:00Z"}
	encodeArgs := func(s db.Session) string {
		data, err := json.Marshal(sessionInsertArgs(s, "m", "fp"))
		require.NoError(t, err)
		return string(data)
	}
	encodeFingerprint := func(s db.Session) string {
		data, err := json.Marshal(duckSessionFingerprintFields(s, "m"))
		require.NoError(t, err)
		return string(data)
	}
	baseArgs := encodeArgs(base)
	baseFingerprint := encodeFingerprint(base)

	mirrored := 0
	typ := reflect.TypeFor[db.Session]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		perturbed, ok := perturbSessionField(base, i)
		if !ok {
			continue
		}
		if encodeArgs(perturbed) == baseArgs {
			continue
		}
		mirrored++
		assert.NotEqual(t, baseFingerprint, encodeFingerprint(perturbed),
			"db.Session.%s is mirrored by upsertSession but not covered by "+
				"duckSessionFingerprintFields; add it to the fingerprint so "+
				"incremental pushes cannot leave the mirror's copy stale",
			field.Name)
	}
	assert.GreaterOrEqual(t, mirrored, 60,
		"perturbation stopped detecting mirrored fields; fix perturbSessionField")
}

// perturbSessionField returns a copy of base with exported field i set to a
// distinct non-zero value, or ok=false for field kinds that cannot be
// perturbed generically (currently only pointers to structs, e.g. the
// QualitySignals JSON-transport mirror, which sessionInsertArgs never
// reads).
func perturbSessionField(base db.Session, i int) (db.Session, bool) {
	perturbed := base
	field := reflect.ValueOf(&perturbed).Elem().Field(i)
	target := field
	if field.Kind() == reflect.Pointer {
		if field.Type().Elem().Kind() == reflect.Struct {
			return base, false
		}
		target = reflect.New(field.Type().Elem()).Elem()
	}
	switch target.Kind() {
	case reflect.String:
		target.SetString("perturbed-" + reflect.TypeFor[db.Session]().Field(i).Name)
	case reflect.Int, reflect.Int64:
		target.SetInt(target.Int() + 101)
	case reflect.Bool:
		target.SetBool(!target.Bool())
	case reflect.Float64:
		target.SetFloat(target.Float() + 1.5)
	default:
		return base, false
	}
	if field.Kind() == reflect.Pointer {
		field.Set(target.Addr())
	}
	return perturbed, true
}

// TestPushMirrorsQualitySignalRecompute is the FINDING 2 regression: a
// quality-signal recompute (what BackfillSignals runs for every session
// after a CurrentQualitySignalVersion bump) goes through
// UpdateSessionSignals, which bumps local_modified_at so the session
// re-enters the incremental candidate window — but with the quality
// columns absent from the fingerprint, every candidate was then skipped as
// "unchanged" and DuckDB's quality analytics stayed stale indefinitely.
// The recomputed values must reach the mirror on the next incremental
// push.
func TestPushMirrorsQualitySignalRecompute(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	sess, err := local.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NoError(t, local.UpdateSessionSignals("sess-1", db.SessionSignalUpdate{
		ToolFailureSignalCount: sess.ToolFailureSignalCount,
		ToolRetryCount:         sess.ToolRetryCount,
		EditChurnCount:         sess.EditChurnCount,
		ConsecutiveFailureMax:  sess.ConsecutiveFailureMax,
		Outcome:                sess.Outcome,
		OutcomeConfidence:      sess.OutcomeConfidence,
		EndedWithRole:          sess.EndedWithRole,
		FinalFailureStreak:     sess.FinalFailureStreak,
		SignalsPendingSince:    sess.SignalsPendingSince,
		CompactionCount:        sess.CompactionCount,
		MidTaskCompactionCount: sess.MidTaskCompactionCount,
		ContextPressureMax:     sess.ContextPressureMax,
		HealthScore:            sess.HealthScore,
		HealthGrade:            sess.HealthGrade,
		HasToolCalls:           sess.HasToolCalls,
		HasContextData:         sess.HasContextData,
		QualitySignals: db.QualitySignals{
			Version:              db.CurrentQualitySignalVersion,
			ShortPromptCount:     7,
			DuplicatePromptCount: 3,
		},
	}))

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"a quality-only recompute must be re-pushed, not skipped as unchanged")

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var version, shortPrompts, duplicatePrompts int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT quality_signal_version, short_prompt_count, duplicate_prompt_count
		FROM sessions WHERE id = ?`, "sess-1",
	).Scan(&version, &shortPrompts, &duplicatePrompts))
	assert.Equal(t, db.CurrentQualitySignalVersion, version)
	assert.Equal(t, 7, shortPrompts)
	assert.Equal(t, 3, duplicatePrompts)
}

// TestSessionFingerprintsWriteColumn asserts the fingerprint-column
// contract at the point where it actually matters: the fingerprint column
// persisted in the mirror, not just the value sessionFingerprints computes.
func TestSessionFingerprintsWriteColumn(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	path := filepath.Join(t.TempDir(), "fingerprint-column.duckdb")

	_, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var fp string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT agentsview_push_fingerprint FROM sessions WHERE id = ?`,
		fixture.alphaID,
	).Scan(&fp))
	assert.Len(t, fp, 64)
	assert.False(t, strings.Contains(fp, "alpha first"))
	assert.False(t, strings.Contains(fp, "secret token sk-duckdb"))
	assert.False(t, strings.Contains(fp, "duck result"))
	assert.False(t, strings.Contains(fp, "pin alpha"))
}

func TestSyncUsesFallbackPricingWhenLocalPricingIsEmpty(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))

	require.NoError(t, syncer.syncModelPricing(ctx))

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
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('other-machine-model', 1, 2, 3, 4, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)

	require.NoError(t, syncer.syncModelPricing(ctx))

	var input, output float64
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_per_mtok, output_per_mtok
		 FROM model_pricing WHERE model_pattern = ?`,
		"other-machine-model",
	).Scan(&input, &output))
	assert.Equal(t, 1.0, input)
	assert.Equal(t, 2.0, output)
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
	require.NoError(t, createSchema(ctx, syncer.DB()))
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
	require.NoError(t, createSchema(ctx, syncer.DB()))
	rev, err := syncer.syncProjectIdentityObservations(ctx, 0, false)
	require.NoError(t, err)

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
	rev2, err := syncer.syncProjectIdentityObservations(ctx, rev, false)
	require.NoError(t, err)
	assert.Equal(t, rev, rev2, "unchanged local revision should not advance")
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = ?`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "sentinel", gotRemote,
		"unchanged local revision should skip mirror publication")

	rev3, err := syncer.syncProjectIdentityObservations(ctx, rev, true)
	require.NoError(t, err)
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = ?`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "https://github.com/acme/app.git", gotRemote,
		"forced publication should rebuild mirror identity rows")

	require.NoError(t, local.DeleteSession("snapshot-session"))
	_, err = syncer.syncProjectIdentityObservations(ctx, rev3, false)
	require.NoError(t, err)
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
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.syncProjectIdentityObservations(ctx, 0, true)
	require.NoError(t, err)

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
	require.NoError(t, createSchema(ctx, filtered.DB()))
	rev, err := filtered.syncProjectIdentityObservations(ctx, 0, false)
	require.NoError(t, err)
	_, err = filtered.DB().ExecContext(ctx, `
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
	_, err = filtered.syncProjectIdentityObservations(ctx, rev, false)
	require.NoError(t, err)
	var alphaRemoteName string
	require.NoError(t, filtered.DB().QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'alpha'`).Scan(&alphaRemoteName))
	assert.Equal(t, "sentinel", alphaRemoteName,
		"out-of-scope changes must not republish the filtered identity scope")
	require.NoError(t, filtered.Close())

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, unfiltered.DB()))
	_, err = unfiltered.syncProjectIdentityObservations(ctx, 0, false)
	require.NoError(t, err)
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
	require.NoError(t, createSchema(ctx, syncer.DB()))
	rev, err := syncer.syncProjectIdentityObservations(ctx, 0, false)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx, `
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
	rev, err = syncer.syncProjectIdentityObservations(ctx, rev, false)
	require.NoError(t, err)

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
	_, err = syncer.syncProjectIdentityObservations(ctx, rev, false)
	require.NoError(t, err)
	assertDuckDBCountWhere(t, syncer.DB(),
		"source_session_project_identity_snapshots",
		"source_session_id = ?", "identity-alpha", 0,
	)
}

func TestSyncIncrementalUpdatesPinsWithoutSessionChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	path := filepath.Join(t.TempDir(), "pins.duckdb")

	first, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "updated duck pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	second, err := Push(ctx, path, local, "test-machine", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var got string
	require.NoError(t, conn.QueryRowContext(ctx,
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
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
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

	includePath := filepath.Join(t.TempDir(), "include.duckdb")
	result, err := Push(ctx, includePath, local, "test-machine",
		SyncOptions{Projects: []string{"alpha"}}, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	includeConn, err := Open(includePath)
	require.NoError(t, err)
	defer includeConn.Close()
	assertDuckDBCount(t, includeConn, "sessions", 1)
	assertDuckDBCountWhere(t, includeConn, "sessions", "project = ?", "alpha", 1)

	excludePath := filepath.Join(t.TempDir(), "exclude.duckdb")
	result, err = Push(ctx, excludePath, local, "test-machine",
		SyncOptions{ExcludeProjects: []string{"alpha"}}, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	excludeConn, err := Open(excludePath)
	require.NoError(t, err)
	defer excludeConn.Close()
	assertDuckDBCount(t, excludeConn, "sessions", 1)
	assertDuckDBCountWhere(t, excludeConn, "sessions", "project = ?", "beta", 1)
}

func TestReadStatusFromConfigReportsTargetPushMetadataAndCounts(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "status.duckdb")

	before := time.Now().UTC()
	_, err := Push(ctx, target, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)
	after := time.Now().UTC()

	conn, err := Open(target)
	require.NoError(t, err)
	insertOtherMachineDuckSession(t, conn)
	require.NoError(t, conn.Close())

	status, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        target,
		MachineName: "test-machine",
	})
	require.NoError(t, err)
	assert.Equal(t, "test-machine", status.Machine)
	assert.Equal(t, "test-machine", status.LastPushMachine)
	assert.Equal(t, SchemaVersion, status.SchemaVersion)
	assert.Equal(t, db.CurrentDataVersion(), status.DataVersion)
	assert.Empty(t, status.Scope, "unfiltered push canonicalizes to empty scope")
	assert.Equal(t, 2, status.DuckDBSessions)
	assert.Equal(t, 3, status.DuckDBMessages)

	lastPushAt, err := time.Parse(time.RFC3339, status.LastPushAt)
	require.NoError(t, err)
	assert.WithinRange(t, lastPushAt, before.Add(-time.Second), after.Add(time.Second))
}

func TestReadStatusFromConfigReportsScopeAndDegradesOnMissingMetadata(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "status.duckdb")

	_, err := Push(ctx, target, local, "test-machine",
		SyncOptions{Projects: []string{"alpha"}}, true, nil)
	require.NoError(t, err)

	status, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        target,
		MachineName: "test-machine",
	})
	require.NoError(t, err)
	assert.Equal(t, canonicalPushScope([]string{"alpha"}, nil), status.Scope)

	conn, err := Open(target)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DELETE FROM sync_metadata`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	blankStatus, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        target,
		MachineName: "test-machine",
	})
	require.NoError(t, err, "missing metadata rows should not fail status")
	assert.Equal(t, "test-machine", blankStatus.Machine)
	assert.Empty(t, blankStatus.LastPushAt)
	assert.Empty(t, blankStatus.LastPushMachine)
	assert.Zero(t, blankStatus.SchemaVersion)
	assert.Zero(t, blankStatus.DataVersion)
	assert.Empty(t, blankStatus.Scope)
	assert.Equal(t, 1, blankStatus.DuckDBSessions,
		"row counts still read even with metadata gone")
}

// TestReadStatusFromConfigDoesNotCreateMissingMirror is the FINDING 2
// regression: status used to open a missing local mirror path through
// NewStoreFromConfig, whose read-write open CREATES the database file. The
// resulting empty file lacks the agentsview sentinel, so the next push
// refused to replace it and mirror initialization stayed blocked until the
// file was removed by hand. Status against a missing local path must
// report MirrorMissing without creating anything.
func TestReadStatusFromConfigDoesNotCreateMissingMirror(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing.duckdb")

	status, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        path,
		MachineName: "test-machine",
	})

	require.NoError(t, err)
	assert.True(t, status.MirrorMissing)
	assert.Equal(t, "test-machine", status.Machine)
	assert.Empty(t, status.LastPushAt)
	assert.Zero(t, status.DuckDBSessions)
	assert.NoFileExists(t, path,
		"status must never create the mirror file")
}

// TestReadStatusFromConfigCountsByTargetMachineNotConfiguredMachine is the
// FIX2 regression: readMachineStatus previously filtered its row counts by
// the CLIENT's configured machine name, while the LastPushMachine it
// displays comes from the target's own metadata. A remote Quack client is
// normally configured under its own hostname, which almost never matches
// whatever machine actually pushed the mirror it is reading, so filtering
// counts by the configured name reports zero rows even though the mirror
// plainly has data and the display line already shows a real
// LastPushMachine. Counts must be keyed off the target's recorded
// LastPushMachine when it is set.
func TestReadStatusFromConfigCountsByTargetMachineNotConfiguredMachine(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "status.duckdb")

	_, err := Push(ctx, target, local, "actual-pusher", SyncOptions{}, true, nil)
	require.NoError(t, err)

	status, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		Path:        target,
		MachineName: "remote-client-hostname",
	})
	require.NoError(t, err)
	assert.Equal(t, "remote-client-hostname", status.Machine)
	assert.Equal(t, "actual-pusher", status.LastPushMachine)
	assert.Equal(t, 2, status.DuckDBSessions,
		"counts must key off the target's LastPushMachine, not the client's configured name")
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
	t.Cleanup(func() {
		require.NoError(t, syncer.Close())
	})
	return syncer
}

func newInMemoryTestSync(t *testing.T, local *db.DB, opts SyncOptions) *Sync {
	t.Helper()
	return newTestSync(t, ":memory:", local, opts)
}

// newPushFixture seeds n local sessions (sess-1..sess-N), each with two
// messages, and returns the local db plus a fresh temp-file mirror path
// that has not been pushed yet.
func newPushFixture(t *testing.T, n int) (*db.DB, string) {
	t.Helper()
	local := newLocalDB(t)
	writes := make([]db.SessionBatchWrite, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("sess-%d", i)
		ts := fmt.Sprintf("2026-02-01T00:%02d:00.000Z", i%60)
		writes = append(writes, db.SessionBatchWrite{
			Session: syncSession(id, "alpha", "first "+id, ts, 2),
			Messages: []db.Message{
				syncMessage(id, 0, "user", "first "+id, ts),
				syncMessage(id, 1, "assistant", "reply "+id, ts),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	return local, path
}

// appendMessage appends one more message to sessionID through the normal
// local write path, bumping local_modified_at (and so sync_marker) so the
// next Push selects it as a candidate.
func appendMessage(t *testing.T, local *db.DB, sessionID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	ordinal := len(msgs)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	sess.LocalModifiedAt = &modifiedAt
	sess.MessageCount = ordinal + 1
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: *sess,
		Messages: []db.Message{
			syncMessage(sessionID, ordinal, "assistant", "appended", modifiedAt),
		},
	}})
	require.NoError(t, err)
}

// mutateSessionContent changes a session's first message content via a raw
// UPDATE that never touches the sessions table, so sync_marker is left
// exactly as it was: the fingerprint changes without the marker moving.
func mutateSessionContent(t *testing.T, local *db.DB, sessionID string) {
	t.Helper()
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE messages SET content = 'mutated content'
			 WHERE session_id = ? AND ordinal = 0`,
			sessionID,
		)
		return err
	}))
}

// setSessionSignalsTo forces sessionID's sync_marker directly, bypassing
// the triggers that normally derive it from the session's signal columns,
// so a test can pin it exactly at a mirror's stored cutoff.
func setSessionSignalsTo(t *testing.T, local *db.DB, sessionID, marker string) {
	t.Helper()
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE sessions SET sync_marker = ? WHERE id = ?`, marker, sessionID,
		)
		return err
	}))
}

// setMirrorMetadataValue writes directly into a closed mirror file's
// sync_metadata table, for tests that need to tamper with probe-visible
// state between two Push calls.
func setMirrorMetadataValue(t *testing.T, path, key, value string) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`
		INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	require.NoError(t, err)
}

func assertMirrorMessageCount(t *testing.T, path, sessionID string, want int) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCountWhere(t, conn, "messages", "session_id = ?", sessionID, want)
}

func assertMirrorSessionAbsent(t *testing.T, path, sessionID string) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCountWhere(t, conn, "sessions", "id = ?", sessionID, 0)
}

// TestDuckGetAnalyticsSkillsIgnoresCrossSessionDuplicateIDs guards the
// skill join: DuckDB mirrors SQLite row IDs from many machines, so
// messages.id is not globally unique. A tool call must join only to a
// message in its own session, not another session's row with the same id.
func TestDuckGetAnalyticsSkillsIgnoresCrossSessionDuplicateIDs(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
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
	path := filepath.Join(t.TempDir(), "duration.duckdb")

	result, err := Push(ctx, path, local, "test-machine", SyncOptions{}, true, nil)
	require.NoError(t, err)
	assert.Greater(t, result.Duration, time.Duration(0))
}
