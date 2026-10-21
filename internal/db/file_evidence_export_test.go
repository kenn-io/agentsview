package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/fileevidence"
)

func TestExportFileEvidenceMatchesSnapshot(t *testing.T) {
	d := testDB(t)
	seedEvidence(t, d)
	snap, err := d.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{})
	require.NoError(t, err)
	export, err := d.ExportFileEvidence(t.Context(), "evidence")
	require.NoError(t, err)
	assert.Equal(t, snap.Rows, export.Rows)
	assert.Equal(t, snap.Coverage, export.Coverage)
	assert.Len(t, export.Digest, 64)

	again, err := d.ExportFileEvidence(t.Context(), "evidence")
	require.NoError(t, err)
	assert.Equal(t, export.Digest, again.Digest, "an unchanged session keeps its digest")

	m := evidenceMessage()
	m.FileEvidenceCandidates[0].Record.DisplayName = "example.txt"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "evidence", []Message{m}))
	changed, err := d.ExportFileEvidence(t.Context(), "evidence")
	require.NoError(t, err)
	assert.NotEqual(t, export.Digest, changed.Digest, "evidence-only changes move the digest")
}

func TestFileEvidenceOccurrenceRoundTrip(t *testing.T) {
	slot, err := fileevidence.Position{Entry: "a", Call: 0, ResultEntry: "b", Result: 0, Block: 0}.Slot()
	require.NoError(t, err)
	binding := FileEvidenceBinding("generation", "session", "7")
	id := EncodeFileEvidenceOccurrence(3, slot, binding)
	ordinal, got, err := DecodeFileEvidenceOccurrence(id, binding)
	require.NoError(t, err)
	assert.Equal(t, 3, ordinal)
	assert.Equal(t, slot, got)

	_, _, err = DecodeFileEvidenceOccurrence(id, FileEvidenceBinding("generation", "session", "8"))
	require.ErrorIs(t, err, ErrFileEvidenceStale, "a selector from another revision is stale")
	_, _, err = DecodeFileEvidenceOccurrence("not base64!", binding)
	require.ErrorIs(t, err, ErrFileEvidenceStale)
	assert.NotEqual(t, FileEvidenceBinding("ab", "c", "1"), FileEvidenceBinding("a", "bc", "1"),
		"length prefixes keep tuple boundaries distinct")
}
