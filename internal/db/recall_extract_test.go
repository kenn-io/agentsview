package db

import (
	"context"
	"database/sql"
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
	seedExtractSession(t, src, "sess-1")
	seedExtractSession(t, src, "sess-gone")
	_, err = src.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
		ParamsJSON: `{"max_window_chars":50000}`,
	})
	require.NoError(t, err)
	require.NoError(t, src.ActivateExtractGeneration(ctx, "fp-a"))
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
		ReopenDone:     true,
	})
	require.ErrorIs(t, err, ErrStaleExtractProgress)

	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID:      "sess-1",
		Fingerprint:    "fp-a",
		ExpectedDigest: "digest-1",
		ExpectedCursor: 2,
		LastError:      "count mismatch",
		ReopenDone:     true,
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

func TestSyncExtractedEntryContextRefreshesGeneratedEntries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	seedExtractSession(t, d, "sess-2")

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
	_, err := d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-auto", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-reviewed", "sess-1", "fp-a", "human_reviewed"),
		entry("e-other-fp", "sess-1", "fp-b", "unreviewed_auto"),
		entry("e-other-sess", "sess-2", "fp-a", "unreviewed_auto"),
	})
	require.NoError(t, err)

	session := &Session{
		ID: "sess-1", Project: "proj-2", Cwd: "/new",
		GitBranch: "feature", Agent: "codex",
	}
	updated, err := d.SyncExtractedEntryContext(ctx, "fp-a", session)
	require.NoError(t, err)
	assert.Equal(t, 1, updated)

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

	updated, err = d.SyncExtractedEntryContext(ctx, "fp-a", session)
	require.NoError(t, err)
	assert.Zero(t, updated, "an already-synchronized corpus must be a no-op")
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
			return d.ActivateExtractGeneration(ctx, "fp-a")
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
	seedExtractSession(t, d, "sess-1")
	for _, fp := range []string{"fp-old", "fp-new"} {
		_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
			Fingerprint: fp, Model: "m", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
	}
	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-old"))

	entry := func(id, fp, status, reviewState string) RecallEntry {
		return RecallEntry{
			ID: id, Type: "fact", Status: status, ReviewState: reviewState,
			Title: "t", Body: "b",
			SourceSessionID: "sess-1", SourceRunID: fp,
		}
	}
	_, err := d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-old", "fp-old", "accepted", "unreviewed_auto"),
		entry("e-new-staged", "fp-new", "archived", "unreviewed_auto"),
		entry("e-reviewed", "fp-old", "accepted", "human_reviewed"),
	})
	require.NoError(t, err)

	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-new"))

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
	seedExtractSession(t, d, "sess-1")
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	require.NoError(t, d.ActivateExtractGeneration(ctx, "fp-a"))
	_, err = d.InsertExtractedRecallEntries(ctx, []RecallEntry{{
		ID: "e-1", Type: "fact", Status: "accepted",
		ReviewState: "unreviewed_auto", Title: "t", Body: "b",
		SourceSessionID: "sess-1", SourceRunID: "fp-a",
	}})
	require.NoError(t, err)

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

func TestDeleteExtractedRecallEntriesScopesToGenerationAndSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedExtractSession(t, d, "sess-1")
	seedExtractSession(t, d, "sess-2")

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
	_, err := d.InsertExtractedRecallEntries(ctx, []RecallEntry{
		entry("e-del-1", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-del-2", "sess-1", "fp-a", "unreviewed_auto"),
		entry("e-reviewed", "sess-1", "fp-a", "human_reviewed"),
		entry("e-other-fp", "sess-1", "fp-b", "unreviewed_auto"),
		entry("e-other-sess", "sess-2", "fp-a", "unreviewed_auto"),
	})
	require.NoError(t, err)

	deleted, err := d.DeleteExtractedRecallEntries(ctx, "fp-a", "sess-1")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)

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
