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

func TestQwenProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentQwen)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentQwen, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestQwenProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "-Users-alice-code-sample-project")
	sourcePath := filepath.Join(projectDir, "chats", "session-123.jsonl")
	nonIDPath := filepath.Join(projectDir, "chats", "2025.01.01.jsonl")
	writeSourceFile(t, sourcePath, qwenProviderFixture("session-123"))
	writeSourceFile(t, nonIDPath, qwenProviderFixture("header-session-id"))
	writeSourceFile(t, filepath.Join(projectDir, "notes", "skip.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "root-session.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(projectDir, "chats", "nested", "deep.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentQwen, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, []string{nonIDPath, sourcePath}, sourceDisplayPaths(discovered))
	assert.Equal(t, []string{"sample_project", "sample_project"}, sourceProjects(discovered))

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~qwen:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "2025.01.01",
	})
	require.NoError(t, err)
	assert.False(t, ok)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: nonIDPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, nonIDPath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestQwenProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-alice-code-sample-project", "chats", "session-123.jsonl")
	targetPath := filepath.Join(targetDir, "chats", "session-123.jsonl")
	writeSourceFile(t, targetPath, qwenProviderFixture("session-123"))
	if err := os.Symlink(targetDir, filepath.Join(root, "-Users-alice-code-sample-project")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentQwen, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~qwen:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestQwenProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-alice-code-sample-project", "chats", "session-123.jsonl")
	writeSourceFile(t, sourcePath, qwenProviderFixture("session-123"))

	provider, ok := NewProvider(AgentQwen, ProviderConfig{
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
	assert.Equal(t, "qwen:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "sample_project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

func TestQwenProviderFingerprintIncludesContentHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-alice-code-sample-project", "chats", "session-123.jsonl")
	writeSourceFile(t, sourcePath, qwenProviderFixture("session-123"))

	provider, ok := NewProvider(AgentQwen, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	// The legacy processQwen path persisted a full-file content hash; the
	// migrated provider must too, or a resync clears the stored file_hash.
	require.NotEmpty(t, fp.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, fp.Hash, outcome.Results[0].Result.Session.File.Hash)
}

// TestQwenProviderFindSourceResolvesStoredPathOutsideRoots locks in single-
// session resync parity with the legacy processQwen path: when the DB-stored
// file_path points outside any configured QWEN_PROJECTS_DIR, FindSource must
// still resolve it from the path's implicit <root>/<project>/chats/ layout and
// recover the project as ProjectHint, so a reparse keeps the canonical
// qwen:<stem> ID instead of failing with "provider source not found".
func TestQwenProviderFindSourceResolvesStoredPathOutsideRoots(t *testing.T) {
	storedRoot := t.TempDir()
	sourcePath := filepath.Join(
		storedRoot, "-Users-alice-code-sample-project", "chats", "session-123.jsonl",
	)
	writeSourceFile(t, sourcePath, qwenProviderFixture("session-123"))

	// Provider is configured with an unrelated root, so the in-root lookup
	// cannot match the stored path.
	provider, ok := NewProvider(AgentQwen, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: sourcePath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
	assert.Equal(t, "sample_project", found.ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  found,
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "qwen:session-123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "sample_project", outcome.Results[0].Result.Session.Project)

	// A stored path that is not a valid Qwen source shape stays unresolved.
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: filepath.Join(storedRoot, "loose.jsonl"),
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func qwenProviderFixture(sessionID string) string {
	return strings.Join([]string{
		`{"uuid":"u1","sessionId":"` + sessionID + `","timestamp":"2026-05-05T11:08:38.572Z","type":"user","cwd":"/Users/alice/code/sample-project","message":{"role":"user","parts":[{"text":"Calculate .089 * 7.85788"}]}}`,
		`{"uuid":"u2","sessionId":"` + sessionID + `","timestamp":"2026-05-05T11:08:46.529Z","type":"assistant","cwd":"/Users/alice/code/sample-project","model":"qwen","message":{"role":"model","parts":[{"text":"The user wants multiplication.","thought":true},{"text":"0.089 times 7.85788 is 0.69935132"}]},"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":5}}`,
	}, "\n")
}
