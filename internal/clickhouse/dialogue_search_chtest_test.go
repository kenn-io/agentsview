//go:build chtest

package clickhouse

import (
	"strings"
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
	dbtest.AssertDialogueSearchParity(t, local, store, "substring", "regex", "fts")
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

	// Model rows pushed before dialogue was stored.
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE messages UPDATE dialogue_text = NULL WHERE session_id = ? SETTINGS mutations_sync=2", fixtureAlphaID)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE key = ? SETTINGS mutations_sync=2", syncer.archiveKey(messageDialogueReadyKeyBase))
	require.NoError(t, err)

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Full)
	assert.Zero(t, result.Errors)
	assert.Equal(t, 3, result.SessionsPushed, "a fresh full push rewrites every fixture session")
	assert.Zero(t, result.SkippedUnchanged)
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
	store.SetVectorSearcher(&activityVectorSearcher{})
	privateDialogue := "answerneedle -----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 10) +
		"-----END RSA PRIVATE KEY-----"
	for _, tc := range []struct {
		name, pattern, dialogue string
		want                    int
		private, reveal         bool
	}{
		{name: "dialogue", pattern: "answerneedle", dialogue: "answerneedle", want: 1},
		{name: "reasoning", pattern: "reasoningneedle", dialogue: "answerneedle"},
		{name: "tool", pattern: "toolneedle", dialogue: "answerneedle"},
		{name: "redacted", pattern: "answerneedle", dialogue: privateDialogue, want: 1, private: true},
		{name: "revealed", pattern: "answerneedle", dialogue: privateDialogue, want: 1, private: true, reveal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages[0].Content = tc.dialogue + "\n[Thinking] reasoningneedle\n[Bash] $ toolneedle"
			messages[0].ThinkingText = "reasoningneedle"
			messages[0].DialogueText = new(tc.dialogue)
			require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages))
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			page, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: tc.pattern, Mode: "hybrid", IncludeOneShot: true, RevealSecrets: tc.reveal,
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, tc.want)
			if tc.want > 0 {
				assert.Equal(t, fixtureAlphaID, page.Matches[0].SessionID)
				snippet := page.Matches[0].Snippet
				assert.Contains(t, snippet, "answerneedle")
				assert.NotContains(t, snippet, "[Thinking]")
				assert.NotContains(t, snippet, "[Bash]")
				if tc.private {
					if tc.reveal {
						assert.Contains(t, snippet, "SECRETKEYMATERIAL")
					} else {
						assert.NotContains(t, snippet, "SECRETKEYMATERIAL")
					}
				}
			}
		})
	}
}

func TestDialogueReparseClickHouseRepushesSession(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	require.NoError(t, local.SetSessionDataVersion(ctx, fixtureAlphaID, 139))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	// The source file and rendered content stay unchanged during a scoped
	// reparse. Ordinary sync writes messages before stamping the parser version.
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	messages[0].DialogueText = new("answerneedle")
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages))
	require.NoError(t, local.SetSessionDataVersion(ctx, fixtureAlphaID, db.CurrentDataVersion()))

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Full)
	assert.Zero(t, result.Errors)
	assert.Equal(t, 1, result.SessionsPushed)
	var dialogue string
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT dialogue_text FROM messages WHERE session_id = ? AND ordinal = 0", fixtureAlphaID).Scan(&dialogue))
	assert.Equal(t, "answerneedle", dialogue)
}
