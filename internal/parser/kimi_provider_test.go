package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimiProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentKimi)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestKimiProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "abc123", "uuid-1", "wire.jsonl")
	newPath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-2",
		"agents",
		"main",
		"wire.jsonl",
	)
	invalidPath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-3",
		"agents",
		"sub agent",
		"wire.jsonl",
	)
	writeSourceFile(t, legacyPath, kimiProviderFixture("legacy question"))
	writeSourceFile(t, newPath, kimiProviderFixture("new layout question"))
	writeSourceFile(t, invalidPath, kimiProviderFixture("bad agent"))
	writeSourceFile(t, filepath.Join(root, "abc123", "uuid-1", "other.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "wire.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, AgentKimi, discovered[0].Provider)
	assert.Equal(t, legacyPath, discovered[0].DisplayPath)
	assert.Equal(t, "abc123", discovered[0].ProjectHint)
	assert.Equal(t, newPath, discovered[1].DisplayPath)
	assert.Equal(t, "kimi-code", discovered[1].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~kimi:abc123:uuid-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, legacyPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, legacyPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "wd_kimi-code_057f5c09ee3f:main:session_uuid-2",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newPath, found.DisplayPath)

	require.NoError(t, os.Remove(legacyPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: legacyPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, legacyPath, changed[0].DisplayPath)
}

func TestKimiProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	targetProject := filepath.Join(targetRoot, "abc123")
	sourceProject := filepath.Join(root, "abc123")
	sourcePath := filepath.Join(sourceProject, "uuid-1", "wire.jsonl")
	writeSourceFile(
		t,
		filepath.Join(targetProject, "uuid-1", "wire.jsonl"),
		kimiProviderFixture("from symlink"),
	)
	if err := os.Symlink(targetProject, sourceProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~kimi:abc123:uuid-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestKimiProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "abc123", "uuid-1", "wire.jsonl")
	writeSourceFile(t, sourcePath, kimiProviderFixture("provider question"))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "kimi:abc123:uuid-1", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func TestKimiProviderParseNewLayoutRoundTrip(t *testing.T) {
	root := t.TempDir()
	rawID := "wd_kimi-code_057f5c09ee3f:main:session_uuid-2"
	sourcePath := filepath.Join(
		root,
		"wd_kimi-code_057f5c09ee3f",
		"session_uuid-2",
		"agents",
		"main",
		"wire.jsonl",
	)
	writeSourceFile(t, sourcePath, kimiProviderFixture("new layout provider question"))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~kimi:" + rawID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, source.DisplayPath)
	assert.Equal(t, "kimi-code", source.ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, "kimi:"+rawID, session.ID)
	assert.Equal(t, "kimi-code", session.Project)
	assert.Equal(t, "devbox", session.Machine)
	assert.Equal(t, sourcePath, session.File.Path)
	assert.Equal(t, "abc123", session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func kimiProviderFixture(firstMessage string) string {
	return `{"type":"metadata","protocol_version":"1.3"}` + "\n" +
		`{"timestamp":1704067200.0,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"` + firstMessage + `"}]}}}` + "\n" +
		`{"timestamp":1704067201.0,"message":{"type":"ContentPart","payload":{"type":"text","text":"Done."}}}` + "\n" +
		`{"timestamp":1704067202.0,"message":{"type":"TurnEnd","payload":{}}}` + "\n"
}
