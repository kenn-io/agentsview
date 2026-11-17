//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

func hostedVectorUnitParity(t *testing.T) {
	f := newHostedFixture(t, "tenant-unit-parity")
	sqlite, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
	require.NoError(t, sqlite.UpsertSession(t.Context(), db.Session{ID: "parity", Project: "p", Machine: "m", Agent: "codex"}))
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('parity','p','m','codex','raw','r')`)
	require.NoError(t, err)
	messages := []db.Message{
		{SessionID: "parity", Ordinal: 0, Role: "user", Content: "hello", SourceUUID: "duplicate"},
		{SessionID: "parity", Ordinal: 1, Role: "assistant", Content: "é", SourceUUID: "run"},
		{SessionID: "parity", Ordinal: 2, Role: "user", Content: "system", IsSystem: true},
		{SessionID: "parity", Ordinal: 3, Role: "assistant", Content: "界", SourceUUID: "next"},
		{SessionID: "parity", Ordinal: 4, Role: "assistant", Content: "side", IsSidechain: true},
		{SessionID: "parity", Ordinal: 5, Role: "user", Content: "again", SourceUUID: "duplicate"},
		{SessionID: "parity", Ordinal: 6, Role: "user", Content: "This session is being continued from a previous one"},
	}
	require.NoError(t, sqlite.InsertMessages(t.Context(), messages))
	for _, m := range messages {
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content,is_system,is_sidechain,source_uuid) VALUES($1,$2,$3,$4,$5,$6,$7)`, m.SessionID, m.Ordinal, m.Role, m.Content, m.IsSystem, m.IsSidechain, m.SourceUUID)
		require.NoError(t, err)
	}
	var local, pg []db.EmbeddableUnit
	_, err = sqlite.ScanEmbeddableUnits(t.Context(), "", false, 512, func(u db.EmbeddableUnit) error { local = append(local, u); return nil })
	require.NoError(t, err)
	require.NoError(t, scanHostedUnits(t.Context(), f.runtime, "parity", false, 512, func(u db.EmbeddableUnit) error { pg = append(pg, u); return nil }))
	require.Equal(t, local, pg)
	require.Len(t, pg, 4)
	assert.Equal(t, "é\n\n界", pg[1].Content)
	assert.Equal(t, []db.UnitOffset{{Ordinal: 1}, {Ordinal: 3, RuneStart: 3, ByteStart: 4}}, pg[1].Offsets)
	docs, err := vector.UnitDocuments(pg)
	require.NoError(t, err)
	docs, _, err = vector.FillUnitDocuments(t.Context(), docs, 128, 1, func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	})
	require.NoError(t, err)
	require.Len(t, docs, 4)
	assert.Equal(t, "u:parity:duplicate", docs[0].DocKey)
	assert.Equal(t, "u:parity:duplicate#2", docs[3].DocKey)
	assert.Equal(t, `[{"o":1,"r":0,"b":0},{"o":3,"r":3,"b":4}]`, docs[1].OffsetsJSON)
}
