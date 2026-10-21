//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/storage"
)

const evidenceSession = "evidence-pg"

func evidencePGMessage(name string) db.Message {
	n := int64(6)
	return db.Message{
		SessionID: evidenceSession, Ordinal: 0, Role: "assistant", Content: "write example.txt", SourceUUID: "a",
		FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"},
		ToolCalls:            []db.ToolCall{{ToolName: "write", ToolUseID: "t", Category: "Write", InputJSON: `{"path":"example.txt","content":"alpha\n"}`, ResultContent: "ok"}},
		FileEvidenceCandidates: []fileevidence.Candidate{{
			Record: fileevidence.Record{
				Position: fileevidence.Position{Entry: "a", Call: 0, ResultEntry: "b", Result: 0, Block: 0},
				Kind:     "write", MediaType: "text/plain", DisplayName: name, Size: &n,
				SHA256:      "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060",
				ProofMethod: fileevidence.ProofPiWriteUTF8V1, Retention: "source",
			},
			Payload: []byte("alpha\n"), Support: "input",
			SupportingText: `{"path":"example.txt","content":"alpha\n"}`, Call: 0,
		}},
	}
}

func seedEvidencePG(t *testing.T, local *db.DB, messages ...db.Message) {
	t.Helper()
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: evidenceSession, Project: "example", Machine: "local", Agent: "pi", MessageCount: len(messages),
	}))
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), evidenceSession, messages))
}

type evidencePGEnv struct {
	local *db.DB
	ps    *Sync
	store *Store
}

func newEvidencePGEnv(t *testing.T) evidencePGEnv {
	t.Helper()
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	local.SetAssetsDir(t.TempDir())
	ps, err := New(pgURL, "agentsview", local, "machine-evidence", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	require.NoError(t, ps.EnsureSchema(t.Context()))
	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return evidencePGEnv{local: local, ps: ps, store: store}
}

func (e evidencePGEnv) push(t *testing.T, full bool) {
	t.Helper()
	_, err := e.ps.Push(t.Context(), full, nil)
	require.NoError(t, err)
}

func TestFileEvidencePGPushParity(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage(""))
	e.push(t, false)

	want, err := e.local.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	require.Len(t, want.Rows, 1)
	got, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	assert.Equal(t, want.Rows, got.Rows)
	assert.Equal(t, want.Total, got.Total)
	assert.Equal(t, want.Coverage, got.Coverage)
	assert.Equal(t, want.Revision, got.Revision)
	assert.NotEmpty(t, got.Binding)
	assert.NotEqual(t, want.Binding, got.Binding, "PG binds to its own archive generation")

	page, err := e.store.ReadMessagesWithFileEvidence(t.Context(), evidenceSession, db.MessageWindow{Asc: true})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, got.Binding, page.Binding)
	assert.Equal(t, want.Rows[0].Record, page.Evidence[0][0])

	sel, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{
		Binding: got.Binding, OccurrenceID: occurrenceID(t, 0, want.Rows[0].Record, got.Binding),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, sel.Total)
	require.NoError(t, e.store.ValidateFileEvidenceBinding(t.Context(), evidenceSession, got.Binding))
	_, err = e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{
		Binding: got.Binding, OccurrenceID: occurrenceID(t, 0, want.Rows[0].Record, want.Binding),
	})
	require.ErrorIs(t, err, db.ErrFileEvidenceStale, "a SQLite selector cannot address PG rows")
}

func TestFileEvidencePGEvidenceOnlyChangeAndRemoval(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage(""))
	e.push(t, false)
	before, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)

	// Same message text, different evidence: message fingerprints match,
	// so only the evidence digest can carry this change.
	seedEvidencePG(t, e.local, evidencePGMessage("example.txt"))
	e.push(t, false)
	after, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	require.Len(t, after.Rows, 1)
	assert.Equal(t, "example.txt", after.Rows[0].Record.DisplayName)
	assert.NotEqual(t, before.Binding, after.Binding)
	require.ErrorIs(t, e.store.ValidateFileEvidenceBinding(t.Context(), evidenceSession, before.Binding), db.ErrFileEvidenceStale)

	// A full push rewrites messages; evidence must survive it.
	e.push(t, true)
	full, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	assert.Equal(t, after.Rows, full.Rows)

	m := evidencePGMessage("")
	m.FileEvidenceCandidates = nil
	seedEvidencePG(t, e.local, m)
	e.push(t, false)
	removed, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	want, err := e.local.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err)
	assert.Equal(t, want.Total, removed.Total)
	assert.Empty(t, removed.Rows)
	assert.Equal(t, want.Coverage, removed.Coverage)
}

func TestFileEvidencePGPushSkipsUnchanged(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage(""))
	e.push(t, false)
	pushOnce := func(full bool) bool {
		tx, err := e.ps.pg.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		changed, err := e.ps.pushFileEvidence(t.Context(), tx, evidenceSession, full)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return changed
	}
	assert.False(t, pushOnce(false))
	assert.False(t, pushOnce(true), "a forced rewrite of identical evidence is not a change")
}

func TestFileEvidencePGWithoutGenerationFallsBack(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage(""))
	e.push(t, false)
	_, err := e.ps.pg.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key = $1`, fileEvidenceGenerationKey)
	require.NoError(t, err)
	_, err = e.store.ReadMessagesWithFileEvidence(t.Context(), evidenceSession, db.MessageWindow{Asc: true})
	require.ErrorIs(t, err, db.ErrDatabaseIDMissing, "the service falls back to plain message reads")
	// The next evidence write records a fresh generation.
	e.push(t, true)
	_, err = e.store.ReadMessagesWithFileEvidence(t.Context(), evidenceSession, db.MessageWindow{Asc: true})
	require.NoError(t, err)
}

func TestFileEvidencePGPushSchemaRequiresTable(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage(""))
	e.push(t, false)
	require.True(t, pushSchemaCurrent(t.Context(), e.ps.pg))
	_, err := e.ps.pg.ExecContext(t.Context(), `DROP TABLE message_file_evidence`)
	require.NoError(t, err)
	assert.False(t, pushSchemaCurrent(t.Context(), e.ps.pg),
		"an upgraded hub must run EnsureSchema before pushing evidence")
	snap, err := e.store.ReadFileEvidenceSnapshot(t.Context(), evidenceSession, fileevidence.Window{})
	require.NoError(t, err, "a read-only role on an older schema still serves")
	assert.Empty(t, snap.Rows)
	assert.Equal(t, "unavailable", snap.Coverage.State)
}

// pg serve wraps the store in the read-only backend; the UI and MCP see
// evidence only through these service operations.
func TestFileEvidencePGServeBackend(t *testing.T) {
	e := newEvidencePGEnv(t)
	seedEvidencePG(t, e.local, evidencePGMessage("example.txt"))
	e.push(t, false)
	backend := service.NewReadOnlyBackend(e.store)

	list, err := backend.Messages(t.Context(), evidenceSession, service.MessageFilter{})
	require.NoError(t, err)
	require.Len(t, list.FileEvidence, 1)
	assert.Equal(t, "complete", list.FileEvidenceCoverage)
	assert.NotEmpty(t, list.Binding)
	require.Len(t, list.FileEvidence[0].Records, 1)
	assert.Equal(t, "example.txt", list.FileEvidence[0].Records[0].DisplayName)

	refs, err := backend.FileReferences(t.Context(), service.FileReferencesInput{SessionID: evidenceSession})
	require.NoError(t, err)
	assert.Equal(t, "available", refs.Capability)
	assert.Equal(t, "disabled", refs.Integration)
	assert.Equal(t, list.Binding, refs.Binding)
	require.Len(t, refs.Occurrences, 1)
	assert.Equal(t, list.FileEvidence[0].Records[0].OccurrenceID, refs.Occurrences[0].OccurrenceID)

	selected, err := backend.FileReferences(t.Context(), service.FileReferencesInput{
		SessionID: evidenceSession, Binding: refs.Binding, OccurrenceID: refs.Occurrences[0].OccurrenceID,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, selected.Total)

	seedEvidencePG(t, e.local, evidencePGMessage("renamed.txt"))
	e.push(t, false)
	_, err = backend.FileReferences(t.Context(), service.FileReferencesInput{
		SessionID: evidenceSession, Binding: refs.Binding, Offset: 1,
	})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func occurrenceID(t *testing.T, ordinal int, r fileevidence.Record, binding string) string {
	t.Helper()
	slot, err := r.Position.Slot()
	require.NoError(t, err)
	return db.EncodeFileEvidenceOccurrence(ordinal, slot, binding)
}
