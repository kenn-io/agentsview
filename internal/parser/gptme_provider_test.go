package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGptmeProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentGptme)
	require.True(t, ok)
	require.NotNil(t, factory)

	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(t, CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(t, CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(t, CapabilitySupported, caps.Source.FindSource)
	assert.Equal(t, CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(t, CapabilityNotApplicable, caps.Source.MultiSessionSource)
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.Model)
	assert.Equal(t, CapabilitySupported, caps.Content.PerMessageTokenUsage)

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestGptmeProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())
	writeSourceFile(t, filepath.Join(root, "conversation.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", sessionID, "conversation.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "other", "notes.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentGptme, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].Key)
	assert.Equal(t, sourcePath, discovered[0].FingerprintKey)
	assert.Equal(t, "write-hello-world", discovered[0].ProjectHint)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, discovered[0].Key, changed[0].Key)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, discovered[0].Key, found.Key)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.NotZero(t, fingerprint.Size)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)
}

func TestGptmeProviderDiscoversSymlinkSessionDirectories(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	targetDir := filepath.Join(targetRoot, sessionID)
	writeSourceFile(
		t,
		filepath.Join(targetDir, "conversation.jsonl"),
		gptmeProviderFixture(),
	)
	linkDir := filepath.Join(root, sessionID)
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Skipf("creating directory symlink: %v", err)
	}

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, filepath.Join(linkDir, "conversation.jsonl"), discovered[0].DisplayPath)
}

func TestGptmeProviderClassifiesDeletedConversationPath(t *testing.T) {
	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NoError(t, os.Remove(sourcePath))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      sourcePath,
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].Key)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
	assert.Equal(t, "write-hello-world", changed[0].ProjectHint)
}

func TestGptmeProviderFindSourceUsesPersistedFallbacks(t *testing.T) {
	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	for _, req := range []FindSourceRequest{
		{FingerprintKey: sourcePath},
		{FullSessionID: "gptme:" + sessionID},
		{FullSessionID: "host~gptme:" + sessionID},
	} {
		found, ok, err := provider.FindSource(context.Background(), req)
		require.NoError(t, err)
		require.Truef(t, ok, "request %#v", req)
		assert.Equal(t, sourcePath, found.DisplayPath)
	}
}

func TestGptmeProviderParse(t *testing.T) {
	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: sourcePath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	assert.False(t, outcome.ForceReplace)
	assert.Empty(t, outcome.SourceErrors)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Empty(t, result.RetryReason)
	assert.Equal(t, "gptme:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, "write-hello-world", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	require.Len(t, result.Result.Messages, 2)
	assert.Equal(t, RoleUser, result.Result.Messages[0].Role)
	assert.Equal(t, RoleAssistant, result.Result.Messages[1].Role)
}

func TestGptmeProviderParseMissingSourceIsWholeSourceError(t *testing.T) {
	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentGptme,
			Key:            "/tmp/missing/conversation.jsonl",
			DisplayPath:    "/tmp/missing/conversation.jsonl",
			FingerprintKey: "/tmp/missing/conversation.jsonl",
		},
		Machine: "devbox",
	})
	require.Error(t, err)
	assert.Empty(t, outcome)
	assert.False(t, errors.Is(err, ErrUnsupportedProviderFeature))
}

func gptmeProviderFixture() string {
	return `{"role":"user","content":"Write hello world.","timestamp":"2026-06-13T10:00:01.000000"}` + "\n" +
		`{"role":"assistant","content":"Hello from gptme.","timestamp":"2026-06-13T10:00:02.000000","metadata":{"model":"demo-model","usage":{"input_tokens":10,"output_tokens":4}}}` + "\n"
}
