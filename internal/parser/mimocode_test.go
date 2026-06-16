package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMiMoCodeFileRelabelsOpenCodeSession(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session_diff", "global", "ses_mimo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_mimo",
		"parentID":  "ses_parent",
		"directory": "/home/user/code/mimoapp",
		"title":     "MiMoCode Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_mimo", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_mimo",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_mimo",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from MiMoCode",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseMiMoCodeFile(sessionPath, "testmachine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)

	assert.Equal(t, "mimocode:ses_mimo", sess.ID)
	assert.Equal(t, "mimocode:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentMiMoCode, sess.Agent)
	assert.Equal(t, "mimoapp", sess.Project)
	assert.Equal(t, "Hello from MiMoCode", msgs[0].Content)
}

func TestDiscoverMiMoCodeSessions(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session_diff", "global", "ses_mimo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_mimo",
		"directory": "/home/user/code/mimoapp",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})

	files := DiscoverMiMoCodeSessions(root)
	require.Len(t, files, 1)

	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, "mimoapp", files[0].Project)
	assert.Equal(t, AgentMiMoCode, files[0].Agent)
}

func TestParseMiMoCodeSQLiteVirtualPath(t *testing.T) {
	wantDBPath := filepath.Join(t.TempDir(), "mimocode.db")
	virtual := wantDBPath + "#ses_mimo"
	dbPath, sessionID, ok := ParseMiMoCodeSQLiteVirtualPath(virtual)
	require.True(t, ok)
	assert.Equal(t, wantDBPath, dbPath)
	assert.Equal(t, "ses_mimo", sessionID)

	_, _, ok = ParseMiMoCodeSQLiteVirtualPath(
		filepath.Join(t.TempDir(), "opencode.db") + "#ses_mimo",
	)
	assert.False(t, ok)
}
