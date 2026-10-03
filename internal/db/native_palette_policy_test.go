package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestNativePaletteArchivePolicies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			source := testDB(t)
			insertSession(t, source, "palette-policy", "project")
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{
				SessionID: "palette-policy", Role: "assistant", Content: "dialogueneedle", ThinkingText: "reasonneedle", HasThinking: true,
				ToolResultText: "outputneedle", HasToolUse: true,
				ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"commandneedle"}`, Rendering: "[Bash]\n$ commandneedle", ResultContent: "resultneedle"}},
			}}))
			page, err := source.Search(t.Context(), SearchFilter{Query: "reasonneedle commandneedle outputneedle resultneedle"})
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			target := testDB(t)
			target.SetArchiveContent(policy)
			copied, err := target.CopyOrphanedDataFromExcluding(source.Path(), nil)
			require.NoError(t, err)
			require.Equal(t, []string{"palette-policy"}, copied)
			for _, query := range []string{"commandneedle", "outputneedle", "resultneedle"} {
				page, err := target.Search(t.Context(), SearchFilter{Query: query})
				require.NoError(t, err)
				assert.Empty(t, page.Results, query)
			}
			for _, query := range []string{"dialogueneedle", "reasonneedle"} {
				page, err := target.Search(t.Context(), SearchFilter{Query: query})
				require.NoError(t, err)
				if policy == config.ArchiveContentTranscripts {
					assert.Len(t, page.Results, 1, query)
				} else {
					assert.Empty(t, page.Results, query)
				}
			}
		})
	}
}

func TestNativePaletteReplaceAndRebuild(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "palette-update", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "palette-update", Role: "assistant", ThinkingText: "oldreason", HasThinking: true}}))
	page, err := d.Search(t.Context(), SearchFilter{Query: "oldreason"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.DropFTS(t.Context()))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "palette-update", []Message{{SessionID: "palette-update", Role: "assistant", ThinkingText: "newreason", HasThinking: true}}))
	require.NoError(t, d.RebuildFTS(t.Context()))
	page, err = d.Search(t.Context(), SearchFilter{Query: "oldreason"})
	require.NoError(t, err)
	assert.Empty(t, page.Results)
	page, err = d.Search(t.Context(), SearchFilter{Query: "newreason"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, 0, page.Results[0].Ordinal)
}

func TestNativePaletteRecipeUpgradePreservesArchive(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "palette-upgrade", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "palette-upgrade", Role: "assistant", Content: "dialogueneedle", ThinkingText: "reasonneedle", HasThinking: true}}))
	before, err := d.GetAllMessages(t.Context(), "palette-upgrade")
	require.NoError(t, err)
	require.Len(t, before, 1)
	// An older derived recipe can be rebuilt; the saved message is untouched.
	_, err = d.getWriter().Exec(t.Context(), "UPDATE palette_messages SET content = 'oldcorpus'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE stats SET value = 'old-v0' WHERE key = 'palette_corpus_recipe'")
	require.NoError(t, err)
	path := d.Path()
	require.NoError(t, d.Close())
	reopened, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	after, err := reopened.GetAllMessages(t.Context(), "palette-upgrade")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].ID, after[0].ID)
	assert.Equal(t, "dialogueneedle", after[0].Content)
	assert.Equal(t, "reasonneedle", after[0].ThinkingText)
	page, err := reopened.Search(t.Context(), SearchFilter{Query: "reasonneedle"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "palette-upgrade", page.Results[0].SessionID)
}

func TestNativePaletteCJKThinkingKeepsDialogueIndexSeparate(t *testing.T) {
	d := testDB(t)
	if !d.HasCJKFTS(t.Context()) {
		t.Skip("simple FTS runtime unavailable")
	}
	insertSession(t, d, "palette-cjk", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "palette-cjk", Role: "assistant", Content: "answer", ThinkingText: "用户认证", HasThinking: true}}))
	page, err := d.Search(t.Context(), SearchFilter{Query: "用户认证"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, 0, page.Results[0].Ordinal)
	dialogue, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "用户认证", Mode: "fts", Sources: []string{"messages"}})
	require.NoError(t, err)
	assert.Empty(t, dialogue.Matches)
}

func TestNativeDialogueFTSCorpusExcludesLegacy(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "palette-legacy", "project")
	legacy := Message{SessionID: "palette-legacy", Ordinal: 1, Role: "assistant", Content: "legacyneedle"}
	legacy.SetContentLayout(nil)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "palette-legacy", Ordinal: 0, Role: "assistant", Content: "dialogueneedle"}, legacy}))
	for _, tc := range []struct {
		query string
		count int
	}{{"dialogueneedle", 1}, {"legacyneedle", 0}} {
		var count int
		require.NoError(t, d.getReader().QueryRow(t.Context(), "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?", tc.query).Scan(&count))
		assert.Equal(t, tc.count, count, tc.query)
	}
	page, err := d.Search(t.Context(), SearchFilter{Query: "legacyneedle"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, 1, page.Results[0].Ordinal)
}
