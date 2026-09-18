package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCortexProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentCortex)
	require.True(ok)
	require.NotNil(factory)

	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(CapabilitySupported, caps.Source.FindSource)
	assert.Equal(CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(CapabilitySupported, caps.Content.SessionName)
	assert.Equal(CapabilitySupported, caps.Content.Cwd)
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(CapabilitySupported, caps.Content.ToolResults)

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestCortexProviderWatchRootsStayBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	transcript := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, transcript, "{}\n")
	companionCalls := 0
	provider := &cortexProvider{
		Def:  AgentDef{Type: AgentCortex},
		Caps: cortexProviderCapabilities(),
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

	assert.Equal(CapabilitySupported, provider.Capabilities().Source.WatchRoots)
	assert.Implements((*WatchRootPlanner)(nil), provider)
	roots, err := ResolveWatchRoots(t.Context(), provider)
	require.NoError(err)
	assert.Equal([]WatchRoot{{
		Path:        root,
		DebounceKey: string(AgentCortex) + ":jsonl:" + root,
	}}, roots)
	assert.Zero(companionCalls,
		"bounded root scheduling must not discover transcript companions")

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Contains(plan.Roots[0].IncludeGlobs, cortexTestUUID+".history.jsonl")
	assert.Equal(1, companionCalls,
		"legacy WatchPlan must retain companion glob discovery")
}

func TestCortexProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.Equal([]string{sourcePath, otherPath}, sourceDisplayPaths(discovered))
	assert.Equal([]string{"", ""}, sourceProjects(discovered))

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	// The shared companion mechanism watches each discovered session's specific
	// .history.jsonl sidecar (not a wildcard), so a sidecar change on a known
	// session is observed live; new sessions are picked up on rediscovery.
	assert.Contains(plan.Roots[0].IncludeGlobs, "*.json")
	assert.Contains(plan.Roots[0].IncludeGlobs, cortexTestUUID+".history.jsonl")
	assert.Contains(plan.Roots[0].IncludeGlobs, otherID+".history.jsonl")

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~cortex:" + cortexTestUUID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.NotZero(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: otherPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(otherPath, found.DisplayPath)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "../" + cortexTestUUID,
	})
	require.NoError(err)
	assert.False(ok)

	require.NoError(os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestCortexProviderClassifiesAndFingerprintsHistoryCompanion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: historyPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
	assert.Equal(sourcePath, changed[0].FingerprintKey)

	before, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	assert.Equal(sourcePath, before.Key)
	assert.NotEmpty(before.Hash)

	writeSourceFile(
		t,
		historyPath,
		`{"role":"user","id":"m1","content":[{"type":"text","text":"updated history"}]}`+"\n",
	)
	after, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	assert.Equal(sourcePath, after.Key)
	assert.NotEqual(before.Hash, after.Hash)
	assert.NotEqual(before.Size, after.Size)

	require.NoError(os.Remove(historyPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: historyPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestCortexProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~cortex:" + cortexTestUUID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestCortexProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, sourcePath, minimalCortexSession(cortexTestUUID))

	provider, ok := NewProvider(AgentCortex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("cortex:"+cortexTestUUID, result.Result.Session.ID)
	assert.Equal("project", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal("abc123", result.Result.Session.File.Hash)
	assert.Equal("Test session", result.Result.Session.SessionName)
	assert.Len(result.Result.Messages, 2)
}

// TestCortexProviderFingerprintIncludesContentHash guards that the Cortex
// provider computes a full-file content hash. The legacy per-agent parse stored
// a file_hash; without WithContentHashing the provider fingerprint hash is empty
// and a resync clears the stored file_hash to NULL. Toggle-provable: removing
// WithContentHashing from newCortexSourceSet makes fp.Hash empty and fails here.
func TestCortexProviderFingerprintIncludesContentHash(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, cortexTestUUID+".json")
	writeSourceFile(t, sourcePath, minimalCortexSession(cortexTestUUID))

	provider, ok := NewProvider(AgentCortex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fp, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	require.NotEmpty(fp.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fp,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, fp.Hash, outcome.Results[0].Result.Session.File.Hash)
}
