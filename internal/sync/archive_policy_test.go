package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Both collecting and scratch-streaming Codex writes must pass the archive's
// identity/deletion policy before writing normalized content.
func TestArchivePolicySuppressesCodexPublication(t *testing.T) {
	for _, threshold := range []int64{0, 1} {
		t.Run(map[int64]string{0: "collecting", 1: "staged"}[threshold], func(t *testing.T) {
			const id = "019eb791-cf7d-75c1-8439-9ed74c122b13"
			root := writeCodexParityRoot(t, id)
			database := openTestDB(t)
			var observed []string
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "source", IDPrefix: "installation~", Ephemeral: true,
				ArchiveReparse: true, StagedCodexParseMinBytes: threshold,
				DiscardPendingWritesOnCancel: true,
				ArchiveSessionPolicy: func(_ context.Context, s *db.Session, native string) (bool, error) {
					observed = append(observed, native)
					assert.Equal(t, "installation~codex:"+id, s.ID)
					return false, nil
				},
			})
			defer engine.Close()
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			assert.Equal(t, []string{"codex:" + id}, observed)
			stored, err := database.GetSessionFull(t.Context(), "installation~codex:"+id)
			require.NoError(t, err)
			assert.Nil(t, stored)
			messages, err := database.GetAllMessages(t.Context(), "installation~codex:"+id)
			require.NoError(t, err)
			assert.Empty(t, messages)
		})
	}
}
