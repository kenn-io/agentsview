package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedExtractSession(t *testing.T, d *DB, id string) {
	t.Helper()
	require.NoError(t, d.UpsertSession(Session{
		ID:      id,
		Project: "proj",
		Machine: defaultMachine,
		Agent:   defaultAgent,
	}))
}

func TestExtractGenerationEnsureIsIdempotent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	first, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a",
		Model:       "model-x",
		Segmenter:   "turns-v1",
		ParamsJSON:  `{"max_window_chars":50000}`,
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractGenerationBuilding, first.State)

	again, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a",
		Model:       "model-y",
		Segmenter:   "other",
	})
	require.NoError(t, err)
	assert.Equal(t, "model-x", again.Model, "existing row wins")
	assert.Equal(t, "turns-v1", again.Segmenter)

	generations, err := d.ExtractGenerations(ctx)
	require.NoError(t, err)
	require.Len(t, generations, 1)
}

func TestExtractGenerationActivateKeepsSingleActive(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedCoveredExtractSession(t, d, "sess-1")
	for _, fp := range []string{"fp-a", "fp-b"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
		seedServableExtractEntry(t, d, fp, "sess-1", "e-"+fp)
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: "sess-1", Fingerprint: fp,
			ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
		})
		require.NoError(t, err)
	}
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-b", []string{"rules-v1"}, time.Now()))

	generations, err := d.ExtractGenerations(ctx)
	require.NoError(t, err)
	states := map[string]string{}
	for _, gen := range generations {
		states[gen.Fingerprint] = gen.State
	}
	assert.Equal(t, ExtractGenerationRetired, states["fp-a"])
	assert.Equal(t, ExtractGenerationActive, states["fp-b"])

	err = d.ActivateExtractGeneration(
		ctx, "fp-missing", []string{"rules-v1"}, time.Now())
	assert.Error(t, err, "unknown fingerprint must refuse")
}

func TestExtractGenerationRetireActiveRequiresForce(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-1", "fp-a")
	seedServableExtractEntry(t, d, "fp-a", "sess-1", "e-a")
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))

	err = d.RetireExtractGeneration(ctx, "fp-a", false)
	require.Error(t, err, "retiring the active generation needs force")

	require.NoError(t, d.RetireExtractGeneration(ctx, "fp-a", true))
	generations, err := d.ExtractGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, ExtractGenerationRetired, generations[0].State)
}

func TestExtractProgressLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	progress, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPending, progress.State)
	assert.Equal(t, 0, progress.UnitCursor)
	assert.Equal(t, 4, progress.UnitsTotal)

	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))
	progress, ok, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ExtractProgressPartial, progress.State)
	assert.Equal(t, 2, progress.UnitCursor)

	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 4))
	progress, _, err = d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State)
}

func TestExtractProgressUpsertResetsOnDigestChange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 4))

	same, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, same.State, "same digest keeps progress")
	assert.Equal(t, 4, same.UnitCursor)

	grown, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-2", UnitsTotal: 6, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPending, grown.State, "digest change resets")
	assert.Equal(t, 0, grown.UnitCursor)
	assert.Equal(t, 6, grown.UnitsTotal)
	assert.Equal(t, "digest-2", grown.ContentDigest)
}

func TestExtractProgressFailureKeepsCursor(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))

	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 2,
		LastError:      "endpoint unreachable",
	}))
	progress, ok, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ExtractProgressFailed, progress.State)
	assert.Equal(t, 2, progress.UnitCursor, "failure keeps the resume point")
	assert.Equal(t, "endpoint unreachable", progress.LastError)
}

func TestExtractProgressUnknownSessionRefused(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-missing", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	assert.Error(t, err, "progress rows require an existing session")
}

func TestAdvanceExtractCursorRejectsStaleDigest(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-2", UnitsTotal: 6, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	err = d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 3)
	require.ErrorIs(t, err, ErrStaleExtractProgress,
		"a worker holding the old digest must not overwrite reset progress")

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, 0, progress.UnitCursor, "stale advance must not move the cursor")
	assert.Equal(t, ExtractProgressPending, progress.State)
}

func TestAdvanceExtractCursorIsMonotonicAndBounded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 3))

	err = d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2)
	require.ErrorIs(t, err, ErrStaleExtractProgress, "cursor must not regress")

	err = d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 5)
	require.Error(t, err, "cursor past units_total must be refused")

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, 3, progress.UnitCursor)
}

func TestMarkExtractProgressFailedRejectsStaleDigest(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-2", UnitsTotal: 6, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	err = d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		LastError:      "boom",
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress)

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPending, progress.State,
		"stale failure must not clobber reset progress")
}

func TestCopyRecallEntriesFromCarriesExtractState(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := Open(srcPath)
	require.NoError(t, err)
	ctx := context.Background()
	seedExtractSession(t, src, "sess-gone")
	_, err = src.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
		ParamsJSON: `{"max_window_chars":50000}`,
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, src, "sess-1", "fp-a")
	seedServableExtractEntry(t, src, "fp-a", "sess-1", "e-src")
	require.NoError(t, src.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	_, err = src.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, src.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))
	_, err = src.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-gone", Fingerprint: "fp-a",
		ContentDigest: "digest-9", UnitsTotal: 3, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	src.Close()

	dst := testDB(t)
	seedExtractSession(t, dst, "sess-1") // sess-gone not re-synced

	require.NoError(t, dst.CopyRecallEntriesFrom(srcPath))

	generations, err := dst.ExtractGenerations(ctx)
	require.NoError(t, err)
	require.Len(t, generations, 1, "resync must carry the generation registry")
	assert.Equal(t, ExtractGenerationActive, generations[0].State)
	assert.Equal(t, `{"max_window_chars":50000}`, generations[0].ParamsJSON)

	progress, ok, err := dst.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, ok, "resync must carry resume cursors")
	assert.Equal(t, 2, progress.UnitCursor)
	assert.Equal(t, ExtractProgressPartial, progress.State)

	_, ok, err = dst.ExtractProgress(ctx, "sess-gone", "fp-a")
	require.NoError(t, err)
	assert.False(t, ok, "progress for sessions absent from the new DB is dropped")
}

func TestCopyRecallEntriesFromToleratesSourceWithoutExtractTables(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := Open(srcPath)
	require.NoError(t, err)
	src.Close()
	conn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err)
	_, err = conn.Exec("DROP TABLE recall_extract_progress")
	require.NoError(t, err)
	_, err = conn.Exec("DROP TABLE recall_extract_generations")
	require.NoError(t, err)
	conn.Close()

	dst := testDB(t)
	require.NoError(t, dst.CopyRecallEntriesFrom(srcPath),
		"archives from releases without extraction tables must still resync")
}

func TestMarkExtractProgressFailedRejectsDoneRow(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))

	err = d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 2,
		LastError:      "late worker",
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress,
		"a completed row must not be demoted to failed")

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State)
	assert.Equal(t, 2, progress.UnitCursor)
	assert.Empty(t, progress.LastError)
}

func TestMarkExtractProgressFailedReopensDoneOnRequest(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))

	// The optimistic guards still apply to a reopen: a stale cursor means
	// another writer moved the row, whose view wins.
	err = d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 1,
		LastError:      "count mismatch",
		Reopen:         true,
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress)

	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 2,
		LastError:      "count mismatch",
		Reopen:         true,
	}))
	progress, found, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ExtractProgressFailed, progress.State)
	assert.Zero(t, progress.UnitCursor,
		"a reopened row restarts from zero: its completed-units claim was "+
			"judged against an inconsistent session, and the strictly "+
			"monotonic cursor could otherwise never reach done again")
	assert.Equal(t, "count mismatch", progress.LastError)
}

func TestMarkExtractProgressFailedReopenRestartsPartialRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 1))

	// Reopen restarts non-done rows too: callers use it after discarding
	// the session's generated entries, so a preserved cursor would skip
	// re-extracting units whose entries no longer exist.
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 1,
		LastError:      "session became ineligible during extraction",
		Reopen:         true,
	}))
	progress, found, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ExtractProgressFailed, progress.State)
	assert.Zero(t, progress.UnitCursor)
}

func TestUpsertExtractProgressPreservesFailedBackoff(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 0, LastError: "boom",
	}))
	failed, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)

	// A retry begins by re-upserting the same digest. If that refreshed
	// updated_at, a retry cancelled before finishing would restart the
	// whole failure backoff instead of staying due.
	time.Sleep(3 * time.Millisecond)
	after, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressFailed, after.State)
	assert.Equal(t, failed.UpdatedAt, after.UpdatedAt,
		"a same-digest upsert on a failed row must not reset the backoff "+
			"clock")

	// A digest change is genuinely new work: it resets to pending and
	// takes a fresh clock.
	reset, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-2", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPending, reset.State)
}

func TestUpsertExtractProgressCompletesZeroUnitRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	first, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-empty", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, ExtractProgressDone, first.State)

	// A reopened zero-unit row must converge back to done on the next
	// stable upsert: the extraction loop runs zero iterations for it, so
	// no cursor advance will ever promote it.
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ExpectedDigest: "dg-empty", ExpectedCursor: 0,
		LastError: "count mismatch", Reopen: true,
	}))
	retried, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-empty", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, retried.State,
		"a zero-unit row is done by construction whatever state it held")
	assert.Empty(t, retried.LastError)
}

func TestExtractCandidatesLegacyNullRowsSettleAfterStamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractCandidate(t, d, "sess-legacy", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-legacy", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-legacy", "fp-a", "dg", 1))
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET local_modified_at = NULL WHERE id = 'sess-legacy'")
	require.NoError(t, err)

	q := ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
		IncludeDone:  true,
	}
	// A legacy row with no recorded local write cannot have changed since
	// its stamp was taken — every write path records one. Revisiting it on
	// every full pass would reload the archive's oldest transcripts
	// forever.
	ids, err := d.ExtractCandidates(ctx, q)
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-legacy",
		"a stamped legacy row must settle, not revisit every full pass")

	// Archives copied from before the stamp column carry an empty stamp:
	// those must re-open once and settle on their first revisit.
	_, err = d.getWriter().Exec(
		"UPDATE recall_extract_progress SET content_stamped_at = '' " +
			"WHERE session_id = 'sess-legacy'")
	require.NoError(t, err)
	ids, err = d.ExtractCandidates(ctx, q)
	require.NoError(t, err)
	assert.Contains(t, ids, "sess-legacy",
		"an unstamped legacy row must re-open for its settling revisit")
}

func seedCommitUnitSession(t *testing.T, d *DB, id string) *Session {
	t.Helper()
	seedExtractCandidate(t, d, id, 2*time.Hour, nil)
	insertMessages(t, d,
		recallEvidenceMessage(id, 0, "user", "ask", id+"-uuid-0"),
		recallEvidenceMessage(id, 1, "assistant", "work", id+"-uuid-1"),
		recallEvidenceMessage(id, 2, "assistant", "done", id+"-uuid-2"),
	)
	// The message writes atomically revoked the scan stamp; restore it the
	// way a completed rescan would.
	require.NoError(t, d.ReplaceSessionSecretFindings(id, nil, 0, "rules-v1"))
	session, err := d.GetSessionFull(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}

func commitUnitEntry(id, sessionID string, start, end int) RecallEntry {
	return RecallEntry{
		ID: id, Type: "fact", ReviewState: "unreviewed_auto",
		Title: "t", Body: "b",
		SourceSessionID: sessionID, SourceRunID: "fp-a",
		ProvenanceOK: true,
		Evidence: []RecallEvidence{{
			SessionID:           sessionID,
			MessageStartOrdinal: start,
			MessageEndOrdinal:   end,
		}},
	}
}

func TestCommitExtractedUnitBindsEvidenceAndAdvances(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	commit := ExtractUnitCommit{
		SessionID: "sess-1", Fingerprint: "fp-a", Digest: "dg", Cursor: 0,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session.MessageCount,
		TranscriptRevision: session.TranscriptRevision,
		LocalModifiedAt:    session.LocalModifiedAt,
		EndedAt:            session.EndedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-1", "sess-1", 0, 1)},
	}
	inserted, err := d.CommitExtractedUnit(ctx, commit)
	require.NoError(t, err)
	assert.Equal(t, 1, inserted)

	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Len(t, entry.Evidence, 1)
	ev := entry.Evidence[0]
	assert.NotEmpty(t, ev.ContentDigest,
		"evidence must carry the host-derived content digest, or the "+
			"reconciler revokes provenance on the first transcript write")
	assert.Equal(t, "sess-1-uuid-0", ev.MessageStartSourceUUID)
	assert.Equal(t, "sess-1-uuid-1", ev.MessageEndSourceUUID)

	progress, found, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, progress.UnitCursor)
	assert.Equal(t, ExtractProgressPartial, progress.State)

	commit.Cursor = 1
	commit.Entries = []RecallEntry{commitUnitEntry("e-2", "sess-1", 2, 2)}
	_, err = d.CommitExtractedUnit(ctx, commit)
	require.NoError(t, err)
	progress, _, err = d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State)
}

func TestCommitExtractedUnitRefusesDriftAndIneligibility(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	commit := ExtractUnitCommit{
		SessionID: "sess-1", Fingerprint: "fp-a", Digest: "dg", Cursor: 0,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session.MessageCount,
		TranscriptRevision: session.TranscriptRevision,
		LocalModifiedAt:    session.LocalModifiedAt,
		EndedAt:            session.EndedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-1", "sess-1", 0, 1)},
	}

	// The snapshot guard is verified inside the insert transaction: a
	// commit carrying a stale view must not persist anything.
	drifted := commit
	drifted.MessageCount = 99
	_, err = d.CommitExtractedUnit(ctx, drifted)
	require.ErrorIs(t, err, ErrExtractSessionDrifted)
	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	assert.Nil(t, entry, "a drifted commit must roll back its entries")
	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Zero(t, progress.UnitCursor,
		"a drifted commit must not advance the cursor")

	// A candidate-confidence finding recorded between the caller's recheck
	// and the commit makes the session ineligible without changing the
	// leak count.
	finding := SecretFinding{
		SessionID: "sess-1", RuleName: "jwt", Confidence: "candidate",
		LocationKind: "message", RedactedMatch: "eyJ…",
		RulesVersion: "rules-v1",
	}
	require.NoError(t, d.ReplaceSessionSecretFindings(
		"sess-1", []SecretFinding{finding}, 0, "rules-v1"))
	fresh, err := d.GetSessionFull(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, fresh)
	withFinding := commit
	withFinding.TranscriptRevision = fresh.TranscriptRevision
	withFinding.LocalModifiedAt = fresh.LocalModifiedAt
	_, err = d.CommitExtractedUnit(ctx, withFinding)
	require.ErrorIs(t, err, ErrExtractSessionDrifted,
		"a finding recorded concurrently must refuse the commit even "+
			"with a matching snapshot")

	// A trashed session refuses the commit outright.
	session2 := seedCommitUnitSession(t, d, "sess-2")
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-2", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession("sess-2"))
	trashed, err := d.GetSessionFull(ctx, "sess-2")
	require.NoError(t, err)
	require.NotNil(t, trashed)
	commit2 := ExtractUnitCommit{
		SessionID: "sess-2", Fingerprint: "fp-a", Digest: "dg", Cursor: 0,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session2.MessageCount,
		TranscriptRevision: trashed.TranscriptRevision,
		LocalModifiedAt:    trashed.LocalModifiedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-3", "sess-2", 0, 1)},
	}
	_, err = d.CommitExtractedUnit(ctx, commit2)
	require.ErrorIs(t, err, ErrExtractSessionDrifted)
}

func TestReconcileIneligibleExtractSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	var err error
	for _, fp := range []string{"fp-a", "fp-old"} {
		_, err = d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	entry := func(id, sessionID, fp, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", ReviewState: reviewState,
			Title: "t", Body: "b",
			SourceSessionID: sessionID, SourceRunID: fp,
			Evidence: []RecallEvidence{{
				SessionID: sessionID, MessageEndOrdinal: 1,
			}},
		}
	}
	for _, id := range []string{
		"sess-trashed", "sess-automated", "sess-finding", "sess-ok",
	} {
		seedExtractCandidate(t, d, id, 2*time.Hour, nil)
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: id, Fingerprint: "fp-a",
			ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
		})
		require.NoError(t, err)
		_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
			entry("e-"+id, id, "fp-a", "unreviewed_auto"),
		})
		require.NoError(t, err)
	}
	// Retraction is generation-independent: a retired-but-registered
	// generation's entries and progress must go too, while runs that are
	// not extraction generations (imports) are untouched.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-trashed", Fingerprint: "fp-old",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-human", "sess-trashed", "fp-a", "human_reviewed"),
		entry("e-old-gen", "sess-trashed", "fp-old", "unreviewed_auto"),
		entry("e-import-run", "sess-trashed", "run-import", "unreviewed_auto"),
	})
	require.NoError(t, err)

	require.NoError(t, d.SoftDeleteSession("sess-trashed"))
	automated, err := d.GetSessionFull(ctx, "sess-automated")
	require.NoError(t, err)
	automated.IsAutomated = true
	require.NoError(t, d.UpsertSession(*automated))
	finding := SecretFinding{
		SessionID: "sess-finding", RuleName: "jwt", Confidence: "candidate",
		LocationKind: "message", RedactedMatch: "eyJ…",
		RulesVersion: "rules-v1",
	}
	require.NoError(t, d.ReplaceSessionSecretFindings(
		"sess-finding", []SecretFinding{finding}, 0, "rules-v1"))

	rowsRemoved, entriesDeleted, err := d.ReconcileIneligibleExtractSessions(
		ctx, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 4, rowsRemoved,
		"three fp-a rows plus the trashed session's fp-old row")
	assert.Equal(t, 4, entriesDeleted)

	for id, want := range map[string]bool{
		"e-sess-trashed":   false,
		"e-sess-automated": false,
		"e-sess-finding":   false,
		"e-old-gen":        false,
		"e-sess-ok":        true,
		"e-human":          true,
		"e-import-run":     true,
	} {
		got, err := d.GetRecallEntry(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, got != nil, "entry %s", id)
	}
	for id, want := range map[string]bool{
		"sess-trashed": false, "sess-automated": false,
		"sess-finding": false, "sess-ok": true,
	} {
		_, found, err := d.ExtractProgress(ctx, id, "fp-a")
		require.NoError(t, err)
		assert.Equal(t, want, found,
			"progress row for %s: a removed row lets a restored session "+
				"rediscover from scratch and stops blocking activation", id)
	}
	_, found, err := d.ExtractProgress(ctx, "sess-trashed", "fp-old")
	require.NoError(t, err)
	assert.False(t, found, "retraction must span every generation")

	// The bound mirrors the done-revisit watermark: every ineligibility
	// write records a local write, so a steady-state pass only examines
	// sessions written since the last completed full pass.
	seedExtractCandidate(t, d, "sess-old-trash", 2*time.Hour, nil)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-old-trash", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession("sess-old-trash"))
	backdateLocalModified(t, d, "sess-old-trash", 3*time.Hour)
	rowsRemoved, _, err = d.ReconcileIneligibleExtractSessions(
		ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Zero(t, rowsRemoved,
		"a bounded reconciliation must skip sessions written before the "+
			"watermark")
	rowsRemoved, _, err = d.ReconcileIneligibleExtractSessions(
		ctx, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 1, rowsRemoved,
		"an unbounded reconciliation must clean the backdated session")
}

func TestActivateExtractGenerationSkipsIneligibleSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	entry := func(id, sessionID string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", ReviewState: "unreviewed_auto",
			Status: "archived", Title: "t", Body: "b",
			SourceSessionID: sessionID, SourceRunID: "fp-a",
			ProvenanceOK: true,
			Evidence: []RecallEvidence{{
				SessionID: sessionID, MessageEndOrdinal: 1,
			}},
		}
	}
	seedExtractCandidate(t, d, "sess-ok", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-trashed", 2*time.Hour, nil)
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-ok", "sess-ok"),
		entry("e-trashed", "sess-trashed"),
	})
	require.NoError(t, err)
	// The session is trashed between staging and activation: promotion
	// must not start serving its entries — it deletes them, since an
	// archived entry under the active generation is stranded if the
	// session is restored before a retraction pass runs.
	require.NoError(t, d.SoftDeleteSession("sess-trashed"))
	// The eligible session is covered; the trashed one is ineligible and
	// needs no coverage. Settle first: seeding bumps local_modified_at,
	// and a same-millisecond stamp reads as stale coverage.
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-ok", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	ok, err := d.GetRecallEntry(ctx, "e-ok")
	require.NoError(t, err)
	require.NotNil(t, ok)
	assert.Equal(t, "accepted", ok.Status)
	trashed, err := d.GetRecallEntry(ctx, "e-trashed")
	require.NoError(t, err)
	assert.Nil(t, trashed,
		"activation must delete entries of an ineligible session")
}

func TestDiscardExtractedSessionOutputIsAtomic(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "dg", 1))
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{{
		ID: "e-1", Type: "fact", ReviewState: "unreviewed_auto",
		Title: "t", Body: "b",
		SourceSessionID: "sess-1", SourceRunID: "fp-a",
		Evidence: []RecallEvidence{{
			SessionID: "sess-1", MessageEndOrdinal: 1,
		}},
	}})
	require.NoError(t, err)

	// A stale guard rolls the whole discard back: deleting the entries
	// while leaving the cursor past them would let a later resume skip
	// units whose output no longer exists.
	err = d.DiscardExtractedSessionOutput(ctx, ExtractFailure{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ExpectedDigest: "dg-other", ExpectedCursor: 1,
		LastError: "ineligible", Reopen: true,
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress)
	got, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	require.NotNil(t, got,
		"a refused discard must not delete the session's entries")

	require.NoError(t, d.DiscardExtractedSessionOutput(ctx, ExtractFailure{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 1,
		LastError: "session became ineligible during extraction",
		Reopen:    true,
	}))
	got, err = d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	assert.Nil(t, got)
	progress, found, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ExtractProgressFailed, progress.State)
	assert.Zero(t, progress.UnitCursor)
}

func TestExtractStatsEntryCountPlanIsIndexBounded(t *testing.T) {
	d := testDB(t)
	rows, err := d.getReader().Query(
		"EXPLAIN QUERY PLAN SELECT COUNT(*) FROM recall_entries " +
			"WHERE source_run_id = 'fp-a'")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		assert.False(t, strings.HasPrefix(detail, "SCAN recall_entries"),
			"stats must not scan the whole corpus per pass: %s", detail)
	}
	require.NoError(t, rows.Err())
}

// refreshCoverageFixture seeds a committed extraction for sess-1 plus
// scoping entries under other generations and sessions, returning the
// current session snapshot.
func refreshCoverageFixture(t *testing.T, d *DB) *Session {
	t.Helper()
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	seedExtractSession(t, d, "sess-2")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	entry := func(id, sessionID, fp, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", ReviewState: reviewState,
			Title: "t", Body: "b",
			Project: "proj", CWD: "/old", GitBranch: "main", Agent: "claude",
			SourceSessionID: sessionID, SourceRunID: fp,
			Evidence: []RecallEvidence{{
				SessionID: sessionID, MessageEndOrdinal: 1,
			}},
		}
	}
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-auto", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-reviewed", "sess-1", "fp-a", "human_reviewed"),
		entry("e-other-fp", "sess-1", "fp-b", "unreviewed_auto"),
		entry("e-other-sess", "sess-2", "fp-a", "unreviewed_auto"),
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	return session
}

func refreshRequest(session *Session) ExtractCoverageRefresh {
	return ExtractCoverageRefresh{
		Fingerprint:  "fp-a",
		Digest:       "dg",
		StampedAt:    time.Now(),
		ScanVersions: []string{"rules-v1"},
		Session:      session,
	}
}

func TestRefreshExtractedSessionCoverageSyncsContext(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := refreshCoverageFixture(t, d)

	// A metadata-only session update leaves the units digest unchanged;
	// the refresh must move the generated entries to the new context.
	session.Project = "proj-2"
	session.Cwd = "/new"
	session.GitBranch = "feature"
	session.Agent = "codex"
	require.NoError(t, d.UpsertSession(*session))
	snapshot, err := d.GetSessionFull(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	progress, err := d.RefreshExtractedSessionCoverage(
		ctx, refreshRequest(snapshot))
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State)

	got, err := d.GetRecallEntry(ctx, "e-auto")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "proj-2", got.Project)
	assert.Equal(t, "/new", got.CWD)
	assert.Equal(t, "feature", got.GitBranch)
	assert.Equal(t, "codex", got.Agent)

	// Human-touched entries and other generations or sessions stay as they
	// were.
	for _, id := range []string{"e-reviewed", "e-other-fp", "e-other-sess"} {
		got, err := d.GetRecallEntry(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got, id)
		assert.Equal(t, "proj", got.Project, id)
		assert.Equal(t, "main", got.GitBranch, id)
	}

	// An already-synchronized corpus refreshes without error.
	_, err = d.RefreshExtractedSessionCoverage(ctx, refreshRequest(snapshot))
	require.NoError(t, err)
}

// TestRefreshExtractedSessionCoverageRefusesDrift pins the refresh guard: a
// transcript write landing after the caller bracketed its snapshot must
// roll the whole refresh back — otherwise stale entries would be rebound to
// the new transcript, marked provenance-verified, and the coverage stamp
// would claim the unseen write.
func TestRefreshExtractedSessionCoverageRefusesDrift(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := refreshCoverageFixture(t, d)
	_, err := d.getWriter().Exec(
		"UPDATE recall_entries SET provenance_ok = 0 WHERE id = 'e-auto'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE recall_evidence SET content_digest = 'stale' " +
			"WHERE entry_id = 'e-auto'")
	require.NoError(t, err)
	readStamp := func() string {
		t.Helper()
		var stamp string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT content_stamped_at FROM recall_extract_progress "+
				"WHERE session_id = 'sess-1'").Scan(&stamp))
		return stamp
	}
	before := readStamp()

	// A concurrent write bumps the session after the snapshot was taken.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET message_count = message_count + 1 " +
			"WHERE id = 'sess-1'")
	require.NoError(t, err)

	_, err = d.RefreshExtractedSessionCoverage(ctx, refreshRequest(session))
	require.ErrorIs(t, err, ErrExtractSessionDrifted)
	got, err := d.GetRecallEntry(ctx, "e-auto")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.ProvenanceOK,
		"a drifted refresh must not restore provenance")
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "stale", got.Evidence[0].ContentDigest,
		"a drifted refresh must not rebind evidence")
	assert.Equal(t, before, readStamp(),
		"a drifted refresh must not advance the coverage stamp")
}

func TestExtractMutationsWaitForDBMutex(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	// Every mutation below is valid regardless of the order the map yields
	// them in: the progress row exists with digest-1 and never reaches done.
	mutations := map[string]func() error{
		"EnsureExtractGeneration": func() error {
			_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
				Fingerprint: "fp-b", Model: "m", Segmenter: "turns-v1",
			})
			return err
		},
		"ActivateExtractGeneration": func() error {
			err := d.ActivateExtractGeneration(
				ctx, "fp-a", []string{"rules-v1"}, time.Now())
			if errors.Is(err, ErrExtractActivationBlocked) {
				// The in-tx activation guards refuse (the seeded row
				// never reaches done); this subtest only measures that
				// the write waited for db.mu.
				return nil
			}
			return err
		},
		"RetireExtractGeneration": func() error {
			return d.RetireExtractGeneration(ctx, "fp-a", true)
		},
		"UpsertExtractProgress": func() error {
			_, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
				SessionID: "sess-1", Fingerprint: "fp-a",
				ContentDigest: "digest-1", UnitsTotal: 2, StampedAt: time.Now(),
			})
			return err
		},
		"AdvanceExtractCursor": func() error {
			return d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 1)
		},
		"MarkExtractProgressFailed": func() error {
			// The advance subtest may or may not have run yet, so observe
			// the stored cursor the way a real worker would.
			progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
			if err != nil {
				return err
			}
			return d.MarkExtractProgressFailed(ctx, ExtractFailure{
				SessionID:      "sess-1",
				Fingerprint:    "fp-a",
				ExpectedDigest: "digest-1",
				ExpectedCursor: progress.UnitCursor,
				LastError:      "x",
			})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d.mu.Lock()
			done := make(chan error, 1)
			go func() { done <- mutate() }()
			select {
			case <-done:
				d.mu.Unlock()
				t.Fatal("mutation completed while db.mu was held; " +
					"CloseConnections relies on db.mu to quiesce writes")
			case <-time.After(100 * time.Millisecond):
			}
			d.mu.Unlock()
			require.NoError(t, <-done)
		})
	}
}

func TestUpsertExtractProgressZeroUnitsCompletesImmediately(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	progress, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a session with no units has nothing left to extract")
	assert.Equal(t, 0, progress.UnitCursor)

	progress, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-2", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a digest reset to zero units must also complete immediately")

	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-3", UnitsTotal: -1, StampedAt: time.Now(),
	})
	require.Error(t, err, "negative unit totals must be refused")
}

func TestAdvanceExtractCursorStaleAfterShrinkingReset(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 10, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 7))

	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-2", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	err = d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 8)
	require.ErrorIs(t, err, ErrStaleExtractProgress,
		"a stale worker beyond the shrunken total must get the typed stale "+
			"error that triggers re-read, not a bounds error")

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, 0, progress.UnitCursor)
	assert.Equal(t, "digest-2", progress.ContentDigest)
}

func TestMarkExtractProgressFailedRejectsAdvancedCursor(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))

	err = d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 1,
		LastError:      "worker that lost the race",
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress,
		"a failure from a worker behind the stored cursor must not demote "+
			"newer progress")

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPartial, progress.State)
	assert.Equal(t, 2, progress.UnitCursor)
	assert.Empty(t, progress.LastError)
}

func TestAdvanceExtractCursorReplayKeepsFailureState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "digest-1", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 2,
		LastError:      "boom",
	}))

	// A delayed duplicate of the cursor-2 advance completed no new unit;
	// it must be an accepted no-op, not resurrect the failed row.
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))

	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressFailed, progress.State)
	assert.Equal(t, 2, progress.UnitCursor)
	assert.Equal(t, "boom", progress.LastError)
}

func seedExtractCandidate(
	t *testing.T, d *DB, id string, endedAgo time.Duration, mutate func(*Session),
) {
	t.Helper()
	ended := time.Now().Add(-endedAgo).UTC().Format("2006-01-02T15:04:05.000Z")
	s := Session{
		ID:           id,
		Project:      "proj",
		Machine:      defaultMachine,
		Agent:        defaultAgent,
		EndedAt:      &ended,
		MessageCount: 3,
	}
	if mutate != nil {
		mutate(&s)
	}
	require.NoError(t, d.UpsertSession(s))
	// Mark the session cleanly scanned under the test rules version;
	// eligibility requires a current scan, not just a zero leak count.
	require.NoError(t, d.ReplaceSessionSecretFindings(id, nil, 0, "rules-v1"))
}

// TestExtractCandidatesMixedPrecisionEndedAt pins the quiet-period
// comparison across RFC3339Nano's variable fractional precision: ended_at
// trims trailing zeros ("...45Z", "...45.52Z") while the cutoff is always
// fixed-millis, and comparing the raw strings sorts a trimmed value after
// a longer one in the same second ('Z' > any digit or '.'). A session
// misjudged here is skipped while the discovery watermark advances past
// its last write, stranding it until a daemon restart resets watermarks.
func TestExtractCandidatesMixedPrecisionEndedAt(t *testing.T) {
	cases := map[string]struct {
		endedAt string
		cutoff  time.Time
		want    bool
	}{
		"whole second inside cutoff": {"2026-01-02T10:00:45Z",
			time.Date(2026, 1, 2, 10, 0, 45, 123_000_000, time.UTC), true},
		"trimmed millis inside cutoff": {"2026-01-02T10:00:45.52Z",
			time.Date(2026, 1, 2, 10, 0, 45, 523_000_000, time.UTC), true},
		"same second past cutoff": {"2026-01-02T10:00:45.9Z",
			time.Date(2026, 1, 2, 10, 0, 45, 123_000_000, time.UTC), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			seedExtractCandidate(t, d, "sess-1", 2*time.Hour,
				func(s *Session) { s.EndedAt = &tc.endedAt })
			ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
				Fingerprint:  "fp-a",
				QuietCutoff:  tc.cutoff,
				ScanVersions: []string{"rules-v1"},
			})
			require.NoError(t, err)
			got := len(ids) == 1
			require.Equal(t, tc.want, got,
				"ended_at %s vs cutoff %s: selected=%v, want %v",
				tc.endedAt, tc.cutoff.Format(time.RFC3339Nano), got, tc.want)
		})
	}
}

// TestExtractCandidateRetryArmUsesUpdatedAtIndex pins that failed-row
// retry discovery is index-bounded: without an updated_at range in the
// index probe, every failed row of a generation — including rows still in
// backoff — is fetched and filtered on each scheduler pass, so pass cost
// grows with the archive's failure history instead of with actionable
// work. The plan assertion stands in for a cardinality benchmark: it is
// deterministic where timing comparisons flake.
func TestExtractCandidateRetryArmUsesUpdatedAtIndex(t *testing.T) {
	d := testDB(t)
	query, args, err := extractCandidateSQL(ExtractCandidateQuery{
		Fingerprint:       "fp-a",
		QuietCutoff:       time.Now().Add(-30 * time.Minute),
		FailedRetryCutoff: time.Now().Add(-time.Hour),
		ScanVersions:      []string{"rules-v1"},
	})
	require.NoError(t, err)
	rows, err := d.getWriter().Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	if !strings.Contains(plan.String(), "updated_at<") {
		t.Fatalf("failed-retry arm is not bounded by updated_at:\n%s",
			plan.String())
	}
}

func TestExtractCandidatesFiltersIneligibleSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-ok", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-automated", 2*time.Hour, func(s *Session) {
		s.IsAutomated = true
	})
	seedExtractCandidate(t, d, "sess-empty", 2*time.Hour, func(s *Session) {
		s.MessageCount = 0
	})
	seedExtractCandidate(t, d, "sess-open", 2*time.Hour, func(s *Session) {
		s.EndedAt = nil
	})
	seedExtractCandidate(t, d, "sess-recent", 5*time.Minute, nil)
	seedExtractCandidate(t, d, "sess-secret", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-trashed", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-stale-scan", 2*time.Hour, nil)
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET secret_leak_count = 2 WHERE id = 'sess-secret'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET deleted_at = '2026-01-01T00:00:00.000Z' " +
			"WHERE id = 'sess-trashed'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET secrets_rules_version = 'rules-v0' " +
			"WHERE id = 'sess-stale-scan'")
	require.NoError(t, err)
	// Never scanned: secrets_rules_version stays '' with leak count 0.
	unscannedEnded := time.Now().Add(-2 * time.Hour).UTC().
		Format("2006-01-02T15:04:05.000Z")
	require.NoError(t, d.UpsertSession(Session{
		ID: "sess-unscanned", Project: "proj",
		Machine: defaultMachine, Agent: defaultAgent,
		EndedAt: &unscannedEnded, MessageCount: 3,
	}))

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-ok"}, ids,
		"unscanned and stale-scanned sessions must never be candidates")
}

func TestExtractCandidatesExcludeSessionsWithAnyFinding(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-clean", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-candidate", 2*time.Hour, nil)
	// A candidate-confidence finding (e.g. a JWT or high-entropy match)
	// is recorded but never counted in secret_leak_count. It must still
	// disqualify the session: confidence tunes alerting, not what may be
	// sent to a model.
	require.NoError(t, d.ReplaceSessionSecretFindings(
		"sess-candidate",
		[]SecretFinding{{
			SessionID:    "sess-candidate",
			RuleName:     "high-entropy-assignment",
			Confidence:   "candidate",
			LocationKind: "message",
		}},
		0, "rules-v1",
	))

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-clean"}, ids,
		"candidate findings must exclude a session even with leak count 0")
}

func TestExtractCandidatesDoneRevisitUsesContentStamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-done", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-done", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-done", "fp-a", "dg", 1))

	// A transcript write lands mid-extraction: after the unit list was
	// derived (content stamp) but before the final cursor advance. The
	// progress row's updated_at overtakes it, so a gate on updated_at
	// would hide the change forever.
	now := time.Now().UTC()
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET local_modified_at = ? WHERE id = 'sess-done'",
		now.Add(2*time.Second).Format("2006-01-02T15:04:05.000Z"))
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE recall_extract_progress SET updated_at = ? "+
			"WHERE session_id = 'sess-done'",
		now.Add(5*time.Second).Format("2006-01-02T15:04:05.000Z"))
	require.NoError(t, err)

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
		IncludeDone:  true,
	})
	require.NoError(t, err)
	assert.Contains(t, ids, "sess-done",
		"a write after the unit snapshot must re-open the session even "+
			"when progress was updated later")
}

func TestUpsertExtractProgressStampsCallerCutoff(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	readStamp := func() string {
		t.Helper()
		var stamp string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT content_stamped_at FROM recall_extract_progress "+
				"WHERE session_id = 'sess-1'").Scan(&stamp))
		return stamp
	}

	// The stamp is the caller's cutoff, captured before it read the
	// transcript — not the row's write time. A write landing between the
	// read and this upsert must compare as after the stamp.
	first := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: first,
	})
	require.NoError(t, err)
	assert.Equal(t, "2026-07-01T10:00:00.000Z", readStamp())
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "dg", 1))

	// A revisit that re-derives the same digest advances the stamp to its
	// own cutoff: the transcript was re-verified as of the new read, and a
	// stale stamp would leave later metadata writes re-opening the session
	// on every full pass forever.
	second := first.Add(time.Hour)
	progress, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: second,
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a same-digest upsert must not reset completed progress")
	assert.Equal(t, "2026-07-01T11:00:00.000Z", readStamp(),
		"a same-digest upsert must advance the stamp to the new cutoff")

	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1,
	})
	assert.Error(t, err, "a zero cutoff would silently claim coverage "+
		"through the row's write time")
}

func TestActivateExtractGenerationSwitchesServedEntries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// Staged output only exists for sessions that were eligible and
	// covered at commit time; promotion clears output of sessions that
	// drifted, so the seed must reflect that reality.
	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	time.Sleep(2 * time.Millisecond)
	for _, fp := range []string{"fp-old", "fp-new"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: "sess-1", Fingerprint: fp,
			ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
		})
		require.NoError(t, err)
	}
	entry := func(id, fp, status, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", Status: status, ReviewState: reviewState,
			Title: "t", Body: "b", ProvenanceOK: true,
			SourceSessionID: "sess-1", SourceRunID: fp,
		}
	}
	_, err := d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-old", "fp-old", "archived", "unreviewed_auto"),
		entry("e-new-staged", "fp-new", "archived", "unreviewed_auto"),
		entry("e-reviewed", "fp-old", "accepted", "human_reviewed"),
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-old", []string{"rules-v1"}, time.Now()))

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-new", []string{"rules-v1"}, time.Now()))

	status := func(id string) string {
		got, err := d.GetRecallEntry(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got, id)
		return got.Status
	}
	assert.Equal(t, "accepted", status("e-new-staged"),
		"activation must promote the new generation's staged entries")
	assert.Equal(t, "archived", status("e-old"),
		"activation must stop serving the retired generation's entries")
	assert.Equal(t, "accepted", status("e-reviewed"),
		"human-reviewed entries are not lifecycle-managed")
}

func TestRetireExtractGenerationArchivesServedEntries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-1", "fp-a")
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{{
		ID: "e-1", Type: "fact", Status: "archived",
		ReviewState: "unreviewed_auto", Title: "t", Body: "b",
		ProvenanceOK:    true,
		SourceSessionID: "sess-1", SourceRunID: "fp-a",
	}})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))

	require.NoError(t, d.RetireExtractGeneration(ctx, "fp-a", true))
	got, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "archived", got.Status,
		"retiring a generation must stop serving its entries")
}

func TestExtractCandidatesRequireScanVersions(t *testing.T) {
	d := testDB(t)
	_, err := d.ExtractCandidates(context.Background(), ExtractCandidateQuery{
		Fingerprint: "fp-a",
		QuietCutoff: time.Now(),
	})
	require.Error(t, err,
		"a query without scan versions would treat unscanned as clean")
}

func TestExtractCandidatesRespectsProgressState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Distinct ended-at offsets pin the expected order (oldest first).
	seedExtractCandidate(t, d, "sess-new", 6*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-pending", 5*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-partial", 4*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-done", 3*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-failed-fresh", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-failed-stale", 1*time.Hour, nil)

	for _, fp := range []string{"fp-a", "fp-b"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	_, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-pending", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-partial", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t,
		d.AdvanceExtractCursor(ctx, "sess-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-done", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-done", "fp-a", "dg", 1))
	for _, id := range []string{"sess-failed-fresh", "sess-failed-stale"} {
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: id, Fingerprint: "fp-a",
			ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
		})
		require.NoError(t, err)
		require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
			SessionID: id, Fingerprint: "fp-a",
			ExpectedDigest: "dg", LastError: "boom",
		}))
	}
	_, err = d.getWriter().Exec(
		"UPDATE recall_extract_progress SET updated_at = " +
			"'2000-01-01T00:00:00.000Z' WHERE session_id = 'sess-failed-stale'")
	require.NoError(t, err)
	// Progress under another generation must not hide a session from fp-a.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-new", Fingerprint: "fp-b",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-new", "fp-b", "dg", 1))

	query := ExtractCandidateQuery{
		Fingerprint:       "fp-a",
		QuietCutoff:       time.Now().Add(-30 * time.Minute),
		FailedRetryCutoff: time.Now().Add(-30 * time.Minute),
		ScanVersions:      []string{"rules-v1"},
	}
	ids, err := d.ExtractCandidates(ctx, query)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"sess-new", "sess-pending", "sess-partial", "sess-failed-stale"},
		ids, "done stays done, fresh failures wait out the backoff")

	// A done session whose transcript has not changed since extraction is
	// left alone even by a full pass; only new writes re-open it.
	query.IncludeDone = true
	ids, err = d.ExtractCandidates(ctx, query)
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-done",
		"unchanged done sessions must not be reloaded by full passes")

	_, err = d.getWriter().Exec(
		"UPDATE sessions SET local_modified_at = '2999-01-01T00:00:00.000Z' " +
			"WHERE id = 'sess-done'")
	require.NoError(t, err)
	ids, err = d.ExtractCandidates(ctx, query)
	require.NoError(t, err)
	assert.Contains(t, ids, "sess-done",
		"a transcript write after extraction re-opens the session")

	query.IncludeDone = false
	query.Limit = 2
	ids, err = d.ExtractCandidates(ctx, query)
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-new", "sess-pending"}, ids)
}

func TestExtractCandidatesZeroFailedCutoffSkipsFailedRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-failed", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-failed", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-failed", Fingerprint: "fp-a",
		ExpectedDigest: "dg", LastError: "boom",
	}))

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
	})
	require.NoError(t, err)
	assert.Empty(t, ids, "zero retry cutoff must never resurrect failures")
}

func TestUpsertExtractProgressDigestChangeScopesDeletion(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	seedExtractSession(t, d, "sess-2")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)

	entry := func(id, sessionID, fp, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", ReviewState: reviewState,
			Title: "t", Body: "b",
			SourceSessionID: sessionID, SourceRunID: fp,
			Evidence: []RecallEvidence{{
				SessionID: sessionID, MessageEndOrdinal: 1,
			}},
		}
	}
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-del-1", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-del-2", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-reviewed", "sess-1", "fp-a", "human_reviewed"),
		entry("e-other-fp", "sess-1", "fp-b", "unreviewed_auto"),
		entry("e-other-sess", "sess-2", "fp-a", "unreviewed_auto"),
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-1", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	// The digest-change delete reaches only this generation and session's
	// machine entries.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-2", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	for id, want := range map[string]bool{
		"e-del-1": false, "e-del-2": false,
		"e-reviewed": true, "e-other-fp": true, "e-other-sess": true,
	} {
		got, err := d.GetRecallEntry(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, got != nil, "entry %s", id)
	}
	var evidence int
	require.NoError(t, d.getWriter().QueryRow(
		"SELECT COUNT(*) FROM recall_evidence WHERE entry_id IN "+
			"('e-del-1','e-del-2')").Scan(&evidence))
	assert.Zero(t, evidence, "evidence must not outlive deleted entries")
}

func TestInsertExtractedRecallEntriesIsIdempotent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")

	entry := func(id, title string) RecallEntry {
		return RecallEntry{
			ID:              id,
			Type:            "fact",
			Scope:           "project",
			ReviewState:     "unreviewed_auto",
			Title:           title,
			Body:            "body",
			Project:         "proj",
			SourceSessionID: "sess-1",
			SourceRunID:     "fp-a",
			ExtractorMethod: "turns-v1",
			Model:           "model-x",
			Evidence: []RecallEvidence{{
				SessionID:           "sess-1",
				MessageStartOrdinal: 0,
				MessageEndOrdinal:   2,
			}},
		}
	}

	inserted, err := d.InsertExtractedRecallEntries(ctx,
		[]RecallEntry{entry("id-1", "one"), entry("id-2", "two")})
	require.NoError(t, err)
	assert.Equal(t, 2, inserted)

	inserted, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("id-1", "one"), entry("id-2", "two"), entry("id-3", "three"),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, inserted, "replayed entries are skipped, not duplicated")

	var entries, evidence int
	require.NoError(t, d.getWriter().QueryRow(
		"SELECT COUNT(*) FROM recall_entries").Scan(&entries))
	require.NoError(t, d.getWriter().QueryRow(
		"SELECT COUNT(*) FROM recall_evidence WHERE entry_id = 'id-1'",
	).Scan(&evidence))
	assert.Equal(t, 3, entries)
	assert.Equal(t, 1, evidence, "skipped entries must not re-insert evidence")
}

func TestInsertExtractedRecallEntriesRollsBackOnInvalidEntry(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")

	good := RecallEntry{
		ID: "id-ok", Type: "fact", ReviewState: "unreviewed_auto",
		Title: "t", Body: "b", SourceSessionID: "sess-1",
	}
	bad := RecallEntry{
		ID: "id-bad", Type: "fact", ReviewState: "not-a-state",
		Title: "t", Body: "b", SourceSessionID: "sess-1",
	}
	_, err := d.InsertExtractedRecallEntries(ctx, []RecallEntry{good, bad})
	require.Error(t, err)

	var count int
	require.NoError(t, d.getWriter().QueryRow(
		"SELECT COUNT(*) FROM recall_entries").Scan(&count))
	assert.Zero(t, count, "batch must be atomic")
}

func TestExtractProgressStatsAggregatesByState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for _, id := range []string{"s-pending", "s-partial", "s-done", "s-failed"} {
		seedExtractSession(t, d, id)
	}
	for _, fp := range []string{"fp-a", "fp-b"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	_, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s-pending", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s-partial", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 3, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "s-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s-done", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "s-done", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s-failed", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 4, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "s-failed", Fingerprint: "fp-a",
		ExpectedDigest: "dg", LastError: "boom",
	}))
	// Rows under another generation must not leak into fp-a's stats.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s-pending", Fingerprint: "fp-b",
		ContentDigest: "dg", UnitsTotal: 9, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		{ID: "e-1", Type: "fact", ReviewState: "unreviewed_auto",
			Title: "t", Body: "b", SourceSessionID: "s-done", SourceRunID: "fp-a"},
		{ID: "e-2", Type: "fact", ReviewState: "unreviewed_auto",
			Title: "t", Body: "b", SourceSessionID: "s-done", SourceRunID: "fp-a"},
		{ID: "e-3", Type: "fact", ReviewState: "unreviewed_auto",
			Title: "t", Body: "b", SourceSessionID: "s-done", SourceRunID: "fp-b"},
	})
	require.NoError(t, err)

	stats, err := d.ExtractProgressStats(ctx, "fp-a")
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Pending)
	assert.Equal(t, 1, stats.Partial)
	assert.Equal(t, 1, stats.Done)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 2, stats.UnitsDone)
	assert.Equal(t, 10, stats.UnitsTotal)
	assert.Equal(t, 2, stats.Entries)
}

func backdateLocalModified(t *testing.T, d *DB, id string, ago time.Duration) {
	t.Helper()
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET local_modified_at = ? WHERE id = ?",
		time.Now().Add(-ago).UTC().Format("2006-01-02T15:04:05.000Z"), id)
	require.NoError(t, err)
}

func TestExtractCandidatesChangedSinceLimitsDiscovery(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-old", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-fresh", 2*time.Hour, nil)
	backdateLocalModified(t, d, "sess-old", 3*time.Hour)

	base := ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
	}

	ids, err := d.ExtractCandidates(ctx, base)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sess-old", "sess-fresh"}, ids,
		"an unrestricted scan must discover everything")

	limited := base
	limited.ChangedSince = time.Now().Add(-time.Hour)
	ids, err = d.ExtractCandidates(ctx, limited)
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-fresh"}, ids,
		"discovery must skip sessions not written since the watermark")

	// A session with no recorded local write predates the watermark column:
	// it must stay discoverable rather than be silently stranded.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET local_modified_at = NULL WHERE id = 'sess-old'")
	require.NoError(t, err)
	ids, err = d.ExtractCandidates(ctx, limited)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sess-old", "sess-fresh"}, ids,
		"a NULL local_modified_at must not hide a session from discovery")
}

func TestExtractCandidatesChangedSinceKeepsProgressBacklog(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-partial", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-failed", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-partial", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t,
		d.AdvanceExtractCursor(ctx, "sess-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-failed", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-failed", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 0, LastError: "boom",
	}))
	backdateLocalModified(t, d, "sess-partial", 3*time.Hour)
	backdateLocalModified(t, d, "sess-failed", 3*time.Hour)

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:       "fp-a",
		QuietCutoff:       time.Now().Add(-30 * time.Minute),
		FailedRetryCutoff: time.Now().Add(time.Minute),
		ScanVersions:      []string{"rules-v1"},
		ChangedSince:      time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sess-partial", "sess-failed"}, ids,
		"the watermark limits discovery only; interrupted and retryable "+
			"sessions already in progress must always be offered")
}

func TestExtractCandidatesChangedSinceAvoidsSessionScan(t *testing.T) {
	d := testDB(t)

	query, args, err := extractCandidateSQL(ExtractCandidateQuery{
		Fingerprint:       "fp-a",
		QuietCutoff:       time.Now().Add(-30 * time.Minute),
		FailedRetryCutoff: time.Now().Add(-time.Hour),
		ScanVersions:      []string{"rules-v1"},
		ChangedSince:      time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	for _, detail := range details {
		assert.NotRegexp(t, `^SCAN s\b`, detail,
			"a watermarked scan must not walk the whole sessions table; "+
				"plan:\n%s", strings.Join(details, "\n"))
	}
}

func TestTranscriptMutationInvalidatesSecretScanFreshness(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	msgs := []Message{{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hi"}}
	require.NoError(t, d.InsertMessages(msgs))
	require.NoError(t, d.ReplaceSessionSecretFindings("sess-1", nil, 0, "rules-v1"))

	// Appending messages must revoke scan freshness in the same
	// transaction: the incremental sync path re-scans in a separate later
	// write, and until it lands the appended content is unscanned.
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "token"},
	}))
	session, err := d.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Empty(t, session.SecretsRulesVersion,
		"a transcript mutation must atomically invalidate the secret scan")

	ids, err := d.ExtractCandidates(ctx, ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
	})
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-1",
		"a session whose scan was invalidated must not be a candidate")
}

func TestReplaceSessionContentEndsScanStamped(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	msgs := []Message{{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hi"}}
	require.NoError(t, d.InsertMessages(msgs))

	// The full-replace path persists messages, signals, and findings in one
	// transaction; the mid-transaction invalidation must not leak out.
	require.NoError(t, d.ReplaceSessionContent("sess-1",
		[]Message{
			{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello"},
		},
		SessionSignalUpdate{SecretsRulesVersion: "rules-v2"},
		nil,
	))
	session, err := d.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "rules-v2", session.SecretsRulesVersion,
		"an atomic content replace carries its own scan stamp")
}

func TestCopyRecallExtractStatePreservesContentStamp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcDB, err := Open(filepath.Join(dir, "old.db"))
	require.NoError(t, err, "open src")
	seedExtractSession(t, srcDB, "s1")
	_, err = srcDB.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	stamp := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err = srcDB.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: stamp,
	})
	require.NoError(t, err)
	require.NoError(t, srcDB.AdvanceExtractCursor(ctx, "s1", "fp-a", "dg", 1))
	require.NoError(t, srcDB.Close())

	destDB, err := Open(filepath.Join(dir, "new.db"))
	require.NoError(t, err, "open dest")
	defer destDB.Close()
	seedExtractSession(t, destDB, "s1")

	require.NoError(t,
		destDB.CopyRecallEntriesFrom(filepath.Join(dir, "old.db")))

	// An empty stamp reads as "changed since coverage" for every completed
	// session, so losing it across a resync would reload the whole
	// archive's transcripts on the next full pass.
	var copied string
	require.NoError(t, destDB.getReader().QueryRow(
		"SELECT content_stamped_at FROM recall_extract_progress "+
			"WHERE session_id = 's1'").Scan(&copied))
	assert.Equal(t, "2026-07-01T10:00:00.000Z", copied,
		"resync must preserve the transcript-read stamp")
}

func TestCopyRecallExtractStateToleratesPreStampArchives(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcDB, err := Open(filepath.Join(dir, "old.db"))
	require.NoError(t, err, "open src")
	seedExtractSession(t, srcDB, "s1")
	_, err = srcDB.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = srcDB.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "s1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	// Simulate an archive written before the stamp column existed.
	_, err = srcDB.getWriter().Exec(
		"ALTER TABLE recall_extract_progress DROP COLUMN content_stamped_at")
	require.NoError(t, err)
	require.NoError(t, srcDB.Close())

	destDB, err := Open(filepath.Join(dir, "new.db"))
	require.NoError(t, err, "open dest")
	defer destDB.Close()
	seedExtractSession(t, destDB, "s1")

	require.NoError(t,
		destDB.CopyRecallEntriesFrom(filepath.Join(dir, "old.db")))
	var state string
	require.NoError(t, destDB.getReader().QueryRow(
		"SELECT state FROM recall_extract_progress "+
			"WHERE session_id = 's1'").Scan(&state))
	assert.Equal(t, ExtractProgressPending, state,
		"pre-stamp rows still copy; their empty stamp re-opens them once")
}

func TestExtractCandidatesDoneRevisitBoundedByWatermark(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	for _, id := range []string{"sess-old-change", "sess-new-change"} {
		seedExtractCandidate(t, d, id, 4*time.Hour, nil)
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: id, Fingerprint: "fp-a",
			ContentDigest: "dg", UnitsTotal: 1,
			StampedAt: time.Now().Add(-3 * time.Hour),
		})
		require.NoError(t, err)
		require.NoError(t, d.AdvanceExtractCursor(ctx, id, "fp-a", "dg", 1))
	}
	// Both sessions changed after their unit snapshots, but only one
	// changed since the last full pass; the other was already offered to
	// (and evidently reconciled by) an earlier full pass.
	backdateLocalModified(t, d, "sess-old-change", 2*time.Hour)
	backdateLocalModified(t, d, "sess-new-change", 10*time.Minute)

	base := ExtractCandidateQuery{
		Fingerprint:  "fp-a",
		QuietCutoff:  time.Now().Add(-30 * time.Minute),
		ScanVersions: []string{"rules-v1"},
		IncludeDone:  true,
	}
	ids, err := d.ExtractCandidates(ctx, base)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{"sess-old-change", "sess-new-change"}, ids,
		"an unbounded revisit scan must offer every changed done session")

	bounded := base
	bounded.DoneChangedSince = time.Now().Add(-time.Hour)
	ids, err = d.ExtractCandidates(ctx, bounded)
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-new-change"}, ids,
		"a bounded revisit scan must only walk sessions written since "+
			"the last full pass")
}

func TestExtractCandidatesFullScanPlanIsIndexBounded(t *testing.T) {
	d := testDB(t)

	query, args, err := extractCandidateSQL(ExtractCandidateQuery{
		Fingerprint:       "fp-a",
		QuietCutoff:       time.Now().Add(-30 * time.Minute),
		FailedRetryCutoff: time.Now().Add(-time.Hour),
		ScanVersions:      []string{"rules-v1"},
		IncludeDone:       true,
		ChangedSince:      time.Now().Add(-time.Hour),
		DoneChangedSince:  time.Now().Add(-2 * time.Hour),
	})
	require.NoError(t, err)

	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	for _, detail := range details {
		assert.NotRegexp(t, `^SCAN s\b`, detail,
			"a watermarked full pass must not walk the sessions table; "+
				"plan:\n%s", strings.Join(details, "\n"))
		assert.NotRegexp(t, `^SCAN p\b`, detail,
			"a watermarked full pass must not walk every progress row; "+
				"plan:\n%s", strings.Join(details, "\n"))
	}
}

// seedServableExtractEntry stores one archived, provenance-verified machine
// entry so activation has something to promote and serve.
func seedServableExtractEntry(t *testing.T, d *DB, fingerprint, sessionID, entryID string) {
	t.Helper()
	_, err := d.InsertExtractedRecallEntries(context.Background(), []RecallEntry{{
		ID: entryID, Type: "fact", ReviewState: "unreviewed_auto",
		Status: "archived", Title: "t", Body: "b",
		SourceSessionID: sessionID, SourceRunID: fingerprint,
		ProvenanceOK: true,
	}})
	require.NoError(t, err)
}

// seedCoveredExtractSession seeds an eligible session with a done
// progress row under each given generation: staged output only exists for
// sessions that were eligible and covered at commit time, and activation
// clears the staged output of sessions that drifted, so an
// under-specified session row would misrepresent what promotion sees.
func seedCoveredExtractSession(
	t *testing.T, d *DB, id string, fingerprints ...string,
) {
	t.Helper()
	seedExtractCandidate(t, d, id, 2*time.Hour, nil)
	time.Sleep(2 * time.Millisecond)
	for _, fingerprint := range fingerprints {
		_, err := d.UpsertExtractProgress(
			context.Background(), ExtractProgressUpsert{
				SessionID: id, Fingerprint: fingerprint,
				ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
			})
		require.NoError(t, err)
	}
}

func generationStates(t *testing.T, d *DB) map[string]string {
	t.Helper()
	generations, err := d.ExtractGenerations(context.Background())
	require.NoError(t, err)
	states := map[string]string{}
	for _, gen := range generations {
		states[gen.Fingerprint] = gen.State
	}
	return states
}

// TestActivateExtractGenerationRefusesUnfinishedCoverage pins that the
// coverage gate holds inside the activation transaction: a session can slip
// back to pending between the caller's backlog probe and the activation
// write, and promoting around it would retire the served corpus while the
// replacement is still being built.
func TestActivateExtractGenerationRefusesUnfinishedCoverage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, fp := range []string{"fp-old", "fp-a"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	seedExtractCandidate(t, d, "sess-old", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-old", "sess-old", "e-old")
	time.Sleep(2 * time.Millisecond)
	_, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-old", Fingerprint: "fp-old",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-old", []string{"rules-v1"}, time.Now()))
	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-1", "e-a")
	time.Sleep(2 * time.Millisecond)
	// sess-old is covered under fp-a too, so the block below is
	// attributable to sess-1's unfinished row alone.
	for _, sessionID := range []string{"sess-old", "sess-1"} {
		units := 0
		if sessionID == "sess-1" {
			units = 2
		}
		_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
			SessionID: sessionID, Fingerprint: "fp-a",
			ContentDigest: "dg", UnitsTotal: units, StampedAt: time.Now(),
		})
		require.NoError(t, err)
	}

	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked)
	states := generationStates(t, d)
	assert.Equal(t, ExtractGenerationActive, states["fp-old"],
		"a blocked activation must leave the served corpus in place")
	assert.Equal(t, ExtractGenerationBuilding, states["fp-a"])
	entry, err := d.GetRecallEntry(ctx, "e-a")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, "archived", entry.Status)
}

// TestActivateExtractGenerationIgnoresIneligibleUnfinishedCoverage pins
// that the unfinished-coverage gate only counts sessions activation could
// still serve: a pending row whose session has since been trashed can never
// finish — the extractor discards its output on the next visit — and an
// explicit activation runs no retraction pass first, so counting it would
// leave activation blocked until an unrelated scheduled pass happens to
// clean the row up.
func TestActivateExtractGenerationIgnoresIneligibleUnfinishedCoverage(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	seedExtractCandidate(t, d, "sess-trashed", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-1", "e-a")
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-trashed", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET deleted_at = '2026-01-01T00:00:00.000Z' " +
			"WHERE id = 'sess-trashed'")
	require.NoError(t, err)

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	states := generationStates(t, d)
	assert.Equal(t, ExtractGenerationActive, states["fp-a"],
		"an ineligible session's pending row must not block activation")
	entry, err := d.GetRecallEntry(ctx, "e-a")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, "accepted", entry.Status)
}

// TestActivateExtractGenerationResetsTransientlyIneligibleStagedOutput
// pins promotion against sessions in transient flux: a failed session that
// reopened or lost its scan stamp passes the hard-ineligibility screen, so
// its staged partial output would start serving even though the session is
// no longer approvable. Promotion must skip it AND clear its staged output
// and progress row — entries left archived with a surviving progress row
// would never be promoted or rediscovered once the generation is active.
func TestActivateExtractGenerationResetsTransientlyIneligibleStagedOutput(
	t *testing.T,
) {
	cases := map[string]string{
		"scan stamp cleared": "UPDATE sessions SET secrets_rules_version " +
			"= '' WHERE id = 'sess-flux'",
		"session reopened": "UPDATE sessions SET ended_at = NULL " +
			"WHERE id = 'sess-flux'",
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
				Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
			})
			require.NoError(t, err)
			seedExtractCandidate(t, d, "sess-ok", 2*time.Hour, nil)
			seedExtractCandidate(t, d, "sess-flux", 2*time.Hour, nil)
			seedServableExtractEntry(t, d, "fp-a", "sess-ok", "e-ok")
			seedServableExtractEntry(t, d, "fp-a", "sess-flux", "e-flux")
			time.Sleep(2 * time.Millisecond)
			_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
				SessionID: "sess-ok", Fingerprint: "fp-a",
				ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
			})
			require.NoError(t, err)
			_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
				SessionID: "sess-flux", Fingerprint: "fp-a",
				ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
			})
			require.NoError(t, err)
			_, err = d.getWriter().Exec(
				"UPDATE recall_extract_progress SET state = 'failed' " +
					"WHERE session_id = 'sess-flux'")
			require.NoError(t, err)
			_, err = d.getWriter().Exec(mutate)
			require.NoError(t, err)

			require.NoError(t, d.ActivateExtractGeneration(
				ctx, "fp-a", []string{"rules-v1"}, time.Now()))
			okEntry, err := d.GetRecallEntry(ctx, "e-ok")
			require.NoError(t, err)
			require.NotNil(t, okEntry)
			assert.Equal(t, "accepted", okEntry.Status)

			fluxEntry, err := d.GetRecallEntry(ctx, "e-flux")
			require.NoError(t, err)
			assert.Nil(t, fluxEntry,
				"staged output of a session in transient flux must be "+
					"deleted, not promoted or stranded archived")
			_, found, err := d.ExtractProgress(ctx, "sess-flux", "fp-a")
			require.NoError(t, err)
			assert.False(t, found,
				"the progress row must go so the session is rediscovered "+
					"and re-extracted once it settles")
		})
	}
}

// TestActivateExtractGenerationResetsTransientlyIneligibleUnfinishedCoverage
// pins that pending rows follow the same transient-flux contract as failed
// ones: a session that reopened or lost its scan stamp is excluded from
// candidate selection, reconciliation clears only hard-ineligible rows, and
// nothing upstream can ever finish its extraction — counting it as
// unfinished coverage would block activation until the session happens to
// settle, possibly forever. The cleanup deletes the row and its staged
// output so the session is rediscovered once it settles.
func TestActivateExtractGenerationResetsTransientlyIneligibleUnfinishedCoverage(
	t *testing.T,
) {
	cases := map[string]string{
		"scan stamp cleared": "UPDATE sessions SET secrets_rules_version " +
			"= '' WHERE id = 'sess-flux'",
		"session reopened": "UPDATE sessions SET ended_at = NULL " +
			"WHERE id = 'sess-flux'",
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
				Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
			})
			require.NoError(t, err)
			seedCoveredExtractSession(t, d, "sess-ok", "fp-a")
			seedServableExtractEntry(t, d, "fp-a", "sess-ok", "e-ok")
			seedExtractCandidate(t, d, "sess-flux", 2*time.Hour, nil)
			seedServableExtractEntry(t, d, "fp-a", "sess-flux", "e-flux")
			time.Sleep(2 * time.Millisecond)
			_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
				SessionID: "sess-flux", Fingerprint: "fp-a",
				ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
			})
			require.NoError(t, err)
			_, err = d.getWriter().Exec(mutate)
			require.NoError(t, err)

			require.NoError(t, d.ActivateExtractGeneration(
				ctx, "fp-a", []string{"rules-v1"}, time.Now()))
			okEntry, err := d.GetRecallEntry(ctx, "e-ok")
			require.NoError(t, err)
			require.NotNil(t, okEntry)
			assert.Equal(t, "accepted", okEntry.Status)

			fluxEntry, err := d.GetRecallEntry(ctx, "e-flux")
			require.NoError(t, err)
			assert.Nil(t, fluxEntry,
				"staged output of an unfinishable pending row must be "+
					"deleted, not promoted or stranded archived")
			_, found, err := d.ExtractProgress(ctx, "sess-flux", "fp-a")
			require.NoError(t, err)
			assert.False(t, found,
				"the pending row must go so the session is rediscovered "+
					"and re-extracted once it settles")
		})
	}
}

// TestActivateExtractGenerationClearsHardIneligibleStagedOutput pins that
// activation deletes — not merely skips — the staged output and progress
// of hard-ineligible sessions: leaving them archived relies on a later
// retraction pass, and a session restored before that pass runs keeps its
// progress row (blocking rediscovery) while nothing ever promotes its
// archived entries under the now-active generation.
func TestActivateExtractGenerationClearsHardIneligibleStagedOutput(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-ok", "fp-a")
	seedServableExtractEntry(t, d, "fp-a", "sess-ok", "e-ok")
	seedCoveredExtractSession(t, d, "sess-gone", "fp-a")
	seedServableExtractEntry(t, d, "fp-a", "sess-gone", "e-gone")
	require.NoError(t, d.SoftDeleteSession("sess-gone"))

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	okEntry, err := d.GetRecallEntry(ctx, "e-ok")
	require.NoError(t, err)
	require.NotNil(t, okEntry)
	assert.Equal(t, "accepted", okEntry.Status)

	goneEntry, err := d.GetRecallEntry(ctx, "e-gone")
	require.NoError(t, err)
	assert.Nil(t, goneEntry,
		"a trashed session's staged output must be deleted at activation; "+
			"archived-until-retraction strands it if the session is "+
			"restored first")
	_, found, err := d.ExtractProgress(ctx, "sess-gone", "fp-a")
	require.NoError(t, err)
	assert.False(t, found,
		"the progress row must go so a restored session is rediscovered")
}

// TestActivateExtractGenerationRefusesDriftedDoneCoverage pins the in-tx
// staleness gates: a transcript write since the coverage stamp, or a scan
// stamp no longer current, means the staged entries describe a transcript
// state that was never re-approved — such coverage must not be promoted
// even though it never re-enters the caller's backlog probe.
func TestActivateExtractGenerationRefusesDriftedDoneCoverage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-1", "e-a")
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v2"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked,
		"a session scanned under a superseded rules version is uncovered")

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, d.BumpLocalModifiedAt("sess-1"))
	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked,
		"a transcript write after the coverage stamp is uncovered work")
	assert.Equal(t, ExtractGenerationBuilding, generationStates(t, d)["fp-a"])

	// Re-covering the session (a revisit advancing the stamp past the
	// write) unblocks activation.
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t,
		d.ActivateExtractGeneration(
			ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	assert.Equal(t, ExtractGenerationActive, generationStates(t, d)["fp-a"])
}

// TestActivateExtractGenerationRefusesStaleFailedPartialCoverage pins that
// the stale-coverage gate also covers failed rows holding staged output: a
// partially extracted session keeps its staged entries behind the failure
// backoff, and a session write after the coverage stamp — a content change,
// or a remap to another project, cwd, or branch — makes those entries
// stale. Promoting them would serve context the retry's refresh has not
// seen yet.
func TestActivateExtractGenerationRefusesStaleFailedPartialCoverage(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-ok", "fp-a")
	seedServableExtractEntry(t, d, "fp-a", "sess-ok", "e-ok")
	seedExtractCandidate(t, d, "sess-fail", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-fail", "e-fail")
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-fail", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-fail", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 0, LastError: "unit failed",
	}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, d.BumpLocalModifiedAt("sess-fail"))

	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked,
		"staged output of a failed row written after its stamp must not promote")
	assert.Equal(t, ExtractGenerationBuilding, generationStates(t, d)["fp-a"])
	entry, err := d.GetRecallEntry(ctx, "e-fail")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, "archived", entry.Status,
		"stale staged output must stay archived behind the blocked activation")
}

// TestActivateExtractGenerationAllowsFreshFailedCoverage pins the scoping
// of the failed-row stale gate: a failed row with no staged output has
// nothing stale to promote no matter when its session was written, and a
// failed partial row whose stamp still covers the last write promotes its
// staged output as designed — the retry tops the corpus up later.
func TestActivateExtractGenerationAllowsFreshFailedCoverage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-ok", "fp-a")
	seedServableExtractEntry(t, d, "fp-a", "sess-ok", "e-ok")
	// A failed partial row with staged output and a current stamp.
	seedExtractCandidate(t, d, "sess-partial", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-partial", "e-partial")
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-partial", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-partial", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 0, LastError: "unit failed",
	}))
	// A failed row with no staged output, written after its stamp.
	seedExtractCandidate(t, d, "sess-bare", 2*time.Hour, nil)
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-bare", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "sess-bare", Fingerprint: "fp-a",
		ExpectedDigest: "dg", ExpectedCursor: 0, LastError: "unit failed",
	}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, d.BumpLocalModifiedAt("sess-bare"))

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	entry, err := d.GetRecallEntry(ctx, "e-partial")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, "accepted", entry.Status,
		"a fresh failed partial row's staged output promotes as designed")
}

// TestActivateExtractGenerationSkipsSupersededEntries pins that promotion
// leaves a superseded generated entry archived: a reviewed replacement
// archived the obsolete entry with a superseded_by link, and flipping every
// archived unreviewed_auto entry back to accepted would serve both the
// obsolete entry and its replacement.
func TestActivateExtractGenerationSkipsSupersededEntries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedCoveredExtractSession(t, d, "sess-1", "fp-a")
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		{
			ID: "e-live", Type: "fact", ReviewState: "unreviewed_auto",
			Status: "archived", Title: "t", Body: "b",
			SourceSessionID: "sess-1", SourceRunID: "fp-a", ProvenanceOK: true,
		},
		{
			ID: "e-super", Type: "fact", ReviewState: "unreviewed_auto",
			Status: "archived", Title: "old", Body: "old",
			SourceSessionID: "sess-1", SourceRunID: "fp-a", ProvenanceOK: true,
			SupersededByEntryID: "e-repl",
		},
	})
	require.NoError(t, err)

	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))

	live, err := d.GetRecallEntry(ctx, "e-live")
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, "accepted", live.Status,
		"a live staged entry must promote")
	super, err := d.GetRecallEntry(ctx, "e-super")
	require.NoError(t, err)
	require.NotNil(t, super)
	assert.Equal(t, "archived", super.Status,
		"a superseded entry must not be promoted back into service")
}

// TestActivateExtractGenerationRefusesEmptyPromotion pins the replacement
// guarantee: when every staged entry's session lost eligibility after the
// caller's checks, activation must abort instead of retiring the served
// corpus with nothing servable to replace it.
func TestActivateExtractGenerationRefusesEmptyPromotion(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, fp := range []string{"fp-old", "fp-a"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	seedExtractCandidate(t, d, "sess-old", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-old", "sess-old", "e-old")
	time.Sleep(2 * time.Millisecond)
	_, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-old", Fingerprint: "fp-old",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-old", []string{"rules-v1"}, time.Now()))
	seedExtractCandidate(t, d, "sess-gone", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-gone", "e-gone")
	require.NoError(t, d.SoftDeleteSession("sess-gone"))

	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked)
	states := generationStates(t, d)
	assert.Equal(t, ExtractGenerationActive, states["fp-old"],
		"an activation with nothing servable must not retire the served corpus")
	assert.Equal(t, ExtractGenerationBuilding, states["fp-a"])
	old, err := d.GetRecallEntry(ctx, "e-old")
	require.NoError(t, err)
	require.NotNil(t, old)
	assert.Equal(t, "accepted", old.Status,
		"the served generation's entries must keep serving")
}

// TestCommitExtractedUnitRefusesEndedAtDrift pins ended_at as part of the
// commit guard: eligibility treats it as state (a session that resumes is
// no longer approved for extraction), and a bare session-row update can
// re-date or clear it without moving any other guarded field.
func TestCommitExtractedUnitRefusesEndedAtDrift(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	commit := ExtractUnitCommit{
		SessionID: "sess-1", Fingerprint: "fp-a", Digest: "dg", Cursor: 0,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session.MessageCount,
		TranscriptRevision: session.TranscriptRevision,
		LocalModifiedAt:    session.LocalModifiedAt,
		EndedAt:            session.EndedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-1", "sess-1", 0, 1)},
	}

	// The session was re-dated (a resume that ended again) between the
	// caller's read and the commit.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET ended_at = '2099-01-01T00:00:00.000Z' " +
			"WHERE id = 'sess-1'")
	require.NoError(t, err)
	_, err = d.CommitExtractedUnit(ctx, commit)
	require.ErrorIs(t, err, ErrExtractSessionDrifted)
	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	assert.Nil(t, entry, "an ended_at drift must roll back its entries")

	// The session was reopened outright (ended_at cleared).
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET ended_at = NULL WHERE id = 'sess-1'")
	require.NoError(t, err)
	_, err = d.CommitExtractedUnit(ctx, commit)
	require.ErrorIs(t, err, ErrExtractSessionDrifted)
}

// TestCommitExtractedUnitRefusesCursorMismatch pins the cursor guard to the
// exact cursor the unit was derived from: after a same-digest reopen resets
// the row to zero (its entries were just deleted), a stale worker's commit
// for a later unit must not fast-forward the cursor past units that no
// longer have output.
func TestCommitExtractedUnitRefusesCursorMismatch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)

	commit := ExtractUnitCommit{
		SessionID: "sess-1", Fingerprint: "fp-a", Digest: "dg", Cursor: 1,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session.MessageCount,
		TranscriptRevision: session.TranscriptRevision,
		LocalModifiedAt:    session.LocalModifiedAt,
		EndedAt:            session.EndedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-1", "sess-1", 2, 2)},
	}
	_, err = d.CommitExtractedUnit(ctx, commit)
	require.ErrorIs(t, err, ErrStaleExtractProgress,
		"the stored cursor is 0: a commit derived from cursor 1 is stale")
	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	assert.Nil(t, entry, "a stale-cursor commit must roll back its entries")
	progress, _, err := d.ExtractProgress(ctx, "sess-1", "fp-a")
	require.NoError(t, err)
	assert.Zero(t, progress.UnitCursor)
}

// TestActivateExtractGenerationRefusesUncoveredEligibleSessions pins the
// in-tx discovery gate: an eligible session with no progress row (for
// example after a single-session run) is uncovered work, and activating
// around it would retire the served corpus for an incomplete generation.
func TestActivateExtractGenerationRefusesUncoveredEligibleSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	seedExtractCandidate(t, d, "sess-1", 2*time.Hour, nil)
	seedServableExtractEntry(t, d, "fp-a", "sess-1", "e-a")
	// Seeding bumps local_modified_at at millisecond precision; settle so
	// the coverage stamp below compares as after the seed writes.
	time.Sleep(2 * time.Millisecond)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	seedExtractCandidate(t, d, "sess-2", 2*time.Hour, nil)
	time.Sleep(2 * time.Millisecond)

	err = d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now())
	require.ErrorIs(t, err, ErrExtractActivationBlocked,
		"an eligible session with no progress row is uncovered work")
	assert.Equal(t, ExtractGenerationBuilding, generationStates(t, d)["fp-a"])

	// Covering the session unblocks activation.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-2", Fingerprint: "fp-a",
		ContentDigest: "dg2", UnitsTotal: 0, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(
		ctx, "fp-a", []string{"rules-v1"}, time.Now()))
	assert.Equal(t, ExtractGenerationActive, generationStates(t, d)["fp-a"])
}

// TestUpsertExtractProgressDigestChangeRemovesEntriesAtomically pins the
// rebuild reset as one transaction: a digest change deletes the session's
// machine entries in the same write that resets the row, so no failure
// window can leave a done row claiming coverage for entries that are gone.
func TestUpsertExtractProgressDigestChangeRemovesEntriesAtomically(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	machineEntry := func(id, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", ReviewState: reviewState,
			Status: "archived", Title: "t", Body: "b",
			SourceSessionID: "sess-1", SourceRunID: "fp-a",
			ProvenanceOK: true,
		}
	}
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		machineEntry("e-1", "unreviewed_auto"),
		machineEntry("e-human", "human_reviewed"),
	})
	require.NoError(t, err)

	// The first visit stores the row; existing entries are untouched.
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-1", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	require.NotNil(t, entry, "a first visit must not delete entries")

	// A digest change deletes the machine entries and resets the row in
	// one transaction. Human-touched entries stay.
	progress, err := d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-2", UnitsTotal: 2, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressPending, progress.State)
	assert.Zero(t, progress.UnitCursor)
	entry, err = d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	assert.Nil(t, entry,
		"a digest change must delete the previous derivation's entries")
	human, err := d.GetRecallEntry(ctx, "e-human")
	require.NoError(t, err)
	require.NotNil(t, human, "human-touched entries are never machine-deleted")

	// A refused progress write rolls the entry delete back with it.
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		machineEntry("e-2", "unreviewed_auto"),
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`CREATE TRIGGER block_progress_write
		BEFORE UPDATE ON recall_extract_progress
		BEGIN SELECT RAISE(ABORT, 'progress write blocked'); END`)
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg-3", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.Error(t, err)
	_, execErr := d.getWriter().Exec("DROP TRIGGER block_progress_write")
	require.NoError(t, execErr)
	entry, err = d.GetRecallEntry(ctx, "e-2")
	require.NoError(t, err)
	require.NotNil(t, entry,
		"a failed progress reset must not leave the entries deleted")
}

// TestRefreshExtractedSessionCoverageRestoresProvenance pins the revisit repair:
// evidence digests cover rows the units digest ignores, so the reconciler
// can revoke provenance without the extraction output changing — a rebind
// against the current transcript must re-stamp the evidence and restore
// the entry.
func TestRefreshExtractedSessionCoverageRestoresProvenance(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	session := seedCommitUnitSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, ExtractProgressUpsert{
		SessionID: "sess-1", Fingerprint: "fp-a",
		ContentDigest: "dg", UnitsTotal: 1, StampedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = d.CommitExtractedUnit(ctx, ExtractUnitCommit{
		SessionID: "sess-1", Fingerprint: "fp-a", Digest: "dg", Cursor: 0,
		ScanVersions:       []string{"rules-v1"},
		MessageCount:       session.MessageCount,
		TranscriptRevision: session.TranscriptRevision,
		LocalModifiedAt:    session.LocalModifiedAt,
		EndedAt:            session.EndedAt,
		Entries:            []RecallEntry{commitUnitEntry("e-1", "sess-1", 0, 1)},
	})
	require.NoError(t, err)

	// The reconciler revoked provenance after an ignored-row change made
	// the stored evidence digest stale.
	_, err = d.getWriter().Exec(
		"UPDATE recall_entries SET provenance_ok = 0 WHERE id = 'e-1'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(
		"UPDATE recall_evidence SET content_digest = 'stale' " +
			"WHERE entry_id = 'e-1'")
	require.NoError(t, err)

	// The snapshot still matches the stored session, so the refresh
	// rebinds and restores in one guarded transaction.
	_, err = d.RefreshExtractedSessionCoverage(ctx, ExtractCoverageRefresh{
		Fingerprint:  "fp-a",
		Digest:       "dg",
		StampedAt:    time.Now(),
		ScanVersions: []string{"rules-v1"},
		Session:      session,
	})
	require.NoError(t, err)
	entry, err := d.GetRecallEntry(ctx, "e-1")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.True(t, entry.ProvenanceOK,
		"a rebind against the current transcript must restore provenance")
	require.Len(t, entry.Evidence, 1)
	evidence := entry.Evidence[0]
	assert.NotEmpty(t, evidence.ContentDigest)
	assert.NotEqual(t, "stale", evidence.ContentDigest)
	assert.Equal(t, "sess-1-uuid-0", evidence.MessageStartSourceUUID)
	assert.Equal(t, "sess-1-uuid-1", evidence.MessageEndSourceUUID)
}
