package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveNativeStagedPublication(t *testing.T) {
	for _, change := range []string{"unchanged", "output", "layout", "rendering"} {
		t.Run(change, func(t *testing.T) {
			d := testDB(t)
			const sessionID = "native-staged"
			insertSession(t, d, sessionID, "project")
			publish := func(message Message) {
				t.Helper()
				staged := newScratchStagedResults(t)
				require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), sessionID,
					[]Message{message}, staged, nil, nil))
			}
			publish(nativeArchiveFixture(t, sessionID))
			stored, err := d.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Len(t, stored[0].ToolCalls, 1)
			require.Equal(t, "old", stored[0].ToolCalls[0].Rendering)
			_, err = d.getWriter().Exec(t.Context(),
				`UPDATE sessions SET transcript_revision='7' WHERE id=?`, sessionID)
			require.NoError(t, err)
			message := nativeArchiveFixture(t, sessionID)
			switch change {
			case "output":
				message.ToolResultText = "newout"
			case "layout":
				message.ContentLayout.Blocks[0], message.ContentLayout.Blocks[1] =
					message.ContentLayout.Blocks[1], message.ContentLayout.Blocks[0]
			case "rendering":
				message.ToolCalls[0].Rendering = "new"
			}
			publish(message)
			stored, err = d.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			assert.Equal(t, "answer", stored[0].Content)
			assert.Equal(t, message.ToolResultText, stored[0].ToolResultText)
			assert.Equal(t, message.ContentLayout, stored[0].ContentLayout)
			require.Len(t, stored[0].ToolCalls, 1)
			assert.Equal(t, message.ToolCalls[0].Rendering, stored[0].ToolCalls[0].Rendering)
			session, err := d.GetSession(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.TranscriptRevision)
			wantRevision := "8"
			if change == "unchanged" {
				wantRevision = "7"
			}
			assert.Equal(t, wantRevision, *session.TranscriptRevision)
		})
	}
}

func TestArchiveNativeStagedUnattachedResultSummary(t *testing.T) {
	d := testDB(t)
	const sessionID = "native-staged-summary"
	insertSession(t, d, sessionID, "project")
	message := nativeArchiveFixture(t, sessionID)
	message.ToolCalls[0].ToolUseID = "call-inline"
	message.ToolCalls[0].ResultContent = "inline output"
	message.ToolCalls[0].ResultContentLength = 13
	staged := newScratchStagedResults(t)
	require.NoError(t, d.ReplaceSessionContentStaged(t.Context(), sessionID,
		[]Message{message}, staged, nil, nil))
	stored, err := d.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Len(t, stored[0].ToolCalls, 1)
	assert.Equal(t, "inline output", stored[0].ToolCalls[0].ResultContent)
	assert.Equal(t, 13, stored[0].ToolCalls[0].ResultContentLength)
	assert.Empty(t, stored[0].ToolCalls[0].ResultEvents)
}
