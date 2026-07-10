package db

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecallQueryEventRecordsRankedExposureSnapshot(t *testing.T) {
	d := testDB(t)
	event := RecallQueryEvent{
		QueryID:            "query-1",
		Query:              "recover wrong cwd",
		Surface:            "brief",
		FiltersJSON:        `{"project":"agentsview","agent":"codex","limit":3}`,
		TrustedOnly:        true,
		ScorePolicyVersion: RecallLexicalScorePolicyVersion,
		ResultCount:        3,
		PackedCount:        2,
		TopScore:           9.75,
		MissReason:         "context_empty",
		Exposures: []RecallQueryExposure{
			{Rank: 1, EntryID: "m1", Score: 9.75, Packed: true},
			{Rank: 2, EntryID: "m2", Score: 6.5, Packed: false},
			{Rank: 3, EntryID: "m3", Score: 4.25, Packed: true},
		},
	}

	id, err := d.RecordRecallQueryEvent(context.Background(), event)

	require.NoError(t, err)
	assert.Equal(t, "query-1", id)
	got, err := d.GetRecallQueryEvent(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "query-1", got.QueryID)
	assert.Equal(t, "recover wrong cwd", got.Query)
	assert.Equal(t, "brief", got.Surface)
	assert.Equal(t, event.FiltersJSON, got.FiltersJSON)
	assert.True(t, got.TrustedOnly)
	assert.Equal(t, RecallLexicalScorePolicyVersion, got.ScorePolicyVersion)
	assert.Equal(t, 3, got.ResultCount)
	assert.Equal(t, 2, got.PackedCount)
	assert.Equal(t, 9.75, got.TopScore)
	assert.Equal(t, "context_empty", got.MissReason)
	assert.NotEmpty(t, got.CreatedAt)
	require.Len(t, got.Exposures, 3)
	for i, want := range event.Exposures {
		assert.Equal(t, id, got.Exposures[i].QueryID)
		assert.Equal(t, want.Rank, got.Exposures[i].Rank)
		assert.Equal(t, want.EntryID, got.Exposures[i].EntryID)
		assert.Equal(t, want.Score, got.Exposures[i].Score)
		assert.Equal(t, want.Packed, got.Exposures[i].Packed)
	}
}

func TestRecallQueryEventGeneratesOpaqueID(t *testing.T) {
	d := testDB(t)

	first, err := d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		Query:   "first query",
		Surface: "query",
	})
	require.NoError(t, err)
	second, err := d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		Query:   "second query",
		Surface: "query",
	})
	require.NoError(t, err)

	assert.Len(t, first, 36)
	assert.Len(t, second, 36)
	assert.NotEqual(t, first, second)
}

func TestRecallQueryEventDuplicateExposureRankRollsBackAtomically(t *testing.T) {
	d := testDB(t)

	_, err := d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		QueryID:     "query-duplicate-rank",
		Query:       "atomic query",
		Surface:     "query",
		ResultCount: 2,
		Exposures: []RecallQueryExposure{
			{Rank: 1, EntryID: "m1", Score: 4},
			{Rank: 1, EntryID: "m2", Score: 3},
		},
	})

	require.Error(t, err)
	got, getErr := d.GetRecallQueryEvent(
		context.Background(), "query-duplicate-rank",
	)
	require.NoError(t, getErr)
	assert.Nil(t, got)
	var exposureCount int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COUNT(*) FROM recall_query_exposures
		WHERE query_id = ?`,
		"query-duplicate-rank",
	).Scan(&exposureCount))
	assert.Zero(t, exposureCount)
}

func TestRecallQueryEventPersistsLargeRankedExposureSnapshot(t *testing.T) {
	d := testDB(t)
	exposures := make([]RecallQueryExposure, 205)
	for i := range exposures {
		rank := i + 1
		exposures[i] = RecallQueryExposure{
			QueryID: "ignored-exposure-query-id",
			Rank:    rank,
			EntryID: fmt.Sprintf("entry-%03d", rank),
			Score:   float64(rank) + 0.25,
			Packed:  rank%2 == 0,
		}
	}

	id, err := d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		QueryID:     "query-large-snapshot",
		Query:       "large ranked snapshot",
		Surface:     "calibration",
		ResultCount: len(exposures),
		PackedCount: 102,
		TopScore:    205.25,
		Exposures:   exposures,
	})
	require.NoError(t, err)

	got, err := d.GetRecallQueryEvent(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Exposures, 205)
	for i, exposure := range got.Exposures {
		assert.Equal(t, i+1, exposure.Rank)
	}

	boundaries := []struct {
		index   int
		rank    int
		entryID string
		score   float64
		packed  bool
	}{
		{index: 0, rank: 1, entryID: "entry-001", score: 1.25},
		{index: 99, rank: 100, entryID: "entry-100", score: 100.25, packed: true},
		{index: 100, rank: 101, entryID: "entry-101", score: 101.25},
		{index: 199, rank: 200, entryID: "entry-200", score: 200.25, packed: true},
		{index: 200, rank: 201, entryID: "entry-201", score: 201.25},
		{index: 204, rank: 205, entryID: "entry-205", score: 205.25},
	}
	for _, boundary := range boundaries {
		exposure := got.Exposures[boundary.index]
		assert.Equal(t, id, exposure.QueryID)
		assert.Equal(t, boundary.rank, exposure.Rank)
		assert.Equal(t, boundary.entryID, exposure.EntryID)
		assert.Equal(t, boundary.score, exposure.Score)
		assert.Equal(t, boundary.packed, exposure.Packed)
	}
}

func TestRecallQueryEventLateDuplicateRankRollsBackAtomically(t *testing.T) {
	d := testDB(t)
	exposures := make([]RecallQueryExposure, 101)
	for i := range exposures {
		rank := i + 1
		exposures[i] = RecallQueryExposure{
			Rank: rank, EntryID: fmt.Sprintf("entry-%03d", rank),
			Score: float64(rank),
		}
	}
	exposures[100].Rank = 100

	_, err := d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		QueryID:     "query-late-duplicate-rank",
		Query:       "atomic query after many exposures",
		Surface:     "query",
		ResultCount: len(exposures),
		Exposures:   exposures,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "exposure ranks 100 through 100")

	got, getErr := d.GetRecallQueryEvent(
		context.Background(), "query-late-duplicate-rank",
	)
	require.NoError(t, getErr)
	assert.Nil(t, got)
	var exposureCount int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COUNT(*) FROM recall_query_exposures
		WHERE query_id = ?`,
		"query-late-duplicate-rank",
	).Scan(&exposureCount))
	assert.Zero(t, exposureCount)
}

func TestRecallQueryEventSurvivesRecallAndSessionDeletion(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "agentsview")
	_, err := d.InsertRecallEntry(RecallEntry{
		ID:              "m1",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Durable exposure",
		Body:            "The measurement outlives its source row.",
		SourceSessionID: "s1",
	})
	require.NoError(t, err)
	_, err = d.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		QueryID:     "query-durable",
		Query:       "durable query",
		Surface:     "query",
		ResultCount: 1,
		PackedCount: 1,
		TopScore:    8,
		Exposures: []RecallQueryExposure{{
			Rank: 1, EntryID: "m1", Score: 8, Packed: true,
		}},
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`DELETE FROM sessions WHERE id = 's1'`)
	require.NoError(t, err)

	got, err := d.GetRecallQueryEvent(context.Background(), "query-durable")

	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Exposures, 1)
	assert.Equal(t, "m1", got.Exposures[0].EntryID)
}

func TestRecallQueryEventSurvivesFullResyncWithoutExposedEntry(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old-query-events.db")
	src, err := Open(srcPath)
	require.NoError(t, err)
	_, err = src.RecordRecallQueryEvent(context.Background(), RecallQueryEvent{
		QueryID:     "query-orphan-exposure",
		Query:       "missing entry query",
		Surface:     "calibration",
		FiltersJSON: `{"project":"missing"}`,
		TrustedOnly: true,
		ResultCount: 1,
		TopScore:    7.25,
		MissReason:  "context_empty",
		Exposures: []RecallQueryExposure{{
			Rank: 1, EntryID: "entry-not-in-new-db", Score: 7.25,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, src.Close())

	dstPath := filepath.Join(dir, "new-query-events.db")
	dst, err := Open(dstPath)
	require.NoError(t, err)
	defer dst.Close()
	require.NoError(t, dst.CopyRecallEntriesFrom(srcPath))

	got, err := dst.GetRecallQueryEvent(
		context.Background(), "query-orphan-exposure",
	)

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "calibration", got.Surface)
	assert.Equal(t, "context_empty", got.MissReason)
	require.Len(t, got.Exposures, 1)
	assert.Equal(t, "entry-not-in-new-db", got.Exposures[0].EntryID)
}

func TestRecallQueryEventCopyToleratesArchiveWithoutLedger(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old-without-ledger.db")
	src, err := Open(srcPath)
	require.NoError(t, err)
	require.NoError(t, src.Close())
	execRawSQLite(t, srcPath, "DROP TABLE recall_query_exposures")
	execRawSQLite(t, srcPath, "DROP TABLE recall_query_events")

	dstPath := filepath.Join(dir, "new-with-ledger.db")
	dst, err := Open(dstPath)
	require.NoError(t, err)
	defer dst.Close()

	assert.NoError(t, dst.CopyRecallEntriesFrom(srcPath))
}
