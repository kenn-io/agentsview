package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestKiroProviderAuthoritativeParsesSQLiteSource exercises the Kiro
// provider end to end now that Kiro is provider-authoritative and the legacy
// package-level entrypoints have been folded away. The provider discovers the
// current-store data.sqlite3 source, fans it out per conversation, and emits a
// force-replace result set so the archive cleanup semantics are preserved.
func TestKiroProviderAuthoritativeParsesSQLiteSource(t *testing.T) {
	root := t.TempDir()
	store := createKiroSQLiteDB(t, root)
	sessionID := "sqlite-session"
	store.addSession(
		t,
		"/home/user/code/kiro-app",
		sessionID,
		readKiroSQLiteFixture(t, "standard_payload.json"),
		1779012000000,
		1779012030000,
	)

	provider, ok := parser.NewProvider(parser.AgentKiro, parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	observation, err := sync.ObserveProviderSource(context.Background(), provider, sync.ProviderObserveRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, observation.Results, 1)

	assert.True(t, observation.ForceReplace)
	session := observation.Results[0].Session
	assert.Equal(t, "kiro:"+sessionID, session.ID)
	assert.Equal(t, parser.AgentKiro, session.Agent)
	assert.Equal(t, "devbox", session.Machine)
	assert.Equal(t, store.path+"#"+sessionID, session.File.Path)
	assert.NotEmpty(t, observation.Results[0].Messages)
	assert.Equal(t, []string{session.ID}, observation.Planned.DataVersionSessionIDs())
	assert.Empty(t, observation.Planned.Diagnostics)
}

// TestKiroIDEProviderAuthoritativeParsesWorkspaceSession exercises the Kiro
// IDE provider end to end after the fold: discovery and parse own the
// workspace-sessions JSON source without any legacy package-level entrypoint.
func TestKiroIDEProviderAuthoritativeParsesWorkspaceSession(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(
		root,
		"workspace-sessions",
		"encoded-workspace",
		"new-session.json",
	)
	writeKiroIDEProviderSource(t, sourcePath, "New IDE question")

	provider, ok := parser.NewProvider(parser.AgentKiroIDE, parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	observation, err := sync.ObserveProviderSource(context.Background(), provider, sync.ProviderObserveRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, observation.Results, 1)

	session := observation.Results[0].Session
	assert.Equal(t, parser.AgentKiroIDE, session.Agent)
	assert.Equal(t, "devbox", session.Machine)
	assert.Equal(t, observation.Fingerprint.Hash, session.File.Hash)
	assert.NotEmpty(t, observation.Results[0].Messages)
	assert.Equal(t, []string{session.ID}, observation.Planned.DataVersionSessionIDs())
	assert.Empty(t, observation.Planned.Diagnostics)
}

func writeKiroIDEProviderSource(t *testing.T, path, question string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"sessionId":"new-session",`+
			`"title":"New title",`+
			`"workspaceDirectory":"/home/user/dev/new-app",`+
			`"history":[`+
			`{"message":{"role":"user","content":"`+question+`","id":"m1"}},`+
			`{"message":{"role":"assistant","content":"New IDE answer","id":"m2"}}`+
			`]}`+"\n",
	), 0o644))
}
