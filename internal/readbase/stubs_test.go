package readbase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStubEmptyResults(t *testing.T) {
	store := MirrorStubs{}
	insights, err := store.ListInsights(t.Context(), db.InsightFilter{})
	require.NoError(t, err)
	assert.NotNil(t, insights)
	assert.Empty(t, insights)

	insight, err := store.GetInsight(t.Context(), 1)
	require.NoError(t, err)
	assert.Nil(t, insight)
	insight, err = store.GetCachedInsight(t.Context(), "missing")
	require.NoError(t, err)
	assert.Nil(t, insight)
}

func TestUnsupportedStubs(t *testing.T) {
	store := MirrorStubs{}
	ctx := t.Context()
	tests := []struct {
		name string
		run  func() (any, error)
		want any
	}{
		{"InsertInsight", func() (any, error) { return store.InsertInsight(ctx, db.Insight{}) }, int64(0)},
		{"DeleteInsight", func() (any, error) { return nil, store.DeleteInsight(ctx, 1) }, nil},
		{"RenameSession", func() (any, error) { return nil, store.RenameSession(ctx, "missing", nil) }, nil},
		{"SoftDeleteSession", func() (any, error) { return nil, store.SoftDeleteSession(ctx, "missing") }, nil},
		{"SoftDeleteSessions", func() (any, error) { return store.SoftDeleteSessions(ctx, []string{"missing"}) }, 0},
		{"RestoreSession", func() (any, error) { return store.RestoreSession(ctx, "missing") }, int64(0)},
		{"DeleteSessionIfTrashed", func() (any, error) { return store.DeleteSessionIfTrashed(ctx, "missing") }, int64(0)},
		{"EmptyTrash", func() (any, error) { return store.EmptyTrash(ctx) }, 0},
		{"StarSession", func() (any, error) { return store.StarSession(ctx, "missing") }, false},
		{"UnstarSession", func() (any, error) { return nil, store.UnstarSession(ctx, "missing") }, nil},
		{"BulkStarSessions", func() (any, error) { return nil, store.BulkStarSessions(ctx, []string{"missing"}) }, nil},
		{"PinMessage", func() (any, error) { return store.PinMessage(ctx, "missing", 1, nil) }, int64(0)},
		{"UnpinMessage", func() (any, error) { return nil, store.UnpinMessage(ctx, "missing", 1) }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := tt.run()
			require.ErrorIs(t, err, db.ErrReadOnly)
			if tt.want != nil {
				assert.Equal(t, tt.want, value)
			}
		})
	}
}

func TestReadOnlyStubs(t *testing.T) {
	store := ReadOnlyStubs{}
	ctx := t.Context()
	tests := []struct {
		name string
		run  func() (any, error)
		want any
	}{
		{"ListRecallEntries", func() (any, error) { return store.ListRecallEntries(ctx, db.RecallQuery{}) }, ([]db.RecallEntry)(nil)},
		{"GetRecallEntry", func() (any, error) { return store.GetRecallEntry(ctx, "missing") }, (*db.RecallEntry)(nil)},
		{"ReviewRecallEntry", func() (any, error) { return store.ReviewRecallEntry(ctx, "missing", db.RecallReviewAction("")) }, db.RecallEntry{}},
		{"QueryRecallEntries", func() (any, error) { return store.QueryRecallEntries(ctx, db.RecallQuery{}) }, db.RecallPage{}},
		{"RecordRecallQueryEvent", func() (any, error) { return store.RecordRecallQueryEvent(ctx, db.RecallQueryEvent{}) }, ""},
		{"InsertRecallEntry", func() (any, error) { return store.InsertRecallEntry(ctx, db.RecallEntry{}) }, ""},
		{"ImportAcceptedRecallEntriesJSONL", func() (any, error) { return store.ImportAcceptedRecallEntriesJSONL(ctx, nil) }, db.RecallImportResult{}},
		{"ImportAcceptedRecallEntriesJSONLWithOptions", func() (any, error) {
			return store.ImportAcceptedRecallEntriesJSONLWithOptions(ctx, nil, db.RecallImportOptions{})
		}, db.RecallImportResult{}},
		{"IngestEvalTrajectory", func() (any, error) { return store.IngestEvalTrajectory(ctx, db.EvalTrajectoryIngest{}) }, db.EvalTrajectoryIngestResult{}},
		{"UpsertSession", func() (any, error) { return nil, store.UpsertSession(ctx, db.Session{}) }, nil},
		{"ReplaceSessionMessages", func() (any, error) { return nil, store.ReplaceSessionMessages(ctx, "missing", nil) }, nil},
		{"WriteSessionBatchAtomic", func() (any, error) { return store.WriteSessionBatchAtomic(ctx, nil) }, db.SessionBatchResult{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := tt.run()
			require.ErrorIs(t, err, db.ErrReadOnly)
			if tt.want != nil {
				assert.Equal(t, tt.want, value)
			}
		})
	}
}
