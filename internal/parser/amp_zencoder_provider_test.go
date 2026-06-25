package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAmpProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentAmp)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentAmp, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestAmpProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	threadID := "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd"
	sourcePath := filepath.Join(root, threadID+".json")
	writeSourceFile(t, sourcePath, ampProviderFixture(threadID))
	writeSourceFile(t, filepath.Join(root, "T-.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "notes.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", threadID+".json"), "{}\n")

	provider, ok := NewProvider(AgentAmp, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentAmp, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~amp:" + threadID,
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

func TestAmpProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	threadID := "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd"
	targetPath := filepath.Join(targetDir, threadID+".json")
	sourcePath := filepath.Join(root, threadID+".json")
	writeSourceFile(t, targetPath, ampProviderFixture(threadID))
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentAmp, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~amp:" + threadID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestAmpProviderParse(t *testing.T) {
	root := t.TempDir()
	threadID := "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd"
	sourcePath := filepath.Join(root, threadID+".json")
	content := ampProviderFixture(threadID)
	writeSourceFile(t, sourcePath, content)

	provider, ok := NewProvider(AgentAmp, ProviderConfig{
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
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "amp:"+threadID, outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "amp-project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		outcome.Results[0].Result.Session.File.Hash,
	)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func TestZencoderProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentZencoder)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestZencoderProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "abc-def-123.jsonl")
	writeSourceFile(t, sourcePath, zencoderProviderFixture("abc-def-123"))
	writeSourceFile(t, filepath.Join(root, "notes.txt"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "abc-def-123.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentZencoder, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~zencoder:abc-def-123",
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

func TestZencoderProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "abc-def-123.jsonl")
	sourcePath := filepath.Join(root, "abc-def-123.jsonl")
	writeSourceFile(t, targetPath, zencoderProviderFixture("abc-def-123"))
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~zencoder:abc-def-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestZencoderProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "abc-def-123.jsonl")
	content := zencoderProviderFixture("abc-def-123")
	writeSourceFile(t, sourcePath, content)

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
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
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "zencoder:abc-def-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "sample_project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		outcome.Results[0].Result.Session.File.Hash,
	)
	assert.Len(t, outcome.Results[0].Result.Messages, 3)
}

func ampProviderFixture(threadID string) string {
	return `{
  "v": 1,
  "id": "` + threadID + `",
  "created": 1704067200000,
  "title": "Migrate database schema",
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "Migrate the DB schema."}]},
    {"role": "assistant", "content": [{"type": "text", "text": "Sure, I will help."}]}
  ],
  "env": {"initial": {"trees": [{"displayName": "amp-project"}]}},
  "meta": {"traces": []}
}`
}

func zencoderProviderFixture(sessionID string) string {
	return strings.Join([]string{
		`{"id":"` + sessionID + `","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`,
		`{"role":"system","content":"Working directory: /Users/alice/code/sample-project"}`,
		`{"role":"user","content":[{"type":"text","text":"hello"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"OK."}]}`,
	}, "\n")
}
