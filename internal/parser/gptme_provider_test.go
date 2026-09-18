package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGptmeProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentGptme)
	require.True(ok)
	require.NotNil(factory)

	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(CapabilitySupported, caps.Source.FindSource)
	assert.Equal(CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(CapabilityNotApplicable, caps.Source.MultiSessionSource)
	assert.Equal(CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(CapabilitySupported, caps.Content.Model)
	assert.Equal(CapabilitySupported, caps.Content.PerMessageTokenUsage)

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestGptmeProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentGptme, discovered[0].Provider)
	assert.Equal(sourcePath, discovered[0].Key)
	assert.Equal(sourcePath, discovered[0].FingerprintKey)
	assert.Equal("write-hello-world", discovered[0].ProjectHint)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(discovered[0].Key, changed[0].Key)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(discovered[0].Key, found.Key)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.NotZero(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)
}

func TestGptmeProviderDiscoversSymlinkSessionDirectories(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(t, filepath.Join(linkDir, "conversation.jsonl"), discovered[0].DisplayPath)
}

func TestGptmeProviderClassifiesDeletedConversationPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	require.NoError(os.Remove(sourcePath))

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      sourcePath,
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].Key)
	assert.Equal(sourcePath, changed[0].DisplayPath)
	assert.Equal("write-hello-world", changed[0].ProjectHint)
}

func TestGptmeProviderFindSourceUsesPersistedFallbacks(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	for _, req := range []FindSourceRequest{
		{FingerprintKey: sourcePath},
		{FullSessionID: "gptme:" + sessionID},
		{FullSessionID: "host~gptme:" + sessionID},
	} {
		found, ok, err := provider.FindSource(t.Context(), req)
		require.NoError(err)
		require.Truef(ok, "request %#v", req)
		assert.Equal(t, sourcePath, found.DisplayPath)
	}
}

func TestGptmeProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "2026-06-13-write-hello-world"
	sourcePath := filepath.Join(root, sessionID, "conversation.jsonl")
	writeSourceFile(t, sourcePath, gptmeProviderFixture())

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: sourcePath,
	})
	require.NoError(err)
	require.True(ok)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	assert.False(outcome.ForceReplace)
	assert.Empty(outcome.SourceErrors)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Empty(result.RetryReason)
	assert.Equal("gptme:"+sessionID, result.Result.Session.ID)
	assert.Equal("write-hello-world", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	require.Len(result.Result.Messages, 2)
	assert.Equal(RoleUser, result.Result.Messages[0].Role)
	assert.Equal(RoleAssistant, result.Result.Messages[1].Role)
}

func TestGptmeProviderParseMissingSourceIsWholeSourceError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentGptme,
			Key:            "/tmp/missing/conversation.jsonl",
			DisplayPath:    "/tmp/missing/conversation.jsonl",
			FingerprintKey: "/tmp/missing/conversation.jsonl",
		},
		Machine: "devbox",
	})
	require.Error(err)
	assert.Empty(outcome)
	assert.NotErrorIs(err, ErrUnsupportedProviderFeature)
}

func gptmeProviderFixture() string {
	return `{"role":"user","content":"Write hello world.","timestamp":"2026-06-13T10:00:01.000000"}` + "\n" +
		`{"role":"assistant","content":"Hello from gptme.","timestamp":"2026-06-13T10:00:02.000000","metadata":{"model":"demo-model","usage":{"input_tokens":10,"output_tokens":4}}}` + "\n"
}
