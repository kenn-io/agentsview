package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
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

func TestRecoverCodexSessionIdentityIsAtomic(t *testing.T) {
	for _, mode := range []string{"new_destination", "same_transcript", "other_source", "delete_failure"} {
		t.Run(mode, func(t *testing.T) {
			database := testDB(t)
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "rollout-restored.jsonl")
			oldID := parser.AltSessionID("codex:old", path)
			const id = "codex:corrected"
			require.NoError(t, database.UpsertSession(ctx, Session{ID: oldID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &path, DataVersion: 135}))
			require.NoError(t, database.InsertMessages(ctx, []Message{{SessionID: oldID, Role: "user", Content: "saved question", SourceUUID: "saved-message"}}))
			require.NoError(t, database.RenameSession(ctx, oldID, new("Saved name")))
			_, err := database.StarSession(ctx, oldID)
			require.NoError(t, err)
			messages, err := database.GetAllMessages(ctx, oldID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(ctx, oldID, messages[0].ID, new("saved note"))
			require.NoError(t, err)
			if mode == "same_transcript" || mode == "other_source" {
				destinationPath := path
				if mode == "other_source" {
					destinationPath = filepath.Join(t.TempDir(), "another.jsonl")
				}
				require.NoError(t, database.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "sample", Machine: "local", FilePath: &destinationPath}))
				require.NoError(t, database.InsertMessages(ctx, []Message{{SessionID: id, Role: "user", Content: "saved question", SourceUUID: "saved-message"}}))
			}
			if mode == "delete_failure" {
				_, err = database.getWriter().ExecContext(ctx, `CREATE TRIGGER reject_identity_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'forced delete failure'); END`)
				require.NoError(t, err)
			}
			err = database.RecoverCodexSessionIdentity(ctx, oldID, id, path, "codex")
			retainedID := id
			if mode == "other_source" || mode == "delete_failure" {
				require.Error(t, err)
				retainedID = oldID
			} else {
				require.NoError(t, err)
				requireSessionGone(t, database, oldID)
			}
			pins, err := database.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, retainedID, pins[0].SessionID)
			assert.Equal(t, new("saved note"), pins[0].Note)
			saved, err := database.GetSession(ctx, retainedID)
			require.NoError(t, err)
			require.NotNil(t, saved)
			assert.Equal(t, new("Saved name"), saved.DisplayName)
			stars, err := database.ListStarredSessionIDs(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{retainedID}, stars)
			messages, err = database.GetAllMessages(ctx, retainedID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "saved question", messages[0].Content)
			if mode == "delete_failure" {
				requireSessionGone(t, database, id)
			}
		})
	}
}
