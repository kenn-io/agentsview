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
	_, err = d.UpsertExtractProgress(ctx, "sess-done", "fp-a", "dg", 1)
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

func TestUpsertExtractProgressHealsEmptyContentStamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedExtractCandidate(t, d, "sess-copied", 2*time.Hour, nil)
	_, err := d.EnsureExtractGeneration(ctx, ExtractGeneration{
		Fingerprint: "fp-a", Model: "m", Segmenter: "turns-v1",
	})
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, "sess-copied", "fp-a", "dg", 1)
	require.NoError(t, err)
	require.NoError(t,
		d.AdvanceExtractCursor(ctx, "sess-copied", "fp-a", "dg", 1))

	// A row copied from a pre-stamp archive has an empty content stamp,
	// which matches every future full pass. A same-digest upsert must
	// re-stamp it — the digest was just re-derived from the live
	// transcript — so the row settles instead of being revisited forever.
	_, err = d.getWriter().Exec(
		"UPDATE recall_extract_progress SET content_stamped_at = '' " +
			"WHERE session_id = 'sess-copied'")
	require.NoError(t, err)

	progress, err := d.UpsertExtractProgress(ctx, "sess-copied", "fp-a", "dg", 1)
	require.NoError(t, err)
	assert.Equal(t, ExtractProgressDone, progress.State,
		"a same-digest upsert must not reset completed progress")

	var stamp string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content_stamped_at FROM recall_extract_progress "+
			"WHERE session_id = 'sess-copied'").Scan(&stamp))
	assert.NotEmpty(t, stamp,
		"an empty content stamp must heal on a same-digest upsert")
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
	_, err := d.UpsertExtractProgress(ctx, "sess-pending", "fp-a", "dg", 2)
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, "sess-partial", "fp-a", "dg", 2)
	require.NoError(t, err)
	require.NoError(t,
		d.AdvanceExtractCursor(ctx, "sess-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, "sess-done", "fp-a", "dg", 1)
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "sess-done", "fp-a", "dg", 1))
	for _, id := range []string{"sess-failed-fresh", "sess-failed-stale"} {
		_, err = d.UpsertExtractProgress(ctx, id, "fp-a", "dg", 2)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-new", "fp-b", "dg", 1)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-failed", "fp-a", "dg", 2)
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
	_, err := d.UpsertExtractProgress(ctx, "s-pending", "fp-a", "dg", 2)
	require.NoError(t, err)
	_, err = d.UpsertExtractProgress(ctx, "s-partial", "fp-a", "dg", 3)
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "s-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, "s-done", "fp-a", "dg", 1)
	require.NoError(t, err)
	require.NoError(t, d.AdvanceExtractCursor(ctx, "s-done", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, "s-failed", "fp-a", "dg", 4)
	require.NoError(t, err)
	require.NoError(t, d.MarkExtractProgressFailed(ctx, ExtractFailure{
		SessionID: "s-failed", Fingerprint: "fp-a",
		ExpectedDigest: "dg", LastError: "boom",
	}))
	// Rows under another generation must not leak into fp-a's stats.
	_, err = d.UpsertExtractProgress(ctx, "s-pending", "fp-b", "dg", 9)
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
	_, err = d.UpsertExtractProgress(ctx, "sess-partial", "fp-a", "dg", 2)
	require.NoError(t, err)
	require.NoError(t,
		d.AdvanceExtractCursor(ctx, "sess-partial", "fp-a", "dg", 1))
	_, err = d.UpsertExtractProgress(ctx, "sess-failed", "fp-a", "dg", 2)
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
