package parser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiloProviderParseRelabelsOpenCodeSession(t *testing.T) {
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

	provider, ok := NewProvider(AgentKilo, ProviderConfig{
		Roots:   []string{root},
		Machine: "testmachine",
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "kilo:ses_kilo",
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

	assert.Equal(t, "kilo:ses_kilo", sess.ID)
	assert.Equal(t, "kilo:ses_parent", sess.ParentSessionID)
	assert.Equal(t, AgentKilo, sess.Agent)
	assert.Equal(t, "kiloapp", sess.Project)
	assert.Equal(t, "Hello from Kilo", msgs[0].Content)
}

func TestKiloProviderDiscoversSessions(t *testing.T) {
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

	provider, ok := NewProvider(AgentKilo, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	assert.Equal(t, sessionPath, sources[0].DisplayPath)
	assert.Equal(t, "kiloapp", sources[0].ProjectHint)
	assert.Equal(t, AgentKilo, sources[0].Provider)
}

func TestKiloSQLiteVirtualPathRoundTrips(t *testing.T) {
	wantDBPath := filepath.Join(t.TempDir(), "kilo.db")
	virtual := KiloSQLiteVirtualPath(wantDBPath, "ses_kilo")
	dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(kiloFmt.dbName, virtual)
	require.True(t, ok)
	assert.Equal(t, wantDBPath, dbPath)
	assert.Equal(t, "ses_kilo", sessionID)

	_, _, ok = parseOpenCodeFormatVirtualPath(
		kiloFmt.dbName,
		filepath.Join(t.TempDir(), "opencode.db")+"#ses_kilo",
	)
	assert.False(t, ok)
}
