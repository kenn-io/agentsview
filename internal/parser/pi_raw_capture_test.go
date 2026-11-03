package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writePiRawFixture(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func TestPiRawCaptureFamily(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentOMO, AgentStepCode, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			dir := root
			if agent != AgentPrimeAgent {
				dir = filepath.Join(root, "project")
			}
			parent := filepath.Join(dir, "parent-file.jsonl")
			child := filepath.Join(dir, "child.jsonl")
			writePiRawFixture(t, parent, `{"type":"session","id":"parent-header"}`+"\n")
			writePiRawFixture(t, child, `{"type":"session","id":"child-header","parentSession":"parent-file.jsonl"}`+"\n"+`{"type":"message","id":"u1","message":{"role":"user","content":"capture this"}}`+"\n")
			provider, ok := NewProvider(agent, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			discovery, err := DiscoverRawCaptureSources(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, discovery.Complete)
			require.Len(t, discovery.Sources, 2)
			var source SourceRef
			for _, s := range discovery.Sources {
				if s.DisplayPath == child {
					source = s
				}
			}
			require.NotEmpty(t, source.Key)
			plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, source)
			require.NoError(t, err)
			require.True(t, supported)
			require.Len(t, plan.Entries, 2)
			canonicalRoot, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			assert.Equal(t, canonicalRoot, plan.CaptureRoot)
			assert.Equal(t, source.Key, plan.SourceKey)
			paths := []string{}
			for _, entry := range plan.Entries {
				paths = append(paths, entry.Path)
				assert.True(t, entry.Appendable)
			}
			prefix := ""
			if agent != AgentPrimeAgent {
				prefix = "project/"
			}
			assert.ElementsMatch(t, []string{prefix + "parent-file.jsonl", prefix + "child.jsonl"}, paths)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, provider.Definition().IDPrefix+"parent-header", outcome.Results[0].Result.Session.ParentSessionID)
			require.Len(t, outcome.Results[0].Result.Messages, 1)
			assert.Equal(t, "capture this", outcome.Results[0].Result.Messages[0].Content)
		})
	}
}

func TestPiRawCapturePhysicalDiscovery(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "invalid.jsonl")
	writePiRawFixture(t, bad, "malformed physical bytes\n")
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root, root}})
	discovery, err := DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	_, supported, err := ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
	require.NoError(t, err)
	assert.True(t, supported)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	discovery, err = DiscoverRawCaptureSources(ctx, provider)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, discovery.Complete)
	streaming, ok := provider.(StreamingRawCaptureSourceProvider)
	require.True(t, ok)
	sentinel := errors.New("stop yielding")
	complete, err := streaming.DiscoverRawCaptureSourcesEach(t.Context(), func(SourceRef) error { return sentinel })
	require.ErrorIs(t, err, sentinel)
	assert.False(t, complete)
	provider, _ = NewProvider(AgentPi, ProviderConfig{Roots: []string{filepath.Join(root, "absent")}})
	discovery, err = DiscoverRawCaptureSources(t.Context(), provider)
	require.Error(t, err)
	assert.False(t, discovery.Complete)
}

func TestPiRawCaptureContinuesAfterUnavailableRoot(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	healthy := filepath.Join(root, "healthy")
	transcript := filepath.Join(healthy, "session.jsonl")
	writePiRawFixture(t, transcript, `{"type":"session","id":"healthy"}`+"\n")
	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{missing, healthy}})
	require.True(t, ok)

	discovery, err := DiscoverRawCaptureSources(t.Context(), provider)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	assert.False(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	assert.Equal(t, transcript, discovery.Sources[0].DisplayPath)
}

func TestPiRawCaptureUsesNestedRootWhenEarlierRootCannotBeListed(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	transcript := filepath.Join(nested, "session.jsonl")
	writePiRawFixture(t, transcript, `{"type":"session","id":"nested"}`+"\n")
	provider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{parent, nested}})
	require.True(t, ok)

	readErr := errors.New("injected directory read failure")
	ctx := withStreamingDirectoryReader(t.Context(), func(ctx context.Context, dir string, yield func(os.DirEntry) error) error {
		if filepath.Clean(dir) == filepath.Clean(parent) {
			return readErr
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})
	discovery, err := DiscoverRawCaptureSources(ctx, provider)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorIs(t, err, readErr)
	assert.False(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	assert.Equal(t, transcript, discovery.Sources[0].Key)
	source, ok := discovery.Sources[0].Opaque.(JSONLSource)
	require.True(t, ok)
	assert.Equal(t, nested, source.Root)
}

func TestPiRawCaptureParentBoundary(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "symlink"}[symlink], func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "parent.jsonl")
			writePiRawFixture(t, outside, `{"type":"session","id":"outside"}`+"\n")
			ref := outside
			if symlink {
				ref = filepath.Join(root, "parent.jsonl")
				require.NoError(t, os.Symlink(outside, ref))
			}
			child := filepath.Join(root, "child.jsonl")
			body := `{"type":"session","id":"child","parentSession":` + strconv.Quote(ref) + "}\n"
			writePiRawFixture(t, child, body)
			provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root, filepath.Dir(outside)}})
			sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			_, _, err = ResolveRawCapturePlan(t.Context(), provider, sources[0])
			assert.Error(t, err)
		})
	}
}

func TestPiRawCaptureStructuralParents(t *testing.T) {
	for _, tc := range []struct {
		agent AgentType
		child string
	}{
		{AgentPi, "project/parent/task/run-0/session.jsonl"},
		{AgentOMO, "project/parent/task/run-2/session.jsonl"},
		{AgentOMP, "project/parent/agent.jsonl"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			writePiRawFixture(t, filepath.Join(root, "project/parent.jsonl"), `{"type":"title","title":"rewritable"}`+"\n"+`{"type":"session","id":"structural-header"}`+"\n")
			child := filepath.Join(root, filepath.FromSlash(tc.child))
			writePiRawFixture(t, child, `{"type":"session","id":"child"}`+"\n")
			provider, _ := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}})
			sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
			require.NoError(t, err)
			require.True(t, supported)
			assert.Len(t, plan.Entries, 2)
			parsed, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			assert.Equal(t, provider.Definition().IDPrefix+"structural-header", parsed.Results[0].Result.Session.ParentSessionID)
			assert.Equal(t, RelSubagent, parsed.Results[0].Result.Session.RelationshipType)
		})
	}
}

func TestPiRawCaptureRetainsMalformedFallbackRead(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "other/parent.jsonl")
	writePiRawFixture(t, parent, `{"type":"session","id":"authoritative"}`+"\n")
	sibling := filepath.Join(root, "project/parent.jsonl")
	writePiRawFixture(t, sibling, "invalid parent header\n")
	child := filepath.Join(root, "project/child.jsonl")
	writePiRawFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n")
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	paths := []string{}
	for _, e := range plan.Entries {
		paths = append(paths, e.Path)
	}
	assert.ElementsMatch(t, []string{"project/child.jsonl", "other/parent.jsonl", "project/parent.jsonl"}, paths)
}

func TestPiRawCaptureConfinesTraversal(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "escape", true: "cycle"}[cycle], func(t *testing.T) {
			root := t.TempDir()
			target := t.TempDir()
			writePiRawFixture(t, filepath.Join(target, "outside.jsonl"), `{"type":"session","id":"outside"}`+"\n")
			if cycle {
				target = root
			}
			require.NoError(t, os.Symlink(target, filepath.Join(root, "linked")))
			provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
			visited := 0
			sentinel := errors.New("test traversal guard")
			ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error {
				visited++
				if visited > 50 {
					return sentinel
				}
				return nil
			})
			discovery, err := DiscoverRawCaptureSources(ctx, provider)
			require.Error(t, err)
			require.NotErrorIs(t, err, sentinel)
			assert.False(t, discovery.Complete)
			assert.Empty(t, discovery.Sources)
		})
	}
}

// A recognizable type field is not a valid header if its JSON is incomplete.
// Capture keeps those bytes and must not follow the apparent parent hint.
func TestPiRawCaptureMalformedHeaderDoesNotReadParent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "parent.jsonl")
	writePiRawFixture(t, outside, `{"type":"session","id":"outside"}`+"\n")
	child := filepath.Join(root, "child.jsonl")
	writePiRawFixture(t, child, `{"type":"session","parentSession":`+strconv.Quote(outside)+"\n")
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	discovery, err := DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovery.Sources, 1)
	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "child.jsonl", plan.Entries[0].Path)
	_, err = provider.Parse(t.Context(), ParseRequest{Source: discovery.Sources[0]})
	assert.ErrorContains(t, err, "invalid JSON header")
}

func TestPiRawCaptureOversizedHeaderCustody(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "oversized.jsonl")
	f, err := os.Create(file)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(64*1024*1024+1))
	require.NoError(t, f.Close())
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	sources, err := RawCaptureSourcesForChangedPath(t.Context(), provider, ChangedPathRequest{Path: file})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "oversized.jsonl", plan.Entries[0].Path)
}

func TestPiRawCaptureCancelledEmptyRoots(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	provider, _ := NewProvider(AgentPi, ProviderConfig{})
	discovery, err := DiscoverRawCaptureSources(ctx, provider)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, discovery.Complete)
}

func TestPiRawCaptureRejectsAmbiguousPrimarySuffix(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "project/child.jsonl")
	suffix := strings.TrimLeft(strings.TrimPrefix(child, filepath.VolumeName(child)), string(filepath.Separator))
	parent := filepath.Join(root, suffix)
	writePiRawFixture(t, parent, `{"type":"session","id":"different-parent"}`+"\n")
	writePiRawFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n")
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	sources, err := RawCaptureSourcesForChangedPath(t.Context(), provider, ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	_, _, err = ResolveRawCapturePlan(t.Context(), provider, sources[0])
	assert.ErrorContains(t, err, "ambiguous primary")
}

func TestPiRawCaptureAllowsAbsoluteInRootSymlink(t *testing.T) {
	for _, primaryLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent", true: "primary"}[primaryLink], func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "data/target.jsonl")
			child := filepath.Join(root, "child.jsonl")
			if primaryLink {
				writePiRawFixture(t, target, `{"type":"session","id":"child"}`+"\n")
				require.NoError(t, os.Symlink(target, child))
			} else {
				writePiRawFixture(t, target, `{"type":"session","id":"parent-header"}`+"\n")
				require.NoError(t, os.Symlink(target, filepath.Join(root, "parent.jsonl")))
				writePiRawFixture(t, child, `{"type":"session","id":"child","parentSession":"parent.jsonl"}`+"\n")
			}
			provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
			sources, err := RawCaptureSourcesForChangedPath(t.Context(), provider, ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
			require.NoError(t, err)
			require.True(t, supported)
			expected := []string{"child.jsonl"}
			if !primaryLink {
				expected = append(expected, "parent.jsonl")
			}
			var paths []string
			for _, entry := range plan.Entries {
				paths = append(paths, entry.Path)
			}
			assert.ElementsMatch(t, expected, paths)
		})
	}
}

func TestPiRawCaptureDeduplicatesEquivalentParentReads(t *testing.T) {
	for _, agent := range []AgentType{AgentPi, AgentOMO} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "project/parent.jsonl")
			child := filepath.Join(root, "project/parent/task/run-0/session.jsonl")
			writePiRawFixture(t, parent, `{"type":"session","id":"parent-header"}`+"\n")
			reference := root + string(filepath.Separator) + "project" + string(filepath.Separator) + "." + string(filepath.Separator) + "parent.jsonl"
			writePiRawFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(reference)+"}\n")
			provider, _ := NewProvider(agent, ProviderConfig{Roots: []string{root}})
			sources, err := RawCaptureSourcesForChangedPath(t.Context(), provider, ChangedPathRequest{Path: child})
			require.NoError(t, err)
			require.Len(t, sources, 1)
			plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
			require.NoError(t, err)
			require.True(t, supported)
			var paths []string
			for _, entry := range plan.Entries {
				paths = append(paths, entry.Path)
			}
			assert.ElementsMatch(t, []string{"project/parent.jsonl", "project/parent/task/run-0/session.jsonl"}, paths)
		})
	}
}

func TestPiRawCaptureKeepsCaseDistinctHardLinks(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "project", "Parent.jsonl")
	structuralParent := filepath.Join(root, "project", "parent.jsonl")
	writePiRawFixture(t, parent, `{"type":"session","id":"parent-header"}`+"\n")
	if err := os.Link(parent, structuralParent); err != nil {
		t.Skip("requires case-sensitive paths and hard-link support")
	}
	child := filepath.Join(root, "project", "parent", "task", "run-0", "session.jsonl")
	writePiRawFixture(t, child, `{"type":"session","id":"child","parentSession":`+strconv.Quote(parent)+"}\n")
	provider, _ := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	sources, err := RawCaptureSourcesForChangedPath(t.Context(), provider, ChangedPathRequest{Path: child})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	var paths []string
	for _, entry := range plan.Entries {
		paths = append(paths, entry.Path)
	}
	assert.ElementsMatch(t, []string{
		"project/Parent.jsonl",
		"project/parent.jsonl",
		"project/parent/task/run-0/session.jsonl",
	}, paths)
}
