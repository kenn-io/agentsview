package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexPermanentDeletionFollowsSourcePath(t *testing.T) {
	for _, agent := range []string{"codex", "traex", "augure-code"} {
		for _, prefix := range []string{"", "remote~"} {
			t.Run(prefix+agent, func(t *testing.T) {
				d := testDB(t)
				path := filepath.Join(t.TempDir(), "rollout-restored.jsonl")
				oldID, correctedID := prefix+agent+":old", prefix+agent+":corrected"
				require.NoError(t, d.UpsertSession(t.Context(), Session{
					ID: oldID, Agent: agent, Project: "sample", Machine: "local", FilePath: &path,
				}))
				require.NoError(t, d.DeleteSession(t.Context(), oldID))

				corrected := Session{ID: correctedID, Agent: agent, Project: "sample", Machine: "local", FilePath: &path}
				require.ErrorIs(t, d.UpsertSession(t.Context(), corrected), ErrSessionExcluded)
				require.ErrorIs(t, d.insertSessionIfAbsent(t.Context(), corrected), ErrSessionExcluded)
				requireSessionGone(t, d, correctedID)
				assert.True(t, d.IsSessionExcluded(t.Context(), oldID))
				assert.False(t, d.IsSessionExcluded(t.Context(), correctedID), "the path alone does not prove ownership of an ID")

				// The same ID can belong to another retained source.
				otherPath := filepath.Join(t.TempDir(), "rollout-other.jsonl")
				corrected.FilePath = &otherPath
				require.NoError(t, d.UpsertSession(t.Context(), corrected))
				assert.Equal(t, otherPath, d.GetSessionFilePath(t.Context(), correctedID))
			})
		}
	}
}
