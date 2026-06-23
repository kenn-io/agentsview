package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenHandsProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentOpenHands)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentOpenHands, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestOpenHandsProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	dirName := "086c7ecf6cb746b69fbcb900358d1247"
	sessionDir := openHandsProviderWriteSession(
		t, root, dirName, sessionID, "provider question",
	)
	openHandsProviderWriteInvalidSession(t, root, "missing-events")
	writeSourceFile(t, filepath.Join(root, "notes.txt"), "{}\n")

	provider, ok := NewProvider(AgentOpenHands, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.NotEmpty(t, plan.Roots[0].DebounceKey)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentOpenHands, discovered[0].Provider)
	assert.Equal(t, sessionDir, discovered[0].Key)
	assert.Equal(t, sessionDir, discovered[0].DisplayPath)
	assert.Equal(t, sessionDir, discovered[0].FingerprintKey)
	assert.Empty(t, discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~openhands:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sessionDir, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: dirName,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sessionDir, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: sessionDir,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sessionDir, found.DisplayPath)

	snapshot, err := OpenHandsSnapshot(sessionDir)
	require.NoError(t, err)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sessionDir, fingerprint.Key)
	assert.Equal(t, snapshot.Size, fingerprint.Size)
	assert.Equal(t, snapshot.Mtime, fingerprint.MTimeNS)
	assert.Equal(t, snapshot.Hash, fingerprint.Hash)

	for _, changedPath := range []string{
		sessionDir,
		filepath.Join(sessionDir, "base_state.json"),
		filepath.Join(sessionDir, "TASKS.json"),
		filepath.Join(sessionDir, "events", "event-00000-user.json"),
	} {
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: changedPath, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1, changedPath)
		assert.Equal(t, sessionDir, changed[0].DisplayPath)
	}

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(sessionDir, "events", "notes.txt"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

	wrongRoot, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      sessionDir,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, wrongRoot)
}

func TestOpenHandsProviderParse(t *testing.T) {
	root := t.TempDir()
	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	sessionDir := openHandsProviderWriteSession(
		t, root, "086c7ecf6cb746b69fbcb900358d1247", sessionID, "parse question",
	)
	provider, ok := NewProvider(AgentOpenHands, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "openhands:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, AgentOpenHands, result.Result.Session.Agent)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, sessionDir, result.Result.Session.File.Path)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, "parse question", result.Result.Session.FirstMessage)
	assert.Len(t, result.Result.Messages, 1)
}

func openHandsProviderWriteSession(
	t *testing.T,
	root string,
	dirName string,
	sessionID string,
	firstMessage string,
) string {
	t.Helper()
	sessionDir := filepath.Join(root, dirName)
	eventsDir := filepath.Join(sessionDir, "events")
	require.NoError(t, os.MkdirAll(eventsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "base_state.json"),
		[]byte(`{"id":"`+sessionID+`","agent":{"llm":{"model":"test-model"}}}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "TASKS.json"),
		[]byte(`[]`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(eventsDir, "event-00000-user.json"),
		[]byte(`{
			"id":"e0",
			"timestamp":"2026-04-02T15:25:40.706887",
			"source":"user",
			"llm_message":{"role":"user","content":[{"type":"text","text":"`+firstMessage+`"}]},
			"kind":"MessageEvent"
		}`),
		0o644,
	))
	return sessionDir
}

func openHandsProviderWriteInvalidSession(
	t *testing.T,
	root string,
	dirName string,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, dirName), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, dirName, "base_state.json"),
		[]byte(`{}`),
		0o644,
	))
}
