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

func TestJSONLSourceSetDiscoverRecursiveStableSources(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "b.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "a.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "c.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "ignored.txt"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "upper.JSONL"), "{}\n")

	roots := []string{root}
	sources := NewJSONLSourceSet(AgentCodex, roots,
		WithRecursive(),
		WithKey(func(root, path string) string {
			return mustRelSlash(t, root, path)
		}),
		WithProjectHint(func(root, path string) string {
			rel := mustRelSlash(t, root, filepath.Dir(path))
			if rel == "." {
				return ""
			}
			return rel
		}),
	)
	roots[0] = filepath.Join(root, "mutated")

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 3)

	assert.Equal(t, []string{
		"a.jsonl",
		"b.jsonl",
		"nested/c.jsonl",
	}, sourceKeys(discovered))
	assert.Equal(t, []string{"", "", "nested"}, sourceProjects(discovered))
	for _, source := range discovered {
		assert.Equal(t, AgentCodex, source.Provider)
		assert.Equal(t, source.DisplayPath, source.FingerprintKey)
		assert.NotEmpty(t, source.DisplayPath)
		assert.IsType(t, JSONLSource{}, source.Opaque)
	}
}

func TestJSONLSourceSetShallowDiscoveryAndFilters(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "keep.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "keep.ndjson"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "drop.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "skip.jsonl"), "{}\n")

	sources := NewJSONLSourceSet(AgentGptme, []string{root},
		WithExtensions(".jsonl", ".ndjson"),
		WithInclude(func(path string, _ os.FileInfo) bool {
			return filepath.Base(path) != "drop.jsonl"
		}),
	)

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(root, "keep.jsonl"),
		filepath.Join(root, "keep.ndjson"),
	}, sourceDisplayPaths(discovered))
}

func TestJSONLSourceSetWatchChangedPathFindAndFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "session-1.jsonl")
	content := "{\"role\":\"user\"}\n"
	writeSourceFile(t, path, content)
	writeSourceFile(t, filepath.Join(root, "nested", "notes.txt"), "{}\n")

	sources := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRecursive(),
		WithContentHashing(),
	)

	plan, err := sources.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)
	assert.NotEmpty(t, plan.Roots[0].DebounceKey)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].Key)
	assert.Equal(t, path, changed[0].DisplayPath)
	assert.Equal(t, path, changed[0].FingerprintKey)

	ignored, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "nested", "notes.txt"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

	outside, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(t.TempDir(), "session-1.jsonl"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, outside)

	found, ok, err := sources.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: path,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.DisplayPath)

	foundByID, ok, err := sources.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "session-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, found.DisplayPath, foundByID.DisplayPath)

	withoutOpaque := found
	withoutOpaque.Opaque = nil
	fingerprint, err := sources.Fingerprint(context.Background(), withoutOpaque)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, path, fingerprint.Key)
	assert.Equal(t, info.Size(), fingerprint.Size)
	assert.Equal(t, info.ModTime().UnixNano(), fingerprint.MTimeNS)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), fingerprint.Hash)
}

func TestJSONLSourceSetChangedPathClassifiesDeletedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "deleted.jsonl")
	sources := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRecursive(),
	)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: path, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].Key)
	assert.Equal(t, path, changed[0].DisplayPath)
	assert.Equal(t, path, changed[0].FingerprintKey)
	assert.Equal(t, "nested/deleted.jsonl", changed[0].Opaque.(JSONLSource).RelPath)

	shallowPath := filepath.Join(root, "nested", "ignored.jsonl")
	shallowSources := NewJSONLSourceSet(AgentCodex, []string{root})
	changed, err = shallowSources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: shallowPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestJSONLSourceSetChangedPathRejectsExistingNonRegularPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "not-a-source.jsonl")
	require.NoError(t, os.MkdirAll(path, 0o755))

	sources := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRecursive(),
	)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestJSONLSourceSetChangedPathUsesPathOnlyFilterForDeletedFiles(t *testing.T) {
	root := t.TempDir()
	sources := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRecursive(),
		WithIncludePath(func(root, path string) bool {
			return filepath.Base(path) == "events.jsonl"
		}),
	)

	ignored, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "session", "notes.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "session", "events.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, filepath.Join(root, "session", "events.jsonl"), changed[0].DisplayPath)
}

func TestJSONLSourceSetDescendPathPrunesSources(t *testing.T) {
	root := t.TempDir()
	keepPath := filepath.Join(root, "keep", "session.jsonl")
	skipPath := filepath.Join(root, "skip", "session.jsonl")
	writeSourceFile(t, keepPath, "{}\n")
	writeSourceFile(t, skipPath, "{}\n")

	sources := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRecursive(),
		WithDescendPath(func(root, path string) bool {
			return filepath.Base(path) != "skip"
		}),
	)

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, keepPath, discovered[0].DisplayPath)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: skipPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	removed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "skip", "removed.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestJSONLSourceSetDuplicateKeysKeepFirstConfiguredRoot(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	firstPath := filepath.Join(firstRoot, "session.jsonl")
	secondPath := filepath.Join(secondRoot, "session.jsonl")
	writeSourceFile(t, firstPath, "{}\n")
	writeSourceFile(t, secondPath, "{}\n")

	sources := NewJSONLSourceSet(AgentCodex, []string{firstRoot, secondRoot},
		WithKey(func(_, path string) string {
			return filepath.Base(path)
		}),
	)

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, firstPath, discovered[0].DisplayPath)

	found, ok, err := sources.FindSource(
		context.Background(),
		FindSourceRequest{StoredFilePath: secondPath},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstPath, found.DisplayPath)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      secondPath,
			EventKind: "write",
			WatchRoot: secondRoot,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestJSONLSourceSetFindSourceNormalizesRawSessionID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-1.jsonl")
	writeSourceFile(t, path, "{}\n")

	// LookupIDValid rejects the raw, un-normalized form, so a lookup only
	// succeeds when RawSessionIDForLookup runs before the validity gate and
	// before the SessionIDFromPath comparison in the discovery loop. The
	// on-disk session ID is "session-1" (base name without extension), which
	// the raw "raw:session-1" only matches once normalized.
	rejectsRaw := func(rawID string) bool {
		return rawID != "" && !strings.HasPrefix(rawID, "raw:")
	}

	normalizing := NewJSONLSourceSet(AgentCodex, []string{root},
		WithRawSessionIDForLookup(func(rawID string) string {
			return strings.TrimPrefix(rawID, "raw:")
		}),
		WithLookupIDValid(rejectsRaw),
	)

	found, ok, err := normalizing.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "raw:session-1"},
	)
	require.NoError(t, err)
	require.True(t, ok, "normalized raw session ID must resolve its source")
	assert.Equal(t, path, found.DisplayPath)

	// Without the normalizer the identical request is gated out: the raw form
	// fails LookupIDValid and never matches the on-disk session ID. This locks
	// in that the normalization step is what enables both checks.
	unnormalized := NewJSONLSourceSet(AgentCodex, []string{root},
		WithLookupIDValid(rejectsRaw),
	)

	_, ok, err = unnormalized.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "raw:session-1"},
	)
	require.NoError(t, err)
	assert.False(t, ok, "un-normalized raw session ID must not resolve")
}

func TestJSONLSourceSetMissingRootAndInvalidLookupAreNoops(t *testing.T) {
	root := t.TempDir()
	sources := NewJSONLSourceSet(AgentCodex, []string{
		filepath.Join(root, "missing"),
	})

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, discovered)

	found, ok, err := sources.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "../session",
	})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, found)
}

func writeSourceFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func mustRelSlash(t *testing.T, root, path string) string {
	t.Helper()

	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)
	return filepath.ToSlash(rel)
}

func sourceKeys(sources []SourceRef) []string {
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		keys = append(keys, source.Key)
	}
	return keys
}

func sourceProjects(sources []SourceRef) []string {
	projects := make([]string, 0, len(sources))
	for _, source := range sources {
		projects = append(projects, source.ProjectHint)
	}
	return projects
}

func sourceDisplayPaths(sources []SourceRef) []string {
	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		paths = append(paths, source.DisplayPath)
	}
	return paths
}

func TestCleanJSONLRootsPreservesS3Scheme(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		want  []string
	}{
		{
			name:  "s3 root kept verbatim",
			roots: []string{"s3://bucket/laptop/raw/claude"},
			want:  []string{"s3://bucket/laptop/raw/claude"},
		},
		{
			name:  "s3 root with trailing slash kept verbatim",
			roots: []string{"s3://bucket/laptop/raw/claude/"},
			want:  []string{"s3://bucket/laptop/raw/claude/"},
		},
		{
			name:  "local roots still cleaned",
			roots: []string{"/tmp/foo/../bar", "/tmp/baz/"},
			// filepath.Clean yields OS-native separators (backslashes on
			// Windows), so build the expectation with FromSlash rather than
			// hard-coding forward slashes.
			want: []string{filepath.FromSlash("/tmp/bar"), filepath.FromSlash("/tmp/baz")},
		},
		{
			name:  "empty roots dropped",
			roots: []string{"", "s3://bucket/x", ""},
			want:  []string{"s3://bucket/x"},
		},
		{
			name:  "mixed local and s3",
			roots: []string{"/tmp/a/./b", "s3://bucket/y/"},
			want:  []string{filepath.FromSlash("/tmp/a/b"), "s3://bucket/y/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// filepath.Clean would collapse "s3://" to "s3:/", which defeats the
			// HasPrefix("s3://") checks that route discovery to the object store.
			assert.Equal(t, tt.want, cleanJSONLRoots(tt.roots))
		})
	}
}
