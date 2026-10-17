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
			_, err := database.SetSessionLabels(ctx, oldID, []string{"saved", "topic"})
			require.NoError(t, err)
			_, err = database.AssignSessionProject(ctx, oldID, "chosen")
			require.NoError(t, err)
			_, err = database.StarSession(ctx, oldID)
			require.NoError(t, err)
			messages, err := database.GetAllMessages(ctx, oldID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(ctx, oldID, messages[0].ID, new("saved note"))
			require.NoError(t, err)
			insertSession(t, database, "manager", "sample")
			_, err = database.SetSessionExternalParent(ctx, oldID, "manager")
			require.NoError(t, err)
			insertSession(t, database, "child", "sample")
			_, err = database.SetSessionExternalParent(ctx, "child", oldID)
			require.NoError(t, err)
			insertVerifiedRecallSelection(t, database, "saved-recall", oldID, 0, 0, nil)
			_, err = database.ReviewRecallEntry(ctx, "saved-recall", RecallReviewApprove)
			require.NoError(t, err)
			if mode == "same_transcript" || mode == "other_source" {
				destinationPath := path
				if mode == "other_source" {
					destinationPath = filepath.Join(t.TempDir(), "another.jsonl")
				}
				require.NoError(t, database.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "sample", Machine: "local", FilePath: &destinationPath}))
				require.NoError(t, database.InsertMessages(ctx, []Message{{SessionID: id, Role: "user", Content: "saved question", SourceUUID: "saved-message"}}))
			}
			if mode == "same_transcript" {
				_, err = database.AssignSessionProject(ctx, id, "destination")
				require.NoError(t, err)
				_, err = database.SetSessionLabels(ctx, id, []string{"destination"})
				require.NoError(t, err)
				insertSession(t, database, "destination-manager", "sample")
				_, err = database.SetSessionExternalParent(ctx, id, "destination-manager")
				require.NoError(t, err)
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
			expectedProject, expectedLabels := "chosen", []string{"saved", "topic"}
			if mode == "same_transcript" {
				expectedProject, expectedLabels = "destination", []string{"destination"}
			}
			assert.Equal(t, expectedProject, saved.Project)
			assert.True(t, saved.ProjectAssigned)
			assert.Equal(t, expectedLabels, saved.Labels)
			parent := "manager"
			if mode == "same_transcript" {
				parent = "destination-manager"
			}
			assert.Equal(t, &parent, saved.ParentSessionID)
			link, err := database.GetSessionExternalParent(ctx, retainedID)
			require.NoError(t, err)
			assert.Equal(t, parent, link.ParentSessionID)
			assert.True(t, link.Applied)
			child, err := database.GetSessionExternalParent(ctx, "child")
			require.NoError(t, err)
			assert.Equal(t, retainedID, child.ParentSessionID)
			assert.True(t, child.Applied)
			entry, err := database.GetRecallEntry(ctx, "saved-recall")
			require.NoError(t, err)
			require.NotNil(t, entry)
			assert.Equal(t, retainedID, entry.SourceSessionID)
			assert.Equal(t, "accepted", entry.Status)
			assert.Equal(t, "human_reviewed", entry.ReviewState)
			assert.True(t, entry.ProvenanceOK)
			require.Len(t, entry.Evidence, 1)
			assert.Equal(t, retainedID, entry.Evidence[0].SessionID)
			if retainedID == id {
				require.NoError(t, database.UpsertSession(ctx, Session{ID: id, Agent: "codex", Project: "parsed", Machine: "local", FilePath: &path}))
				reparsed, err := database.GetSession(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, expectedProject, reparsed.Project)
				var retainedLabels int
				require.NoError(t, database.getReader().QueryRowContext(ctx, "SELECT COUNT(*) FROM session_labels WHERE session_id = ?", oldID).Scan(&retainedLabels))
				assert.Zero(t, retainedLabels)
			}
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

func TestCodexUpgradePreservesDestinationCuration(t *testing.T) {
	ctx := t.Context()
	source, destination := testDB(t), testDB(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, source.UpsertSession(ctx, Session{ID: "codex:old", Agent: "codex", Project: "sample", Machine: "local", FilePath: &path}))
	_, err := source.getWriter().ExecContext(ctx, "PRAGMA user_version = 135")
	require.NoError(t, err)
	_, err = source.SetSessionLabels(ctx, "codex:old", []string{"old"})
	require.NoError(t, err)
	_, err = source.AssignSessionProject(ctx, "codex:old", "old-project")
	require.NoError(t, err)
	// A launcher can label the destination before the source is discovered.
	_, err = source.SetSessionLabels(ctx, "codex:new", []string{"destination"})
	require.NoError(t, err)
	require.NoError(t, destination.UpsertSession(ctx, Session{ID: "codex:new", Agent: "codex", Project: "parsed", Machine: "local", FilePath: &path, DataVersion: 136}))
	_, err = destination.AssignSessionProject(ctx, "codex:new", "destination-project")
	require.NoError(t, err)
	require.NoError(t, destination.CopySessionMetadataFrom(source.Path()))
	got, err := destination.GetSession(ctx, "codex:new")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"destination"}, got.Labels)
	assert.Equal(t, "destination_project", got.Project)
	assert.True(t, got.ProjectAssigned)
}
