//go:build !(windows && arm64)

package duckdb

import (
	"context"
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
// with a project filter configured, an incremental push must select, push,
// and tombstone only in-scope sessions. An out-of-scope session was never
// mirrored by the initial filtered full push, so mutating or hard-deleting
// it locally must be invisible to both the diagnostics and the mirror
// contents of a later incremental push.
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
		"the out-of-scope mutation must never enter the candidate window")
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"only the in-scope mutated session is pushed")
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions,
		"only the in-scope hard delete is tombstoned")

	assertMirrorMessageCount(t, path, "sess-in-1", 2)
	assertMirrorSessionAbsent(t, path, "sess-in-2")
	assertMirrorSessionAbsent(t, path, "sess-out-1")
	assertMirrorSessionAbsent(t, path, "sess-out-2")
}

// TestReplaceCurationBoundedByLocalCurationSizeNotMirrorSize is the
// cardinality-scaling regression for replaceCuration/replaceAllPinnedMessages
// (push.go): before this change, every incremental push refreshed curation
// by listing every mirror session id and then issuing one local
// ListPinnedMessages query per id, so the local SQLite round-trip count
// scaled with total mirror size regardless of how many sessions were
// actually starred or pinned. replaceAllPinnedMessages now loads pins with
// one batched local query (db.PinnedMessagesBySession, chunked at 900 ids)
// keyed off the same mirror-resident id list.
//
// What this test can observe from a black-box *testing.T: curation ends up
// byte-correct (exactly the local starred/pinned rows, and only those) at
// both a small and a twenty-times-larger archive size, across both a push
// that adds curation and one that removes it. What it cannot observe here:
// the actual local SQL round-trip count or per-push latency — asserting
// those would require either instrumenting db.DB's driver or a timing-based
// assertion, and timing assertions on shared CI runners are exactly the kind
// of flake AGENTS.md's CI guidance warns against. The boundedness claim
// itself rests on the code path (replaceAllPinnedMessages calling
// PinnedMessagesBySession once instead of ListPinnedMessages per session),
// not on a measurement in this test.
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
