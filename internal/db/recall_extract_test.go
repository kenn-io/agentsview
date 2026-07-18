package db

import (
	"context"
	"database/sql"
	"path/filepath"
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

	for _, fp := range []string{"fp-a", "fp-b"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-a"))
	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-b"))

	generations, err := d.ExtractGenerations(ctx)
	require.NoError(t, err)
	states := map[string]string{}
	for _, gen := range generations {
		states[gen.Fingerprint] = gen.State
	}
	assert.Equal(t, ExtractGenerationRetired, states["fp-a"])
	assert.Equal(t, ExtractGenerationActive, states["fp-b"])

	err = d.ActivateExtractGeneration(ctx, "fp-missing")
	assert.Error(t, err, "unknown fingerprint must refuse")
}

func TestExtractGenerationRetireActiveRequiresForce(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-a"))

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

	progress, err := d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
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

	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 4))

	same, err := d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, same.State, "same digest keeps progress")
	assert.Equal(t, 4, same.UnitCursor)

	grown, err := d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-2", 6)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
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

	_, err = d.UpsertExtractProgress(ctx, "sess-missing", "fp-a", "digest-1", 4)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-2", 6)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-2", 6)
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
	seedExtractSession(t, src, "sess-1")
	seedExtractSession(t, src, "sess-gone")
	_, err = src.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
		ParamsJSON: `{"max_window_chars":50000}`,
	})
	require.NoError(t, err)
	require.NoError(t, src.ActivateExtractGeneration(ctx, "fp-a"))
	_, err = src.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
	require.NoError(t, err)
	require.NoError(t, src.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 2))
	_, err = src.UpsertExtractProgress(ctx, "sess-gone", "fp-a", "digest-9", 3)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 2)
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

func TestExtractMutationsWaitForDBMutex(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 2)
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
			return d.ActivateExtractGeneration(ctx, "fp-a")
		},
		"RetireExtractGeneration": func() error {
			return d.RetireExtractGeneration(ctx, "fp-a", true)
		},
		"UpsertExtractProgress": func() error {
			_, err := d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 2)
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

	progress, err := d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 0)
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a session with no units has nothing left to extract")
	assert.Equal(t, 0, progress.UnitCursor)

	progress, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-2", 0)
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a digest reset to zero units must also complete immediately")

	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-3", -1)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 10)
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-1", "fp-a", "digest-1", 7))

	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-2", 4)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-1", "fp-a", "digest-1", 4)
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
