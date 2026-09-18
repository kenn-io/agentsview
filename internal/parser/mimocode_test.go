package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiMoCodeProviderParseRelabelsOpenCodeSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "mimocode:ses_mimo",
	})
	require.NoError(err)
	require.True(found)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  source,
		Machine: "testmachine",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(msgs, 1)

	assert.Equal("mimocode:ses_mimo", sess.ID)
	assert.Equal("mimocode:ses_parent", sess.ParentSessionID)
	assert.Equal(AgentMiMoCode, sess.Agent)
	assert.Equal("mimoapp", sess.Project)
	assert.Equal("Hello from MiMoCode", msgs[0].Content)
}

func TestMiMoCodeProviderDiscoversSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	assert.Equal(sessionPath, sources[0].DisplayPath)
	assert.Equal("mimoapp", sources[0].ProjectHint)
	assert.Equal(AgentMiMoCode, sources[0].Provider)
}

func TestMiMoCodeSQLiteVirtualPathRoundTrips(t *testing.T) {
	assert := assert.New(t)

	wantDBPath := filepath.Join(t.TempDir(), "mimocode.db")
	virtual := MiMoCodeSQLiteVirtualPath(wantDBPath, "ses_mimo")
	dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(mimoFmt.dbName, virtual)
	require.True(t, ok)
	assert.Equal(wantDBPath, dbPath)
	assert.Equal("ses_mimo", sessionID)

	_, _, ok = parseOpenCodeFormatVirtualPath(
		mimoFmt.dbName,
		filepath.Join(t.TempDir(), "opencode.db")+"#ses_mimo",
	)
	assert.False(ok)
}
