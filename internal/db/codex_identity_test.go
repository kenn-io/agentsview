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
			for _, suffixes := range [][2]string{{"", ""}, {"", ".zst"}, {".zst", ""}} {
				t.Run(prefix+agent+suffixes[0]+"_to_"+suffixes[1], func(t *testing.T) {
					d := testDB(t)
					path := filepath.Join(t.TempDir(), "rollout-restored.jsonl")
					oldPath, newPath := path+suffixes[0], path+suffixes[1]
					oldID, correctedID := prefix+agent+":old", prefix+agent+":corrected"
					require.NoError(t, d.UpsertSession(t.Context(), Session{
						ID: oldID, Agent: agent, Project: "sample", Machine: "local", FilePath: &oldPath,
					}))
					require.NoError(t, d.DeleteSession(t.Context(), oldID))

					corrected := Session{ID: correctedID, Agent: agent, Project: "sample", Machine: "local", FilePath: &newPath}
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
}
