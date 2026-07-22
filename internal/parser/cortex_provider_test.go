package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCortexProviderCapabilities(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentCortex)
	require.True(t, ok)
	require.NotNil(t, factory)

	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(t, CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(t, CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(t, CapabilitySupported, caps.Source.FindSource)
	assert.Equal(t, CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.SessionName)
	assert.Equal(t, CapabilitySupported, caps.Content.Cwd)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestCortexProviderWatchRootsStayBounded(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, transcript, "{}\n")
	companionCalls := 0
	provider := &cortexProvider{
		ProviderBase: ProviderBase{
			Def:  AgentDef{Type: AgentCortex},
			Caps: cortexProviderCapabilities(),
		},
		sources: NewJSONLSourceSet(
			AgentCortex,
			[]string{root},
			WithExtensions(".json"),
			WithCompanionFiles(func(path string) []string {
				companionCalls++
				return []string{cortexHistoryCompanionPath(path)}
			}),
		),
	}

	assert.Equal(t, CapabilitySupported, provider.Capabilities().Source.WatchRoots)
	assert.Implements(t, (*WatchRootPlanner)(nil), provider)
	roots, err := ResolveWatchRoots(context.Background(), provider)
	require.NoError(t, err)
	assert.Equal(t, []WatchRoot{{
		Path:        root,
		DebounceKey: string(AgentCortex) + ":jsonl:" + root,
	}}, roots)
	assert.Zero(t, companionCalls,
		"bounded root scheduling must not discover transcript companions")

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, cortexTestUUID+".history.jsonl")
	assert.Equal(t, 1, companionCalls,
		"legacy WatchPlan must retain companion glob discovery")
}

func TestCortexProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	otherID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	otherPath := filepath.Join(root, otherID+".json")
	writeSourceFile(t, sourcePath, minimalCortexSession(cortexTestUUID))
	writeSourceFile(t, otherPath, minimalCortexSession(otherID))
	writeSourceFile(t, filepath.Join(root, cortexTestUUID+".history.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, cortexTestUUID+".back.123.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "has spaces.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", cortexTestUUID+".json"), "{}\n")

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, []string{sourcePath, otherPath}, sourceDisplayPaths(discovered))
	assert.Equal(t, []string{"", ""}, sourceProjects(discovered))

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	// The shared companion mechanism watches each discovered session's specific
	// .history.jsonl sidecar (not a wildcard), so a sidecar change on a known
	// session is observed live; new sessions are picked up on rediscovery.
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.json")
	assert.Contains(t, plan.Roots[0].IncludeGlobs, cortexTestUUID+".history.jsonl")
	assert.Contains(t, plan.Roots[0].IncludeGlobs, otherID+".history.jsonl")

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~cortex:" + cortexTestUUID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.NotZero(t, fingerprint.Size)
	assert.NotZero(t, fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: otherPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, otherPath, found.DisplayPath)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "../" + cortexTestUUID,
	})
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCortexProviderClassifiesAndFingerprintsHistoryCompanion(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	historyPath := filepath.Join(root, cortexTestUUID+".history.jsonl")
	writeSourceFile(t, sourcePath, `{
		"session_id":"`+cortexTestUUID+`",
		"working_directory":"/home/user/project"
	}`)
	writeSourceFile(
		t,
		historyPath,
		`{"role":"user","id":"m1","content":[{"type":"text","text":"from history"}]}`+"\n",
	)

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: historyPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
	assert.Equal(t, sourcePath, changed[0].FingerprintKey)

	before, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, sourcePath, before.Key)
	assert.NotEmpty(t, before.Hash)

	writeSourceFile(
		t,
		historyPath,
		`{"role":"user","id":"m1","content":[{"type":"text","text":"updated history"}]}`+"\n",
	)
	after, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, sourcePath, after.Key)
	assert.NotEqual(t, before.Hash, after.Hash)
	assert.NotEqual(t, before.Size, after.Size)

	require.NoError(t, os.Remove(historyPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: historyPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCortexProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	targetPath := filepath.Join(targetRoot, cortexTestUUID+".json")
	writeSourceFile(t, targetPath, minimalCortexSession(cortexTestUUID))
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~cortex:" + cortexTestUUID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestCortexProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, sourcePath, minimalCortexSession(cortexTestUUID))

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
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
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "cortex:"+cortexTestUUID, result.Result.Session.ID)
	assert.Equal(t, "project", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, "abc123", result.Result.Session.File.Hash)
	assert.Equal(t, "Test session", result.Result.Session.SessionName)
	assert.Len(t, result.Result.Messages, 2)
}

// TestCortexProviderFingerprintIncludesContentHash guards that the Cortex
// provider computes a full-file content hash. The legacy per-agent parse stored
// a file_hash; without WithContentHashing the provider fingerprint hash is empty
// and a resync clears the stored file_hash to NULL. Toggle-provable: removing
// WithContentHashing from newCortexSourceSet makes fp.Hash empty and fails here.
func TestCortexProviderFingerprintIncludesContentHash(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, sourcePath, minimalCortexSession(cortexTestUUID))

	provider, ok := NewProvider(AgentCortex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fp, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	require.NotEmpty(t, fp.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, fp.Hash, outcome.Results[0].Result.Session.File.Hash)
}
