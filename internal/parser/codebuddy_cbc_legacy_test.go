package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Remote sync mirrors a CodeBuddy home's history directory as its own 4.x
// root; a converted conversation still finds its 4.x directory there, and a
// change inside it still names the conversation.
func TestCodeBuddyLegacyHistoryRootIsItsOwnHistory(t *testing.T) {
	home := t.TempDir()
	history := filepath.Join(home, "history")
	convDir := filepath.Join(history, "ws", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[]}`), 0o600))

	assert.Equal(t, []string{history}, codebuddyLegacyHistoryDirsFor(t.Context(), history))
	assert.Equal(t, convDir, codebuddyLegacySessionDir(t.Context(), []string{history}, "", "conv1"))

	changed := filepath.Join(convDir, "messages", "a1.json")
	for _, root := range []string{home, history} {
		id, ok := codebuddyLegacyConversationID(root, changed)
		assert.True(t, ok, root)
		assert.Equal(t, "conv1", id, root)
	}
}
