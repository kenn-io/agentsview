package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandCodeProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentCommandCode)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestCommandCodeProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	sourcePath := filepath.Join(projectDir, "sess_123.jsonl")
	writeSourceFile(t, sourcePath, commandCodeProviderFixture())
	writeSourceFile(t, filepath.Join(projectDir, "sess_123.checkpoints.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(projectDir, "sess_123.prompts.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentCommandCode, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Empty(t, discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~commandcode:sess_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FingerprintKey: sourcePath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCommandCodeProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	realProjectDir := filepath.Join(t.TempDir(), "real-project")
	linkProjectDir := filepath.Join(root, "linked-project")
	if err := os.Symlink(realProjectDir, linkProjectDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	sourcePath := filepath.Join(linkProjectDir, "sess_123.jsonl")
	writeSourceFile(t, filepath.Join(realProjectDir, "sess_123.jsonl"), commandCodeProviderFixture())

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestCommandCodeProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
		Fingerprint: SourceFingerprint{
			Key: sourcePath,
		},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "commandcode:sess_123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(transcript))),
		outcome.Results[0].Result.Session.File.Hash,
	)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func TestCommandCodeProviderParsePreservesTranscriptFileHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "sess_123.jsonl")
	transcript := commandCodeProviderFixture()
	writeSourceFile(t, sourcePath, transcript)
	writeSourceFile(t, commandCodeMetaCompanionPath(sourcePath), `{"title":"Renamed"}`)

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	transcriptHash := fmt.Sprintf("%x", sha256.Sum256([]byte(transcript)))
	require.NotEqual(t, transcriptHash, fingerprint.Hash,
		"fixture must prove metadata participates in freshness separately")

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, transcriptHash, outcome.Results[0].Result.Session.File.Hash)
}

func commandCodeProviderFixture() string {
	return `{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess_123","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2,"cwd":"/Users/alice/code/sample-project"}}
{"id":"m2","timestamp":"2026-06-01T10:00:03Z","sessionId":"sess_123","role":"assistant","content":[{"type":"text","text":"The error is in the startup path."}],"gitBranch":"feature/command-code","metadata":{"version":2}}`
}
