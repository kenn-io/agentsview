//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseDialogueSearchParity(t *testing.T) {
	local, target := seedFixture(t)
	dbtest.SeedDialogueSearch(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertDialogueSearchParity(t, local, store, "", "substring", "regex", "fts")
}

func TestDialogueClickHouseBackfillsExistingPushCursor(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	messages[0].DialogueText = new("answerneedle")
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	// Model an older writer: its rows and fingerprints predate dialogue.
	snapshot, err := local.LoadSessionMirrorSnapshot(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	for i := range snapshot.Messages {
		snapshot.Messages[i].DialogueText = nil
	}
	oldFingerprint, err := syncer.snapshotFingerprint(snapshot)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE messages UPDATE dialogue_text = NULL WHERE session_id = ? SETTINGS mutations_sync=2", fixtureAlphaID)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE sessions UPDATE agentsview_push_fingerprint = ? WHERE id = ? SETTINGS mutations_sync=2", oldFingerprint, fixtureAlphaID)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE key = ? SETTINGS mutations_sync=2", syncer.archiveKey(messageDialogueReadyKeyBase))
	require.NoError(t, err)

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Full)
	assert.Zero(t, result.Errors)
	assert.Equal(t, 1, result.SessionsPushed)
	var dialogue string
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT dialogue_text FROM messages WHERE session_id = ? AND ordinal = 0", fixtureAlphaID).Scan(&dialogue))
	assert.Equal(t, "answerneedle", dialogue)
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Full)
	assert.Zero(t, result.Errors)
	assert.Zero(t, result.SessionsPushed)
}

func TestDialogueClickHouseHybridKeywordSnippets(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	messages[0].Content = "answerneedle\n[Thinking] reasoningneedle\n[Bash] $ toolneedle"
	messages[0].ThinkingText = "reasoningneedle"
	messages[0].DialogueText = new("answerneedle")
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store.SetVectorSearcher(&activityVectorSearcher{})
	for _, tc := range []struct {
		pattern string
		want    int
	}{
		{"answerneedle", 1},
		{"reasoningneedle", 0},
		{"toolneedle", 0},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			page, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: tc.pattern, Mode: "hybrid", IncludeOneShot: true,
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, tc.want)
			if tc.want > 0 {
				assert.Equal(t, fixtureAlphaID, page.Matches[0].SessionID)
				assert.Contains(t, page.Matches[0].Snippet, "answerneedle")
				assert.NotContains(t, page.Matches[0].Snippet, "[Thinking]")
				assert.NotContains(t, page.Matches[0].Snippet, "[Bash]")
			}
		})
	}
}
