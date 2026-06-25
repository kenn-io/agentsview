package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQwenPawProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentQwenPaw)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestQwenPawProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	rootPath := qwenPawProviderWriteSession(
		t, root, "default", "", "root_1", "root question",
	)
	consolePath := qwenPawProviderWriteSession(
		t, root, "default", "console", "console_1", "console question",
	)
	qwenPawProviderWriteSession(
		t, root, "default", ".weixin-legacy", "hidden_1", "hidden",
	)
	deepDir := filepath.Join(root, "default", "sessions", "console", "nested")
	require.NoError(t, os.MkdirAll(deepDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(deepDir, "deep.json"),
		[]byte(qwenPawProviderFixture("deep")),
		0o644,
	))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "default", "dialog"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "default", "dialog", "legacy.jsonl"),
		[]byte("{}\n"),
		0o644,
	))

	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.json"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{rootPath, consolePath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(t, AgentQwenPaw, source.Provider)
		assert.Equal(t, "default", source.ProjectHint)
		assert.Equal(t, source.DisplayPath, source.FingerprintKey)
	}

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~qwenpaw:default:root_1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rootPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "default:console:console_1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, consolePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: rootPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rootPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, rootPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: rootPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, rootPath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(consolePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: consolePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, consolePath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      rootPath,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestQwenPawProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := qwenPawProviderWriteSession(
		t, root, "default", "console", "console_1", "provider question",
	)
	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
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
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "qwenpaw:default:console:console_1", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "default", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
}

// TestQwenPawProviderFindSourceResolvesStoredPathOutsideRoots locks in the
// single-session resync parity: when the DB-stored file_path points outside
// any configured QWENPAW_DIR, FindSource must still resolve it from the path's
// implicit <root>/<workspace>/sessions/ layout and recover the workspace as
// ProjectHint, so a reparse keeps the canonical qwenpaw:<workspace>:<stem> ID
// instead of orphaning it under an empty workspace.
func TestQwenPawProviderFindSourceResolvesStoredPathOutsideRoots(t *testing.T) {
	storedRoot := t.TempDir()
	storedPath := qwenPawProviderWriteSession(
		t, storedRoot, "my_ws", "", "default_1", "outside question",
	)

	// Provider is configured with an unrelated root, so the in-root lookup
	// cannot match the stored path.
	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: storedPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, storedPath, found.DisplayPath)
	assert.Equal(t, "my_ws", found.ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  found,
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "qwenpaw:my_ws:default_1", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "my_ws", outcome.Results[0].Result.Session.Project)

	// A stored path that is not a valid qwenpaw source shape stays unresolved.
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: filepath.Join(storedRoot, "loose.json"),
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestQwenPawProviderDiscoversSymlinkedWorkspace(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	qwenPawProviderWriteSession(
		t, targetRoot, "default", "", "root_1", "from symlink",
	)
	sourceWorkspace := filepath.Join(root, "default")
	targetWorkspace := filepath.Join(targetRoot, "default")
	sourcePath := filepath.Join(sourceWorkspace, "sessions", "root_1.json")
	if err := os.Symlink(targetWorkspace, sourceWorkspace); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Equal(t, "default", discovered[0].ProjectHint)
}

func TestQwenPawProviderPrunesSymlinkedSessionNamespaces(t *testing.T) {
	root := t.TempDir()
	sourcePath := qwenPawProviderWriteSession(
		t, root, "default", "", "root_1", "root question",
	)
	targetDir := filepath.Join(t.TempDir(), "console-target")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetDir, "linked_1.json"),
		[]byte(qwenPawProviderFixture("linked question")),
		0o644,
	))
	linkedDir := filepath.Join(root, "default", "sessions", "linked")
	if err := os.Symlink(targetDir, linkedDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	linkedPath := filepath.Join(linkedDir, "linked_1.json")

	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      linkedPath,
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func qwenPawProviderWriteSession(
	t *testing.T,
	root string,
	workspace string,
	subdir string,
	stem string,
	firstMessage string,
) string {
	t.Helper()
	parts := []string{root, workspace, "sessions"}
	if subdir != "" {
		parts = append(parts, subdir)
	}
	dir := filepath.Join(parts...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, stem+".json")
	require.NoError(t, os.WriteFile(
		path,
		[]byte(qwenPawProviderFixture(firstMessage)),
		0o644,
	))
	return path
}

func qwenPawProviderFixture(firstMessage string) string {
	return `{"agent":{"memory":{"content":[` +
		`[{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"` + firstMessage + `"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.004"},[]],` +
		`[{"id":"a1","name":"Friday","role":"assistant","content":[{"type":"text","text":"Done."}],"metadata":{},"timestamp":"2026-04-19 22:37:35.123"},[]]` +
		`]}}}`
}
