package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseIcodemateFileRelabelsOpenCodeSession(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session_diff", "global", "ses_icode.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_icode",
		"parentID":  "ses_parent",
		"directory": "/home/user/code/icodeapp",
		"title":     "IcodeMate Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_icode", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_icode",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_icode",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from IcodeMate",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseIcodemateFile(sessionPath, "testmachine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)

	assert.Equal(t, "icodemate:ses_icode", sess.ID)
	assert.Equal(t, "icodemate:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentIcodemate, sess.Agent)
	assert.Equal(t, "icodeapp", sess.Project)
	assert.Equal(t, "Hello from IcodeMate", msgs[0].Content)
}

func TestDiscoverIcodemateSessions(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session_diff", "global", "ses_icode.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_icode",
		"directory": "/home/user/code/icodeapp",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})

	files := DiscoverIcodemateSessions(root)
	require.Len(t, files, 1)

	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, "icodeapp", files[0].Project)
	assert.Equal(t, AgentIcodemate, files[0].Agent)
}

func TestParseIcodemateSQLiteVirtualPath(t *testing.T) {
	wantDBPath := filepath.Join(t.TempDir(), "icodemate.db")
	virtual := wantDBPath + "#ses_icode"
	dbPath, sessionID, ok := ParseIcodemateSQLiteVirtualPath(virtual)
	require.True(t, ok)
	assert.Equal(t, wantDBPath, dbPath)
	assert.Equal(t, "ses_icode", sessionID)

	_, _, ok = ParseIcodemateSQLiteVirtualPath(
		filepath.Join(t.TempDir(), "opencode.db") + "#ses_icode",
	)
	assert.False(t, ok)
}

func TestDiscoverIcodemateSessionsEmptyDir(t *testing.T) {
	root := t.TempDir()
	files := DiscoverIcodemateSessions(root)
	assert.Empty(t, files)
}

func TestDiscoverIcodemateSessionsNoSessionDiff(t *testing.T) {
	root := t.TempDir()
	writeOpenCodeStorageFile(t,
		filepath.Join(root, "storage", "other", "x.json"),
		map[string]any{"id": "x"},
	)
	files := DiscoverIcodemateSessions(root)
	assert.Empty(t, files)
}
