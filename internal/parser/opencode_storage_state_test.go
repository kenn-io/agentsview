package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatOpenCodeStorageSessionState(t *testing.T) {
	root := t.TempDir()
	sessionPath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_state", "state-app", "State Session",
	)

	state, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok, "state capture must succeed")
	require.NotEmpty(t, state)

	again, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok)
	assert.Equal(t, state, again, "untouched tree must produce equal states")

	t.Run("added part changes state", func(t *testing.T) {
		writeOpenCodeStorageFile(t, filepath.Join(
			root, "storage", "part", "msg_1", "prt_2.json",
		), map[string]any{
			"id":        "prt_2",
			"sessionID": "ses_state",
			"messageID": "msg_1",
			"type":      "text",
			"text":      "appended part",
			"time":      map[string]any{"created": int64(1700000001000)},
		})
		next, ok := StatOpenCodeStorageSessionState(sessionPath)
		require.True(t, ok)
		assert.NotEqual(t, state, next,
			"a new part file must change the state")
		state = next
	})

	t.Run("part mtime bump changes state", func(t *testing.T) {
		partPath := filepath.Join(
			root, "storage", "part", "msg_1", "prt_2.json",
		)
		bumped := time.Unix(1810000000, 123456789)
		require.NoError(t, os.Chtimes(partPath, bumped, bumped))
		next, ok := StatOpenCodeStorageSessionState(sessionPath)
		require.True(t, ok)
		assert.NotEqual(t, state, next,
			"a part mtime change must change the state")
		state = next
	})

	t.Run("size change with restored mtime changes state", func(t *testing.T) {
		partPath := filepath.Join(
			root, "storage", "part", "msg_1", "prt_2.json",
		)
		info, err := os.Stat(partPath)
		require.NoError(t, err)
		writeOpenCodeStorageFile(t, partPath, map[string]any{
			"id":        "prt_2",
			"sessionID": "ses_state",
			"messageID": "msg_1",
			"type":      "text",
			"text":      "appended part grown longer",
			"time":      map[string]any{"created": int64(1700000001000)},
		})
		require.NoError(t,
			os.Chtimes(partPath, info.ModTime(), info.ModTime()))
		next, ok := StatOpenCodeStorageSessionState(sessionPath)
		require.True(t, ok)
		assert.NotEqual(t, state, next,
			"a size change must change the state even at a restored mtime")
	})

	t.Run("missing session file fails capture", func(t *testing.T) {
		_, ok := StatOpenCodeStorageSessionState(
			filepath.Join(root, "storage", "session", "global", "nope.json"),
		)
		assert.False(t, ok, "missing session file must never be trusted")
	})
}

func TestStatOpenCodeStorageSessionStateWithoutMessages(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_lonely.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_lonely",
		"directory": "/home/user/code/lonely-app",
		"title":     "Lonely",
		"time": map[string]any{
			"created": int64(1700000000000),
			"updated": int64(1700000060000),
		},
	})

	state, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok,
		"a session without a message dir is valid and capturable")
	require.NotEmpty(t, state)

	again, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok)
	assert.Equal(t, state, again)
}
