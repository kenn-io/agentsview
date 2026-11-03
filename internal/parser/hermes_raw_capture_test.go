package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hermesRawFixtureProvider(t *testing.T, roots ...string) Provider {
	t.Helper()
	p, ok := NewProvider(AgentHermes, ProviderConfig{Roots: roots})
	require.True(t, ok)
	return p
}

func writeHermesRawTranscript(t *testing.T, home, name string) string {
	t.Helper()
	file := filepath.Join(home, "sessions", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, []byte(`{"role":"user","content":"Hello"}`+"\n"), 0o600))
	return file
}

func TestHermesRawCaptureCompleteHomeAndStableAnchor(t *testing.T) {
	root := t.TempDir()
	writeHermesRawTranscript(t, root, "one.jsonl")
	writeHermesRawTranscript(t, root, "session_one.json")
	require.NoError(t, os.WriteFile(filepath.Join(root, "credentials.json"), []byte("private"), 0o600))
	p := hermesRawFixtureProvider(t, root)
	discover := func() SourceRef {
		d, err := DiscoverRawCaptureSources(t.Context(), p)
		require.NoError(t, err)
		require.True(t, d.Complete)
		require.Len(t, d.Sources, 1)
		return d.Sources[0]
	}
	first := discover()
	canonical := canonicalRawCaptureTestPath(t, root)
	assert.Equal(t, filepath.Join(canonical, "state.db"), first.Key)
	plan, ok, err := ResolveRawCapturePlan(t.Context(), p, first)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, canonical, plan.ConfiguredRoot)
	assert.Equal(t, canonical, plan.CaptureRoot)
	assert.Empty(t, plan.SnapshotPath)
	var paths []string
	for _, e := range plan.Entries {
		paths = append(paths, e.Path)
		assert.False(t, e.Appendable)
	}
	assert.Equal(t, []string{"sessions/one.jsonl", "sessions/session_one.json"}, paths)
	createHermesStateDB(t, root)
	after := discover()
	assert.Equal(t, first.Key, after.Key)
	plan, ok, err = ResolveRawCapturePlan(t.Context(), p, after)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "state.db", plan.SnapshotPath)
	require.Len(t, plan.Entries, 3)
	require.NoError(t, os.Remove(filepath.Join(root, "state.db")))
	require.NoError(t, os.Remove(filepath.Join(root, "sessions", "one.jsonl")))
	assert.Equal(t, first.Key, discover().Key)
}

func TestHermesRawCaptureRootAliasesAndFirstOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roots []string
		owner string
	}{
		{"home", []string{"."}, "."},
		{"sessions", []string{"sessions"}, "sessions"},
		{"database", []string{"state.db"}, "."},
		{"overlap home first", []string{".", "sessions"}, "."},
		{"overlap sessions first", []string{"sessions", "."}, "sessions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeHermesRawTranscript(t, root, "one.jsonl")
			createHermesStateDB(t, root)
			var roots []string
			for _, r := range tc.roots {
				roots = append(roots, filepath.Join(root, r))
			}
			p := hermesRawFixtureProvider(t, roots...)
			d, err := DiscoverRawCaptureSources(t.Context(), p)
			require.NoError(t, err)
			require.True(t, d.Complete)
			require.Len(t, d.Sources, 1)
			plan, ok, err := ResolveRawCapturePlan(t.Context(), p, d.Sources[0])
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, canonicalRawCaptureTestPath(t, filepath.Join(root, tc.owner)), plan.ConfiguredRoot)
			assert.Equal(t, canonicalRawCaptureTestPath(t, root), plan.CaptureRoot)
			configured := p.(interface{ ConfiguredRoots() []string }).ConfiguredRoots()
			assert.Equal(t, filepath.Clean(filepath.Join(root, tc.owner)), configured[0])
		})
	}
}

func TestHermesRawCaptureProfilesAndEvents(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".hermes", "profiles")
	first := filepath.Join(container, "first")
	second := filepath.Join(container, "second")
	firstFile := writeHermesRawTranscript(t, first, "same.jsonl")
	writeHermesRawTranscript(t, second, "same.jsonl")
	p := hermesRawFixtureProvider(t, container)
	d, err := DiscoverRawCaptureSources(t.Context(), p)
	require.NoError(t, err)
	require.True(t, d.Complete)
	require.Len(t, d.Sources, 2)
	assert.NotEqual(t, d.Sources[0].Key, d.Sources[1].Key)
	for _, s := range d.Sources {
		plan, ok, err := ResolveRawCapturePlan(t.Context(), p, s)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, canonicalRawCaptureTestPath(t, container), plan.ConfiguredRoot)
	}
	for _, event := range []string{firstFile, filepath.Join(first, "state.db-wal"), filepath.Join(first, "state.db-shm"), filepath.Join(first, "state.db-journal"), first} {
		sources, err := RawCaptureSourcesForChangedPath(t.Context(), p, ChangedPathRequest{Path: event, WatchRoot: container})
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assert.Equal(t, filepath.Join(canonicalRawCaptureTestPath(t, first), "state.db"), sources[0].Key)
	}
	unrelated, err := RawCaptureSourcesForChangedPath(t.Context(), p, ChangedPathRequest{Path: filepath.Join(first, "config.yaml"), WatchRoot: container})
	require.NoError(t, err)
	assert.Empty(t, unrelated)
	writeHermesRawTranscript(t, filepath.Join(container, "new-profile"), "same.jsonl")
	d, err = DiscoverRawCaptureSources(t.Context(), p)
	require.NoError(t, err)
	require.Len(t, d.Sources, 3)
}

func TestHermesRawCaptureFlatTranscriptRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "flat.jsonl")
	require.NoError(t, os.WriteFile(file, []byte("{}\n"), 0o600))
	p := hermesRawFixtureProvider(t, root)
	d, err := DiscoverRawCaptureSources(t.Context(), p)
	require.NoError(t, err)
	require.Len(t, d.Sources, 1)
	plan, ok, err := ResolveRawCapturePlan(t.Context(), p, d.Sources[0])
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "sessions/flat.jsonl", plan.Entries[0].Path)
	assert.Equal(t, canonicalRawCaptureTestPath(t, file), plan.Entries[0].LocalPath)
}

func TestHermesRawCaptureIncompleteAndInterrupted(t *testing.T) {
	t.Run("missing configured root", func(t *testing.T) {
		missingRoot := filepath.Join(t.TempDir(), "missing")
		healthyRoot := t.TempDir()
		writeHermesRawTranscript(t, healthyRoot, "healthy.jsonl")
		d, err := DiscoverRawCaptureSources(
			t.Context(), hermesRawFixtureProvider(t, missingRoot, healthyRoot),
		)
		assert.False(t, d.Complete)
		var incomplete DiscoveryIncompleteError
		require.ErrorAs(t, err, &incomplete)
		require.Len(t, d.Sources, 1,
			"discovery must continue through a healthy configured root")
		assert.Equal(t, filepath.Join(canonicalRawCaptureTestPath(t, healthyRoot), "state.db"), d.Sources[0].Key)
	})
	t.Run("empty existing root", func(t *testing.T) {
		d, err := DiscoverRawCaptureSources(t.Context(), hermesRawFixtureProvider(t, t.TempDir()))
		require.NoError(t, err)
		assert.True(t, d.Complete)
		assert.Empty(t, d.Sources)
	})
	t.Run("yield", func(t *testing.T) {
		root := t.TempDir()
		writeHermesRawTranscript(t, root, "one.jsonl")
		sentinel := errors.New("stop yield")
		complete, err := StreamRawCaptureSources(t.Context(), hermesRawFixtureProvider(t, root), func(SourceRef) error { return sentinel })
		require.ErrorIs(t, err, sentinel)
		assert.False(t, complete)
	})
	t.Run("progress", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"one.jsonl", "two.jsonl", "three.jsonl"} {
			writeHermesRawTranscript(t, root, name)
		}
		steps := 0
		sentinel := errors.New("pause scan")
		ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error {
			steps++
			if steps == 3 {
				return sentinel
			}
			return nil
		})
		complete, err := StreamRawCaptureSources(ctx, hermesRawFixtureProvider(t, root), func(SourceRef) error { return nil })
		require.ErrorIs(t, err, sentinel)
		assert.False(t, complete)
		assert.Equal(t, 3, steps)
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		d, err := DiscoverRawCaptureSources(ctx, hermesRawFixtureProvider(t, t.TempDir()))
		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, d.Complete)
	})
	t.Run("unsafe companion", func(t *testing.T) {
		root := t.TempDir()
		file := writeHermesRawTranscript(t, root, "one.jsonl")
		external := filepath.Join(t.TempDir(), "external.jsonl")
		require.NoError(t, os.WriteFile(external, []byte("{}"), 0o600))
		require.NoError(t, os.Remove(file))
		if err := os.Symlink(external, file); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		d, err := DiscoverRawCaptureSources(t.Context(), hermesRawFixtureProvider(t, root))
		assert.False(t, d.Complete)
		require.Error(t, err)
	})
}

func TestHermesRawSnapshotFanoutMatchesLocalDiscovery(t *testing.T) {
	root := t.TempDir()
	createHermesStateDB(t, root)
	writeHermesRawTranscript(t, root, "orphan.jsonl")
	p := hermesRawFixtureProvider(t, root)
	local, err := p.Discover(t.Context())
	require.NoError(t, err)
	d, err := DiscoverRawCaptureSources(t.Context(), p)
	require.NoError(t, err)
	require.Len(t, d.Sources, 1)
	hosted, ok, err := ResolveRawSnapshotSessions(t.Context(), p, d.Sources[0])
	require.NoError(t, err)
	require.True(t, ok)
	keys := func(sources []SourceRef) []string {
		var out []string
		for _, source := range sources {
			out = append(out, hermesRawEventPath(source.Key))
		}
		slices.Sort(out)
		return out
	}
	assert.Equal(t, keys(local), keys(hosted))
}

func TestHermesRawChangedPathIgnoresUnrelatedInvalidProfile(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".hermes", "profiles")
	file := writeHermesRawTranscript(t, filepath.Join(container, "alpha"), "one.jsonl")
	other := filepath.Join(container, "beta")
	require.NoError(t, os.Mkdir(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, "sessions"), []byte("invalid directory"), 0o600))
	p := hermesRawFixtureProvider(t, container)
	sources, err := RawCaptureSourcesForChangedPath(t.Context(), p, ChangedPathRequest{Path: file, WatchRoot: container})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, hermesRawEventPath(filepath.Join(container, "alpha", "state.db")), sources[0].Key)
}

func TestHermesRawProfilesAliasKeepsContainerSemantics(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(filepath.Dir(container), 0o700))
	target := t.TempDir()
	writeHermesRawTranscript(t, filepath.Join(target, "alpha"), "one.jsonl")
	writeHermesRawTranscript(t, filepath.Join(target, "beta"), "two.jsonl")
	if err := os.Symlink(target, container); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	p := hermesRawFixtureProvider(t, container)
	d, err := DiscoverRawCaptureSources(t.Context(), p)
	require.NoError(t, err)
	require.True(t, d.Complete)
	require.Len(t, d.Sources, 2)
	for _, source := range d.Sources {
		assert.Equal(t, hermesRawEventPath(target), source.ConfiguredRoot)
	}
}

func TestHermesRawChangedPathWorkDoesNotGrowWithOtherProfiles(t *testing.T) {
	var steps []int
	for _, count := range []int{1, 256} {
		container := filepath.Join(t.TempDir(), ".hermes", "profiles")
		file := writeHermesRawTranscript(t, filepath.Join(container, "target"), "one.jsonl")
		for i := range count {
			writeHermesRawTranscript(t, filepath.Join(container, "unrelated"), fmt.Sprintf("session-%d.jsonl", i))
		}
		calls := 0
		ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error { calls++; return nil })
		sources, err := RawCaptureSourcesForChangedPath(ctx, hermesRawFixtureProvider(t, container), ChangedPathRequest{Path: file, WatchRoot: container})
		require.NoError(t, err)
		require.Len(t, sources, 1)
		steps = append(steps, calls)
	}
	assert.Equal(t, []int{1, 1}, steps)
}
