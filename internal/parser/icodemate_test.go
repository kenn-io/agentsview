package parser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIcodemateProviderParseRelabelsOpenCodeSession exercises the migrated
// path: IcodeMate is provider-authoritative and reuses the shared
// OpenCode-format provider, which parses the storage session and relabels
// it onto the icodemate: ID prefix.
func TestIcodemateProviderParseRelabelsOpenCodeSession(t *testing.T) {
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

	provider, ok := NewProvider(AgentIcodemate, ProviderConfig{
		Roots:   []string{root},
		Machine: "testmachine",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 1)

	assert.Equal(t, "icodemate:ses_icode", sess.ID)
	assert.Equal(t, "icodemate:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentIcodemate, sess.Agent)
	assert.Equal(t, "icodeapp", sess.Project)
	assert.Equal(t, "Hello from IcodeMate", msgs[0].Content)
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
