package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecallQueryEventRecordsRankedExposureSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	id, err := d.RecordRecallQueryEvent(t.Context(), event)

	require.NoError(err)
	assert.Equal("query-1", id)
	got, err := d.GetRecallQueryEvent(t.Context(), id)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("query-1", got.QueryID)
	assert.Equal("recover wrong cwd", got.Query)
	assert.Equal("brief", got.Surface)
	assert.Equal(event.FiltersJSON, got.FiltersJSON)
	assert.True(got.TrustedOnly)
	assert.Equal(RecallLexicalScorePolicyVersion, got.ScorePolicyVersion)
	assert.Equal(3, got.ResultCount)
	assert.Equal(2, got.PackedCount)
	assert.Equal(9.75, got.TopScore)
	assert.Equal("context_empty", got.MissReason)
	assert.NotEmpty(got.CreatedAt)
	require.Len(got.Exposures, 3)
	for i, want := range event.Exposures {
		assert.Equal(id, got.Exposures[i].QueryID)
		assert.Equal(want.Rank, got.Exposures[i].Rank)
		assert.Equal(want.EntryID, got.Exposures[i].EntryID)
		assert.Equal(want.Score, got.Exposures[i].Score)
		assert.Equal(want.Packed, got.Exposures[i].Packed)
	}
}

func TestRecallQueryEventGeneratesOpaqueID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	first, err := d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
		Query:   "first query",
		Surface: "query",
	})
	require.NoError(err)
	second, err := d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
		Query:   "second query",
		Surface: "query",
	})
	require.NoError(err)

	assert.Len(first, 36)
	assert.Len(second, 36)
	assert.NotEqual(first, second)
}

func TestRecallQueryEventDuplicateExposureRankRollsBackAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	_, err := d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
		QueryID:     "query-duplicate-rank",
		Query:       "atomic query",
		Surface:     "query",
		ResultCount: 2,
		Exposures: []RecallQueryExposure{
			{Rank: 1, EntryID: "m1", Score: 4},
			{Rank: 1, EntryID: "m2", Score: 3},
		},
	})

	require.Error(err)
	got, getErr := d.GetRecallQueryEvent(
		t.Context(), "query-duplicate-rank",
	)
	require.NoError(getErr)
	assert.Nil(got)
	var exposureCount int
	require.NoError(d.getReader().QueryRow(`
		SELECT COUNT(*) FROM recall_query_exposures
		WHERE query_id = ?`,
		"query-duplicate-rank",
	).Scan(&exposureCount))
	assert.Zero(exposureCount)
}

func TestRecallQueryEventPersistsLargeRankedExposureSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	id, err := d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
		QueryID:     "query-large-snapshot",
		Query:       "large ranked snapshot",
		Surface:     "calibration",
		ResultCount: len(exposures),
		PackedCount: 102,
		TopScore:    205.25,
		Exposures:   exposures,
	})
	require.NoError(err)

	got, err := d.GetRecallQueryEvent(t.Context(), id)
	require.NoError(err)
	require.NotNil(got)
	require.Len(got.Exposures, 205)
	for i, exposure := range got.Exposures {
		assert.Equal(i+1, exposure.Rank)
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
		assert.Equal(id, exposure.QueryID)
		assert.Equal(boundary.rank, exposure.Rank)
		assert.Equal(boundary.entryID, exposure.EntryID)
		assert.Equal(boundary.score, exposure.Score)
		assert.Equal(boundary.packed, exposure.Packed)
	}
}

func TestRecallQueryEventLateDuplicateRankRollsBackAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	_, err := d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
		QueryID:     "query-late-duplicate-rank",
		Query:       "atomic query after many exposures",
		Surface:     "query",
		ResultCount: len(exposures),
		Exposures:   exposures,
	})
	require.Error(err)
	assert.ErrorContains(err, "exposure ranks 100 through 100")

	got, getErr := d.GetRecallQueryEvent(
		t.Context(), "query-late-duplicate-rank",
	)
	require.NoError(getErr)
	assert.Nil(got)
	var exposureCount int
	require.NoError(d.getReader().QueryRow(`
		SELECT COUNT(*) FROM recall_query_exposures
		WHERE query_id = ?`,
		"query-late-duplicate-rank",
	).Scan(&exposureCount))
	assert.Zero(exposureCount)
}

func TestRecallQueryEventSurvivesRecallAndSessionDeletion(t *testing.T) {
	require := require.New(t)

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
	require.NoError(err)
	_, err = d.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
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
	require.NoError(err)
	_, err = d.getWriter().Exec(`DELETE FROM sessions WHERE id = 's1'`)
	require.NoError(err)

	got, err := d.GetRecallQueryEvent(t.Context(), "query-durable")

	require.NoError(err)
	require.NotNil(got)
	require.Len(got.Exposures, 1)
	assert.Equal(t, "m1", got.Exposures[0].EntryID)
}

func TestRecallQueryEventSurvivesFullResyncWithoutExposedEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old-query-events.db")
	src, err := Open(srcPath)
	require.NoError(err)
	_, err = src.RecordRecallQueryEvent(t.Context(), RecallQueryEvent{
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
	require.NoError(err)
	require.NoError(src.Close())

	dstPath := filepath.Join(dir, "new-query-events.db")
	dst, err := Open(dstPath)
	require.NoError(err)
	defer dst.Close()
	require.NoError(dst.CopyRecallEntriesFrom(srcPath))

	got, err := dst.GetRecallQueryEvent(
		t.Context(), "query-orphan-exposure",
	)

	require.NoError(err)
	require.NotNil(got)
	assert.Equal("calibration", got.Surface)
	assert.Equal("context_empty", got.MissReason)
	require.Len(got.Exposures, 1)
	assert.Equal("entry-not-in-new-db", got.Exposures[0].EntryID)
}

func TestRecallQueryEventCopyToleratesArchiveWithoutLedger(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old-without-ledger.db")
	src, err := Open(srcPath)
	require.NoError(err)
	require.NoError(src.Close())
	execRawSQLite(t, srcPath, "DROP TABLE recall_query_exposures")
	execRawSQLite(t, srcPath, "DROP TABLE recall_query_events")

	dstPath := filepath.Join(dir, "new-with-ledger.db")
	dst, err := Open(dstPath)
	require.NoError(err)
	defer dst.Close()

	assert.NoError(t, dst.CopyRecallEntriesFrom(srcPath))
}
