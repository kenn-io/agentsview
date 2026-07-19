//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushWorkBoundedByChangedBatchNotArchiveSize is the AGENTS.md
// cardinality-scaling regression for incremental Push: the candidate set an
// incremental push selects, and the sessions it actually pushes or
// tombstones, must track the changed batch (one appended session, one
// hard-deleted session) rather than growing with total archive size.
func TestPushWorkBoundedByChangedBatchNotArchiveSize(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int{20, 400} {
		t.Run(fmt.Sprintf("archive_%d", size), func(t *testing.T) {
			local, path := newPushFixture(t, size)
			_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)

			appendMessage(t, local, "sess-7")
			require.NoError(t, local.DeleteSession("sess-3"))

			res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)
			assert.False(t, res.Diagnostics.Full)
			// Work is bounded by the changed batch regardless of archive size:
			assert.LessOrEqual(t, res.Diagnostics.CandidateSessions.Total, 3)
			assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)
			assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions)
			// And an untouched follow-up push does nothing:
			res2, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)
			assert.Zero(t, res2.Diagnostics.PushedSessions.Total)
			assert.Zero(t, res2.Diagnostics.DeletedStaleSessions)
			assert.LessOrEqual(t, res2.Diagnostics.CandidateSessions.Total, 2)
		})
	}
}

// TestRebuildUnderReaderNeverErrorsAndEventuallyServesRebuiltData drives a
// live Store (with WatchMirrorReplacement polling) through a concurrent
// --full rebuild: a background reader keeps querying the store's current
// handle while Push(..., full=true) builds a fresh mirror file into a temp
// path and atomically renames it over the one the store has open. The
// contract under test is the live-reader path production `serve` uses: the
// rebuild must never error, concurrent reads against the store must never
// see an error during the swap, and the store must eventually pick up and
// serve the rebuilt content. (Windows behavior when the destination handle
// blocks the rename is covered separately by TestSwapMirrorFileRetriesThenFailsWithActionableError
// in rebuild_test.go; POSIX rename is atomic, which is what this test relies
// on to avoid ever observing a torn file.)
func TestRebuildUnderReaderNeverErrorsAndEventuallyServesRebuiltData(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := context.Background()
	local, path := newPushFixture(t, 3)
	_, err := rebuildMirror(ctx, path, local, "m", SyncOptions{}, nil)
	require.NoError(t, err)

	store, err := NewStore(path)
	require.NoError(t, err)
	defer store.Close()

	watchCtx := t.Context()
	store.WatchMirrorReplacement(watchCtx, 10*time.Millisecond, nil)

	readerCtx, cancelReader := context.WithCancel(context.Background())
	var readerErrs atomic.Int32
	var wg sync.WaitGroup
	wg.Go(func() {
		for readerCtx.Err() == nil {
			if _, err := store.GetStats(ctx, false, false); err != nil {
				readerErrs.Add(1)
			}
		}
	})

	require.NoError(t, local.DeleteSession("sess-2"))
	appendMessage(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, true, nil)
	require.NoError(t, err, "a rebuild racing a live reader must never error")
	assert.True(t, res.Diagnostics.Full)
	assert.Zero(t, res.Errors)

	// The reader goroutine keeps running through the watcher's poll-and-adopt
	// window (rather than stopping right after Push returns), so it actually
	// exercises swapHandle concurrently with reads instead of merely racing
	// the rename itself.
	require.Eventually(t, func() bool {
		ids := listMirrorSessionIDs(t, store)
		if len(ids) != 2 {
			return false
		}
		return !slices.Contains(ids, "sess-2")
	}, 5*time.Second, 100*time.Millisecond, "store must eventually serve the rebuilt mirror")

	cancelReader()
	wg.Wait()
	assert.Zero(t, readerErrs.Load(),
		"concurrent reads against the live store must never error during a rebuild swap")

	assert.Equal(t, 3, mirrorMessageCountViaStore(t, store, "sess-1"),
		"store must eventually serve sess-1's appended message")
}

func mirrorMessageCountViaStore(t *testing.T, store *Store, sessionID string) int {
	t.Helper()
	var n int
	require.NoError(t, store.queryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID,
	).Scan(&n))
	return n
}

// TestFilteredIncrementalPushScopesCandidatesPushesAndDeletes is the
// project-filter counterpart to TestPushWorkBoundedByChangedBatchNotArchiveSize:
// with a project filter configured, an incremental push must push and
// tombstone only in-scope sessions. An out-of-scope session was never
// mirrored by the initial filtered full push, so mutating or hard-deleting
// it locally must leave the in-scope diagnostics counts and the mirror
// contents untouched (the window listing does see out-of-scope changes,
// but only to reconcile mirror-resident rows — a never-mirrored session is
// skipped without counting anywhere).
func TestFilteredIncrementalPushScopesCandidatesPushesAndDeletes(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession("sess-in-1", "alpha", "in first", "2026-03-01T00:00:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("sess-in-1", 0, "user", "in first", "2026-03-01T00:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("sess-in-2", "alpha", "in second", "2026-03-01T00:01:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("sess-in-2", 0, "user", "in second", "2026-03-01T00:01:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("sess-out-1", "beta", "out first", "2026-03-01T00:02:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("sess-out-1", 0, "user", "out first", "2026-03-01T00:02:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("sess-out-2", "beta", "out second", "2026-03-01T00:03:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("sess-out-2", 0, "user", "out second", "2026-03-01T00:03:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)

	opts := SyncOptions{Projects: []string{"alpha"}}
	path := filepath.Join(t.TempDir(), "filtered.duckdb")
	_, err = Push(ctx, path, local, "m", opts, true, nil)
	require.NoError(t, err)
	// Every mirror connection here is opened and closed immediately, never
	// held open across a later Push call: DuckDB does not tolerate a second
	// independent connection on the same literal path string while one is
	// already open (see the openMirrorAlias comment in mirror_watch.go), and
	// a lingering connection here was observed to make the following Push
	// read back a stale probe and force an unwanted rebuild.
	assertMirrorSessionAbsent(t, path, "sess-out-1")

	appendMessage(t, local, "sess-in-1")
	appendMessage(t, local, "sess-out-1")
	require.NoError(t, local.DeleteSession("sess-in-2"))
	require.NoError(t, local.DeleteSession("sess-out-2"))

	res, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.LessOrEqual(t, res.Diagnostics.CandidateSessions.Total, 2,
		"the out-of-scope mutation must not count as an in-scope candidate")
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"only the in-scope mutated session is pushed")
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions,
		"only the in-scope, mirror-resident hard delete counts as removed")

	assertMirrorMessageCount(t, path, "sess-in-1", 2)
	assertMirrorSessionAbsent(t, path, "sess-in-2")
	assertMirrorSessionAbsent(t, path, "sess-out-1")
	assertMirrorSessionAbsent(t, path, "sess-out-2")
}

// TestProjectTransitionRemovesSessionFromFilteredMirror covers the
// scope-transition gap: a session pushed into a filtered mirror while its
// project was in scope, whose project then changes to an out-of-scope one
// (a real transition also bumps a sync signal, here local_modified_at, the
// way a session rewrite does), must be REMOVED from the mirror by the next
// incremental push. A scope-filtered candidate listing would never select
// it again, leaving the stale row behind until a full rebuild.
func TestProjectTransitionRemovesSessionFromFilteredMirror(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		opts      SyncOptions
		toProject string
	}{
		{
			name:      "moves into an excluded project",
			opts:      SyncOptions{ExcludeProjects: []string{"scratch"}},
			toProject: "scratch",
		},
		{
			name:      "moves off the include allowlist",
			opts:      SyncOptions{Projects: []string{"alpha"}},
			toProject: "gamma",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, path := newPushFixture(t, 2)
			_, err := Push(ctx, path, local, "m", tt.opts, false, nil)
			require.NoError(t, err)
			assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "sess-1", 1)

			moveSessionToProject(t, local, "sess-1", tt.toProject)

			res, err := Push(ctx, path, local, "m", tt.opts, false, nil)
			require.NoError(t, err)
			assert.False(t, res.Diagnostics.Full)
			assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions,
				"the out-of-scope-moved resident session must count as removed")
			assert.Zero(t, res.Diagnostics.PushedSessions.Total)
			assertMirrorSessionAbsent(t, path, "sess-1")
			assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "sess-2", 1)
			assertMirrorTableCountWhere(t, path, "messages", "session_id = ?", "sess-1", 0)

			// A follow-up push has nothing left to reconcile.
			res2, err := Push(ctx, path, local, "m", tt.opts, false, nil)
			require.NoError(t, err)
			assert.Zero(t, res2.Diagnostics.DeletedStaleSessions)
		})
	}
}

// TestProjectTransitionThenHardDeleteAppliesTombstone is the deletion-journal
// side of the scope-transition gap: a session that moved out of the push
// scope and was THEN hard-deleted journals a tombstone under its new
// (out-of-scope) project. A scope-filtered tombstone load would skip it and
// strand the mirror row forever; the unfiltered load must apply it.
func TestProjectTransitionThenHardDeleteAppliesTombstone(t *testing.T) {
	ctx := context.Background()
	opts := SyncOptions{ExcludeProjects: []string{"scratch"}}
	local, path := newPushFixture(t, 2)
	_, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "sess-1", 1)

	moveSessionToProject(t, local, "sess-1", "scratch")
	require.NoError(t, local.DeleteSession("sess-1"))

	res, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions,
		"the out-of-scope tombstone must still remove the resident mirror row")
	assertMirrorSessionAbsent(t, path, "sess-1")
	assertMirrorTableCountWhere(t, path, "sessions", "id = ?", "sess-2", 1)
}

// moveSessionToProject reassigns a session's project the way a real
// transition lands: alongside a bumped local_modified_at (which advances
// sync_marker via the trigger), since a project change always comes from a
// session rewrite rather than an isolated column edit.
func moveSessionToProject(t *testing.T, local *db.DB, sessionID, project string) {
	t.Helper()
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE sessions SET project = ?, local_modified_at = ? WHERE id = ?`,
			project, modifiedAt, sessionID,
		)
		return err
	}))
}

// TestReplaceCurationBoundedByLocalCurationSizeNotMirrorSize is the
// cardinality-scaling regression for replaceCuration (push.go): before
// this change, every refresh listed every mirror session id and validated
// the whole set, so the work scaled with total mirror size regardless of
// how many sessions were actually starred or pinned. replaceCuration now
// loads the curation-sized local starred/pinned session ID lists, validates
// only those against the mirror (mirrorResidentSessionIDs, batched), and
// loads pins with one batched local query (db.PinnedMessagesBySession,
// chunked at 900 ids).
//
// What this test can observe from a black-box *testing.T: curation ends up
// byte-correct (exactly the local starred/pinned rows, and only those) at
// both a small and a twenty-times-larger archive size, across both a push
// that adds curation and one that removes it. What it cannot observe here:
// the actual local SQL round-trip count or per-push latency — asserting
// those would require either instrumenting db.DB's driver or a timing-based
// assertion, and timing assertions on shared CI runners are exactly the kind
// of flake AGENTS.md's CI guidance warns against. The boundedness claim
// itself rests on the code path (curation-sized queries plus a
// curation-sized membership probe), not on a measurement in this test.
func TestReplaceCurationBoundedByLocalCurationSizeNotMirrorSize(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int{20, 400} {
		t.Run(fmt.Sprintf("archive_%d", size), func(t *testing.T) {
			local, path := newPushFixture(t, size)
			ok, err := local.StarSession("sess-1")
			require.NoError(t, err)
			require.True(t, ok)
			msgs, err := local.GetAllMessages(ctx, "sess-2")
			require.NoError(t, err)
			require.NotEmpty(t, msgs)
			note := "curation scale note"
			_, err = local.PinMessage("sess-2", msgs[0].ID, &note)
			require.NoError(t, err)

			_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)

			// Every mirror connection below is opened and closed immediately
			// (assertMirrorTableCount*), never held open across a later Push
			// call — see the comment in
			// TestFilteredIncrementalPushScopesCandidatesPushesAndDeletes for
			// why a lingering connection corrupts the next Push's probe read.
			assertMirrorTableCount(t, path, "starred_sessions", 1)
			assertMirrorTableCount(t, path, "pinned_messages", 1)
			assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", "sess-1", 1)
			assertMirrorTableCountWhere(t, path, "pinned_messages", "session_id = ?", "sess-2", 1)

			require.NoError(t, local.UnstarSession("sess-1"))
			require.NoError(t, local.UnpinMessage("sess-2", msgs[0].ID))
			// A mutating incremental push (a required precondition for
			// replaceCuration to be worth asserting on): appending a message
			// bumps sess-3's sync_marker so the push actually does mutating
			// work and reruns curation refresh alongside it.
			appendMessage(t, local, "sess-3")

			_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
			require.NoError(t, err)
			assertMirrorTableCount(t, path, "starred_sessions", 0)
			assertMirrorTableCount(t, path, "pinned_messages", 0)
		})
	}
}

// TestCurationRefreshSkipsWhenLocalCurationStateUnchanged is the FIX3
// contract: an incremental push whose local in-scope curation state
// (starred ids, pinned message id/note pairs) has not changed since the
// last refresh skips replaceCuration's O(mirror) delete+reinsert entirely
// (Diagnostics.CurationRefreshed false), while a push that follows a real
// curation change still refreshes and propagates it (CurationRefreshed
// true) exactly as before this change.
func TestCurationRefreshSkipsWhenLocalCurationStateUnchanged(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 2)
	ok, err := local.StarSession("sess-1")
	require.NoError(t, err)
	require.True(t, ok)

	first, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, first.Diagnostics.Full, "initial push is a rebuild")
	assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", "sess-1", 1)

	// A mutating incremental push with no curation change: the star
	// propagated during the rebuild above, so nothing about curation state
	// changed. appendMessage forces this to be a real (non-no-op)
	// incremental push so the curation-refresh step actually runs its
	// unchanged-fingerprint check.
	appendMessage(t, local, "sess-2")
	second, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, second.Diagnostics.Full)
	assert.False(t, second.Diagnostics.CurationRefreshed,
		"unchanged local curation state must skip the mirror-side refresh")
	assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", "sess-1", 1)

	// Now star sess-2 too: a real curation change with no other session
	// mutation. The next push must detect it and refresh.
	ok, err = local.StarSession("sess-2")
	require.NoError(t, err)
	require.True(t, ok)
	third, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, third.Diagnostics.Full)
	assert.True(t, third.Diagnostics.CurationRefreshed,
		"a real curation change must trigger the mirror-side refresh")
	assertMirrorTableCountWhere(t, path, "starred_sessions", "session_id = ?", "sess-2", 1)

	// And a further no-op push (curation unchanged again) skips once more.
	fourth, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, fourth.Diagnostics.CurationRefreshed)
}

// TestCurationSkipsSessionsAbsentFromMirror pins replaceCuration's
// membership rule: stars and pins are only mirrored for sessions that
// actually exist in the mirror. A star/pin on an out-of-scope session (or
// any session the mirror does not hold) must be skipped, not inserted as a
// dangling curation row.
func TestCurationSkipsSessionsAbsentFromMirror(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local) // alpha + beta project sessions
	path := filepath.Join(t.TempDir(), "curation-scope.duckdb")
	opts := SyncOptions{Projects: []string{"alpha"}}

	_, err := Push(ctx, path, local, "test-machine", opts, false, nil)
	require.NoError(t, err)

	// Star the out-of-scope beta session and pin one of its messages; the
	// filtered mirror does not contain it.
	ok, err := local.StarSession("duck-sync-beta")
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(ctx, "duck-sync-beta")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	_, err = local.PinMessage("duck-sync-beta", msgs[0].ID, nil)
	require.NoError(t, err)

	res, err := Push(ctx, path, local, "test-machine", opts, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)

	assertMirrorTableCountWhere(t, path,
		"starred_sessions", "session_id = ?", "duck-sync-beta", 0)
	assertMirrorTableCountWhere(t, path,
		"pinned_messages", "session_id = ?", "duck-sync-beta", 0)
	// The in-scope alpha curation from the fixture is still mirrored.
	assertMirrorTableCountWhere(t, path,
		"starred_sessions", "session_id = ?", "duck-sync-alpha", 1)
	assertMirrorTableCountWhere(t, path,
		"pinned_messages", "session_id = ?", "duck-sync-alpha", 1)
}

// TestReplaceCurationSkipsStarForSessionAbsentFromMirror drives
// replaceCuration directly against a mirror that holds only one of two
// in-scope local sessions: curation rows for the unmirrored session must
// be skipped by the mirror-membership check even though its star and pin
// are fully in scope locally.
func TestReplaceCurationSkipsStarForSessionAbsentFromMirror(t *testing.T) {
	ctx := context.Background()
	local, _ := newPushFixture(t, 2)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))

	// Mirror only sess-1; sess-2 stays local-only.
	sessions, err := local.ListSessionsForMirrorWindow(ctx, "", "", nil, nil)
	require.NoError(t, err)
	fingerprints, err := syncer.sessionFingerprints(ctx, sessions)
	require.NoError(t, err)
	for _, sess := range sessions {
		if sess.ID != "sess-1" {
			continue
		}
		_, err := syncer.pushSingleSession(ctx, sess, fingerprints[sess.ID])
		require.NoError(t, err)
	}

	for _, id := range []string{"sess-1", "sess-2"} {
		ok, err := local.StarSession(id)
		require.NoError(t, err)
		require.True(t, ok)
	}
	msgs, err := local.GetAllMessages(ctx, "sess-2")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	_, err = local.PinMessage("sess-2", msgs[0].ID, nil)
	require.NoError(t, err)

	require.NoError(t, syncer.replaceCuration(ctx))

	assertDuckDBCountWhere(t, syncer.DB(),
		"starred_sessions", "session_id = ?", "sess-1", 1)
	assertDuckDBCountWhere(t, syncer.DB(),
		"starred_sessions", "session_id = ?", "sess-2", 0)
	assertDuckDBCountWhere(t, syncer.DB(),
		"pinned_messages", "session_id = ?", "sess-2", 0)
}

// TestCurationFingerprintDetectsNoteOnlyEdit guards the specific gap a
// pinned-message-id-only fingerprint would miss: PinMessage on an
// already-pinned message updates its note in place without changing the
// pinned message id set or created_at, so the fingerprint must incorporate
// note content, not just membership.
func TestCurationFingerprintDetectsNoteOnlyEdit(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	msgs, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	firstNote := "first note"
	_, err = local.PinMessage("sess-1", msgs[0].ID, &firstNote)
	require.NoError(t, err)

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	secondNote := "second note"
	_, err = local.PinMessage("sess-1", msgs[0].ID, &secondNote)
	require.NoError(t, err)
	appendMessage(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.CurationRefreshed,
		"a note-only pin edit must still be detected as a curation change")

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var got string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		"sess-1", msgs[0].ID,
	).Scan(&got))
	assert.Equal(t, secondNote, got)
}

// TestCurationFingerprintDetectsUnpinRepinWithSameNote is the FIX6
// regression: a curation fingerprint built only from (message_id, note) is
// unchanged by an unpin followed by a repin of the same message with the
// identical note, so it would treat the cycle as a no-op and skip the
// mirror refresh even though it is a genuine curation event (a distinct pin
// row, with a new id and created_at). A second, untouched pin (sess-2) is
// kept present throughout so pinned_messages is never fully empty locally,
// avoiding SQLite's empty-table rowid reuse for a plain INTEGER PRIMARY KEY
// (see the analogous comment in internal/db/pins_test.go).
func TestCurationFingerprintDetectsUnpinRepinWithSameNote(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 2)
	msgs1, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.NotEmpty(t, msgs1)
	msgs2, err := local.GetAllMessages(ctx, "sess-2")
	require.NoError(t, err)
	require.NotEmpty(t, msgs2)

	note := "same note"
	_, err = local.PinMessage("sess-1", msgs1[0].ID, &note)
	require.NoError(t, err)
	_, err = local.PinMessage("sess-2", msgs2[0].ID, &note)
	require.NoError(t, err)

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	require.NoError(t, local.UnpinMessage("sess-1", msgs1[0].ID))
	_, err = local.PinMessage("sess-1", msgs1[0].ID, &note)
	require.NoError(t, err)
	appendMessage(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.CurationRefreshed,
		"an unpin+repin with the same note must still be detected as a curation change")
}

// assertMirrorTableCount opens path, asserts table's total row count, and
// closes the connection before returning — see the comment in
// TestFilteredIncrementalPushScopesCandidatesPushesAndDeletes for why a
// mirror connection must never be held open across a later Push call.
func assertMirrorTableCount(t *testing.T, path, table string, want int) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCount(t, conn, table, want)
}

// assertMirrorTableCountWhere is assertMirrorTableCount with a WHERE clause;
// see assertMirrorTableCount for the open/assert/close-immediately contract.
func assertMirrorTableCountWhere(
	t *testing.T, path, table, where string, arg any, want int,
) {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	assertDuckDBCountWhere(t, conn, table, where, arg, want)
}
