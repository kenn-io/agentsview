package parser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiMoCodeProviderParseRelabelsOpenCodeSession(t *testing.T) {
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

	provider, ok := NewProvider(AgentMiMoCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "testmachine",
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "mimocode:ses_mimo",
	})
	require.NoError(t, err)
	require.True(t, found)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  source,
		Machine: "testmachine",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 1)

	assert.Equal(t, "mimocode:ses_mimo", sess.ID)
	assert.Equal(t, "mimocode:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentMiMoCode, sess.Agent)
	assert.Equal(t, "mimoapp", sess.Project)
	assert.Equal(t, "Hello from MiMoCode", msgs[0].Content)
}

func TestMiMoCodeProviderDiscoversSessions(t *testing.T) {
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

	provider, ok := NewProvider(AgentMiMoCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	assert.Equal(t, sessionPath, sources[0].DisplayPath)
	assert.Equal(t, "mimoapp", sources[0].ProjectHint)
	assert.Equal(t, AgentMiMoCode, sources[0].Provider)
}

func TestMiMoCodeSQLiteVirtualPathRoundTrips(t *testing.T) {
	wantDBPath := filepath.Join(t.TempDir(), "mimocode.db")
	virtual := MiMoCodeSQLiteVirtualPath(wantDBPath, "ses_mimo")
	dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(mimoFmt.dbName, virtual)
	require.True(t, ok)
	assert.Equal(t, wantDBPath, dbPath)
	assert.Equal(t, "ses_mimo", sessionID)

	_, _, ok = parseOpenCodeFormatVirtualPath(
		mimoFmt.dbName,
		filepath.Join(t.TempDir(), "opencode.db")+"#ses_mimo",
	)
	assert.False(t, ok)
}
