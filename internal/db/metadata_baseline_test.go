package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisitMetadataBaselinePagesBoundsSmallAndLargeCuration(t *testing.T) {
	for _, size := range []int{4, 257} {
		t.Run(fmt.Sprintf("rows_%d", size), func(t *testing.T) {
			database := testDB(t)
			seedMetadataBaselineCuration(t, database, size)
			counts := MetadataBaselineSnapshot{}
			maxPage := 0
			pages := 0

			err := database.VisitMetadataBaselinePages(t.Context(), func(page MetadataBaselineSnapshot) error {
				pages++
				pageRows := len(page.Renames) + len(page.StarredSessionIDs) +
					len(page.SoftDeletedIDs) + len(page.Pins)
				maxPage = max(maxPage, pageRows)
				counts.Renames = append(counts.Renames, page.Renames...)
				counts.StarredSessionIDs = append(
					counts.StarredSessionIDs, page.StarredSessionIDs...,
				)
				counts.SoftDeletedIDs = append(counts.SoftDeletedIDs, page.SoftDeletedIDs...)
				counts.Pins = append(counts.Pins, page.Pins...)
				return database.SetSyncState(fmt.Sprintf("baseline_page_%d", pages), "visited")
			})
			require.NoError(t, err)
			assert.Len(t, counts.Renames, size)
			assert.Len(t, counts.StarredSessionIDs, size)
			assert.Len(t, counts.SoftDeletedIDs, size)
			assert.Len(t, counts.Pins, size)
			assert.Equal(t, min(size, 128), maxPage,
				"retained page size must not grow with total curation cardinality")
			assert.Positive(t, pages)
		})
	}
}

func TestVisitMetadataBaselinePagesStopsBetweenPagesOnCancellation(t *testing.T) {
	database := testDB(t)
	seedMetadataBaselineCuration(t, database, 129)
	ctx, cancel := context.WithCancel(t.Context())
	visited := 0
	pages := 0

	err := database.VisitMetadataBaselinePages(ctx, func(page MetadataBaselineSnapshot) error {
		pages++
		visited += len(page.Renames) + len(page.StarredSessionIDs) +
			len(page.SoftDeletedIDs) + len(page.Pins)
		cancel()
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, pages)
	assert.Equal(t, 128, visited)
}

func TestVisitMetadataBaselinePagesPinsUseCompositeKeysetWithoutOffsetDrift(t *testing.T) {
	database := testDB(t)
	sessionID := "many-pins"
	require.NoError(t, database.UpsertSession(Session{
		ID: sessionID, Project: "project-a", Machine: "local", Agent: "claude",
		MessageCount: 257, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	messages := make([]Message, 257)
	for index := range messages {
		messages[index] = Message{
			SessionID: sessionID, Ordinal: index, Role: "user", Content: "hello",
			ContentLength: 5, SourceUUID: fmt.Sprintf("uuid-%04d", index),
		}
	}
	require.NoError(t, database.InsertMessages(messages))
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 257)
	for index := range messages {
		note := fmt.Sprintf("note-%04d", index)
		_, err := database.PinMessage(sessionID, messages[index].ID, &note)
		require.NoError(t, err)
	}

	var ordinals []int
	maxPage := 0
	pinPages := 0
	removedVisitedPin := false
	err = database.VisitMetadataBaselinePages(t.Context(), func(page MetadataBaselineSnapshot) error {
		if len(page.Pins) == 0 {
			return nil
		}
		pinPages++
		maxPage = max(maxPage, len(page.Pins))
		for _, pin := range page.Pins {
			ordinals = append(ordinals, pin.Ordinal)
		}
		if !removedVisitedPin {
			removedVisitedPin = true
			return database.UnpinMessage(sessionID, messages[0].ID)
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, ordinals, 257,
		"deleting an already-visited row must not shift a later keyset page")
	for index, ordinal := range ordinals {
		assert.Equal(t, index, ordinal)
	}
	assert.Equal(t, 3, pinPages)
	assert.Equal(t, 128, maxPage)
}

// A session that was renamed, starred, and pinned before the machine first
// opted into artifact sync must baseline that curation even while it sits in
// trash: only the soft delete would otherwise publish, and a later restore
// would reach peers without the name, star, or pin.
func TestMetadataBaselineSnapshotIncludesTrashedSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "proj", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, d.InsertMessages([]Message{{
		SessionID: "s1", Ordinal: 0, Role: "user", Content: "hi",
		ContentLength: 2, SourceUUID: "uuid-1",
	}}))
	name := "Kept name"
	require.NoError(t, d.RenameSession("s1", &name))
	starred, err := d.StarSession("s1")
	require.NoError(t, err)
	require.True(t, starred)
	msgs, err := d.GetAllMessages(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	note := "kept pin"
	_, err = d.PinMessage("s1", msgs[0].ID, &note)
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession("s1"))

	snap, err := d.MetadataBaselineSnapshot(ctx)
	require.NoError(t, err)

	require.Len(t, snap.Renames, 1)
	assert.Equal(t, "s1", snap.Renames[0].SessionID)
	require.NotNil(t, snap.Renames[0].DisplayName)
	assert.Equal(t, name, *snap.Renames[0].DisplayName)
	assert.Equal(t, []string{"s1"}, snap.StarredSessionIDs)
	assert.Equal(t, []string{"s1"}, snap.SoftDeletedIDs)
	require.Len(t, snap.Pins, 1)
	assert.Equal(t, "s1", snap.Pins[0].SessionID)
	assert.Equal(t, "uuid-1", snap.Pins[0].SourceUUID)
}

func seedMetadataBaselineCuration(t *testing.T, database *DB, count int) {
	t.Helper()
	ctx := t.Context()
	for index := range count {
		sessionID := fmt.Sprintf("session-%04d", index)
		require.NoError(t, database.UpsertSession(Session{
			ID: sessionID, Project: "project-a", Machine: "local", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: sessionID, Ordinal: 0, Role: "user", Content: "hello",
			ContentLength: 5, SourceUUID: fmt.Sprintf("uuid-%04d", index),
		}}))
		name := fmt.Sprintf("Curated %04d", index)
		require.NoError(t, database.RenameSession(sessionID, &name))
		starred, err := database.StarSession(sessionID)
		require.NoError(t, err)
		require.True(t, starred)
		messages, err := database.GetAllMessages(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		note := fmt.Sprintf("note-%04d", index)
		_, err = database.PinMessage(sessionID, messages[0].ID, &note)
		require.NoError(t, err)
		require.NoError(t, database.SoftDeleteSession(sessionID))
	}
}
