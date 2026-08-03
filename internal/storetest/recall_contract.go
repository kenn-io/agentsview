package storetest

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// RecallStore is the Task 6 shared Recall entry-point family.
type RecallStore interface {
	ListRecallEntries(context.Context, db.RecallQuery) ([]db.RecallEntry, error)
	GetRecallEntry(context.Context, string) (*db.RecallEntry, error)
	QueryRecallEntries(context.Context, db.RecallQuery) (db.RecallPage, error)
	RecordRecallQueryEvent(context.Context, db.RecallQueryEvent) (string, error)
	InsertRecallEntry(db.RecallEntry) (string, error)
	ImportAcceptedRecallEntriesJSONL(context.Context, io.Reader) (db.RecallImportResult, error)
	ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Context, io.Reader, db.RecallImportOptions,
	) (db.RecallImportResult, error)
	IngestEvalTrajectory(
		context.Context, db.EvalTrajectoryIngest,
	) (db.EvalTrajectoryIngestResult, error)
}

type RecallBackend struct {
	Name     string
	Open     func(*testing.T) RecallStore
	Writable bool
}

// RunRecallContract verifies that the public Recall surface is owned by the
// embedded BunStore and capability-gated before parsing or SQL.
func RunRecallContract(t *testing.T, backend RecallBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store := backend.Open(t)
		if !backend.Writable {
			assertReadOnlyRecall(t, store)
			return
		}
		assertWritableRecall(t, store)
	})
}

func assertReadOnlyRecall(t *testing.T, store RecallStore) {
	t.Helper()
	ctx := t.Context()
	entries, err := store.ListRecallEntries(ctx, db.RecallQuery{})
	assert.Nil(t, entries)
	require.ErrorIs(t, err, db.ErrReadOnly)
	entry, err := store.GetRecallEntry(ctx, "recall")
	assert.Nil(t, entry)
	require.ErrorIs(t, err, db.ErrReadOnly)
	page, err := store.QueryRecallEntries(ctx, db.RecallQuery{})
	assert.Empty(t, page.RecallEntries)
	require.ErrorIs(t, err, db.ErrReadOnly)
	id, err := store.RecordRecallQueryEvent(ctx, db.RecallQueryEvent{})
	assert.Empty(t, id)
	require.ErrorIs(t, err, db.ErrReadOnly)
	id, err = store.InsertRecallEntry(db.RecallEntry{})
	assert.Empty(t, id)
	require.ErrorIs(t, err, db.ErrReadOnly)
	imported, err := store.ImportAcceptedRecallEntriesJSONL(
		ctx, strings.NewReader("not-json"),
	)
	assert.Empty(t, imported)
	require.ErrorIs(t, err, db.ErrReadOnly)
	imported, err = store.ImportAcceptedRecallEntriesJSONLWithOptions(
		ctx, strings.NewReader("not-json"), db.RecallImportOptions{DryRun: true},
	)
	assert.Empty(t, imported)
	require.ErrorIs(t, err, db.ErrReadOnly)
	ingested, err := store.IngestEvalTrajectory(ctx, db.EvalTrajectoryIngest{})
	assert.Empty(t, ingested)
	require.ErrorIs(t, err, db.ErrReadOnly)
}

func assertWritableRecall(t *testing.T, store RecallStore) {
	t.Helper()
	ctx := t.Context()
	insertedID, err := store.InsertRecallEntry(db.RecallEntry{
		ID: "bun-recall-entry", Type: "fact", Scope: "project",
		Title: "Canonical Recall entry", Body: "Recall uses the common Store surface.",
		Project: "recall-contract", SourceSessionID: "bun-recall-source",
		Transferable: true, ProvenanceOK: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "bun-recall-entry", insertedID)

	got, err := store.GetRecallEntry(ctx, insertedID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Canonical Recall entry", got.Title)
	listed, err := store.ListRecallEntries(ctx, db.RecallQuery{
		SourceSessionID: "bun-recall-source",
	})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, insertedID, listed[0].ID)
	page, err := store.QueryRecallEntries(ctx, db.RecallQuery{
		SourceSessionID: "bun-recall-source",
	})
	require.NoError(t, err)
	require.Len(t, page.RecallEntries, 1)
	assert.Equal(t, insertedID, page.RecallEntries[0].ID)

	queryID, err := store.RecordRecallQueryEvent(ctx, db.RecallQueryEvent{
		Query: "canonical recall", Surface: db.RecallQuerySurfaceQuery,
		ResultCount: 1,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, queryID)

	input := strings.NewReader(`
{"candidate_id":"bun-recall-import","type":"fact","scope":"project","title":"Imported Recall entry","body":"Imported through the common Store surface.","project":"recall-contract","session_id":"bun-recall-import-source","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)
	imported, err := store.ImportAcceptedRecallEntriesJSONL(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, 1, imported.Imported)
	importedEntry, err := store.GetRecallEntry(ctx, "bun-recall-import")
	require.NoError(t, err)
	require.NotNil(t, importedEntry)
	assert.Equal(t, "Imported Recall entry", importedEntry.Title)

	ingested, err := store.IngestEvalTrajectory(ctx, db.EvalTrajectoryIngest{
		RunID: "bun-recall-run", TrajectoryID: "bun-recall-trajectory",
		Trajectory:      json.RawMessage(`{"message":"portable recall trajectory"}`),
		ExtractorMethod: "contract-raw", SourceVersion: "contract-v1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, ingested.EntriesIndexed)
	assert.NotEmpty(t, ingested.CorpusID)
}
