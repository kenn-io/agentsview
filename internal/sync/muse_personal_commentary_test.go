package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestMusePersonalCommentarySearchAndExport(t *testing.T) {
	root, _ := musePersonalSyncFile(t,
		musePersonalSyncItem(0, `{"type":"message","role":"user","text":"Investigate the outage"}`)+
			musePersonalSyncItem(1, `{"type":"commentary_text","text":"Investigating sentinelprogress in the gateway logs"}`)+
			musePersonalSyncItem(2, `{"type":"message","role":"assistant","text":"Finished the investigation"}`))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	id := "muse-personal:" + musePersonalSyncID
	messages, err := database.GetMessages(t.Context(), id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "assistant", messages[1].Role)
	assert.Equal(t, "Investigating sentinelprogress in the gateway logs", messages[1].Content)

	for _, mode := range []string{"substring", "fts", "terms"} {
		t.Run(mode, func(t *testing.T) {
			page, err := database.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "sentinelprogress", Mode: mode, Sources: []string{"messages"}, SessionID: id,
				IncludeAutomated: true, IncludeOneShot: true, IncludeChildren: true,
			})
			require.NoError(t, err)
			assert.Len(t, page.Matches, 1, "visible assistant commentary must be searchable")
		})
	}

	t.Run("visible export", func(t *testing.T) {
		changes, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{TextMode: "visible"})
		require.NoError(t, err)
		var text []string
		for _, change := range changes.Changes {
			if change.Type != "message" {
				continue
			}
			body, err := database.GetConversationMessage(t.Context(), db.ConversationMessageOptions{
				TextMode: "visible", DatabaseID: changes.DatabaseID, SessionID: id, MessageID: change.MessageID, Revision: change.Revision,
			})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			text = append(text, *body.Text)
		}
		assert.Equal(t, []string{
			"Investigate the outage",
			"Investigating sentinelprogress in the gateway logs",
			"Finished the investigation",
		}, text)
	})
}
