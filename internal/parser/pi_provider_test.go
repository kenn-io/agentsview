package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentPi)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestOMPProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentOMP, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentOMP, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~omp:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, AgentOMP, found.Provider)
	assert.Equal(t, sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "omp:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, AgentOMP, outcome.Results[0].Result.Session.Agent)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, AgentOMP, changed[0].Provider)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestPiProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	lookupOnlyPath := filepath.Join(root, "encoded-cwd", "lookup-only.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))
	writeSourceFile(t, lookupOnlyPath, `{"type":"message"}`+"\n")
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "notes.txt"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "root-session.jsonl"), piProviderFixture("root-session"))
	writeSourceFile(t, filepath.Join(root, "encoded-cwd", "nested", "deep.jsonl"), piProviderFixture("deep"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentPi, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Empty(t, discovered[0].ProjectHint)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "pi:lookup-only",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, lookupOnlyPath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestPiProviderDiscoveryAcceptsSessionHeaderInNonSessionIDFilename(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "2025.01.01.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("header-session-id"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "pi:header-session-id", outcome.Results[0].Result.Session.ID)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "2025.01.01",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPiProviderDiscoversSymlinkedCWDDirectory(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	sourcePath := filepath.Join(root, "linked-cwd", "session-123.jsonl")
	targetPath := filepath.Join(targetDir, "session-123.jsonl")
	writeSourceFile(t, targetPath, piProviderFixture("session-123"))
	if err := os.Symlink(targetDir, filepath.Join(root, "linked-cwd")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentPi, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~pi:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestPiProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "encoded-cwd", "session-123.jsonl")
	writeSourceFile(t, sourcePath, piProviderFixture("session-123"))

	provider, ok := NewProvider(AgentPi, ProviderConfig{
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
	assert.Equal(t, "pi:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "pi_project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func piProviderFixture(sessionID string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/pi-project"}`,
		`{"type":"message","id":"msg-1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":"Inspect the Pi source."}}`,
		`{"type":"message","id":"msg-2","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":"Looks ready.","model":"claude-opus-4-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
	}, "\n")
}
