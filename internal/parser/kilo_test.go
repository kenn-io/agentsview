package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseKiloFileRelabelsOpenCodeSession(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_kilo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_kilo",
		"parentID":  "ses_parent",
		"directory": "/home/user/code/kiloapp",
		"title":     "Kilo Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_kilo", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_kilo",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_kilo",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from Kilo",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseKiloFile(sessionPath, "testmachine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)

	assert.Equal(t, "kilo:ses_kilo", sess.ID)
	assert.Equal(t, "kilo:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentKilo, sess.Agent)
	assert.Equal(t, "kiloapp", sess.Project)
	assert.Equal(t, "Hello from Kilo", msgs[0].Content)
}

func TestDiscoverKiloSessions(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_kilo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_kilo",
		"directory": "/home/user/code/kiloapp",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})

	files := DiscoverKiloSessions(root)
	require.Len(t, files, 1)

	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, "kiloapp", files[0].Project)
	assert.Equal(t, AgentKilo, files[0].Agent)
}

func TestParseKiloSQLiteVirtualPath(t *testing.T) {
	wantDBPath := filepath.Join(t.TempDir(), "kilo.db")
	virtual := wantDBPath + "#ses_kilo"
	dbPath, sessionID, ok := ParseKiloSQLiteVirtualPath(virtual)
	require.True(t, ok)
	assert.Equal(t, wantDBPath, dbPath)
	assert.Equal(t, "ses_kilo", sessionID)

	_, _, ok = ParseKiloSQLiteVirtualPath(
		filepath.Join(t.TempDir(), "opencode.db") + "#ses_kilo",
	)
	assert.False(t, ok)
}
