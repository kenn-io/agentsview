package parser

import (
	"context"
	"crypto/sha256"
	"errors"
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

func TestJSONLSourceSetStreamingDiscoveryPropagatesTraversalErrors(t *testing.T) {
	t.Run("configured root is not a directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root.jsonl")
		writeSourceFile(t, root, "{}\n")
		set := NewJSONLSourceSet(AgentCodex, []string{root})

		err := set.DiscoverEach(t.Context(), func(SourceRef) error { return nil })

		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})

	t.Run("followed source stat", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.Symlink(
			filepath.Join(root, "missing-target"), filepath.Join(root, "a-broken.jsonl"),
		))
		writeSourceFile(t, filepath.Join(root, "z-healthy.jsonl"), "{}\n")
		set := NewJSONLSourceSet(
			AgentCodex, []string{root}, WithFollowSymlinkFiles(),
		)
		var yielded []string

		err := set.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{filepath.Join(root, "z-healthy.jsonl")}, yielded)
	})

	t.Run("followed directory symlink stat", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "linked-dir")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "a-linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.RemoveAll(target))
		writeSourceFile(t, filepath.Join(root, "z-healthy.jsonl"), "{}\n")
		// No exported option follows directory symlinks without also
		// following file symlinks, whose own stat failure would mask
		// the directory-descent path under test; build the
		// directory-only configuration directly.
		set := jsonlSourceSetFromOptions(AgentCodex, []string{root},
			JSONLSourceSetOptions{Recursive: true, FollowSymlinkDirs: true})
		var yielded []string

		err := set.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{filepath.Join(root, "z-healthy.jsonl")}, yielded)
	})

	t.Run("entry info race continues healthy sibling", func(t *testing.T) {
		root := t.TempDir()
		healthy := filepath.Join(root, "z-healthy.jsonl")
		writeSourceFile(t, healthy, "{}\n")
		ctx := withStreamingDirectoryReader(t.Context(), func(
			_ context.Context, _ string, yield func(os.DirEntry) error,
		) error {
			if err := yield(failingInfoDirEntry{
				name: "a-vanished.jsonl", err: os.ErrNotExist,
			}); err != nil {
				return err
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				return err
			}
			return yield(entries[0])
		})
		set := NewJSONLSourceSet(AgentCodex, []string{root})
		var yielded []string

		err := set.DiscoverEach(ctx, func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})

		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{healthy}, yielded)
	})

	t.Run("nested directory read continues healthy sibling", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "a-nested")
		require.NoError(t, os.Mkdir(nested, 0o755))
		healthy := filepath.Join(root, "z-healthy.jsonl")
		writeSourceFile(t, healthy, "{}\n")
		injected := errors.New("nested directory read failed")
		ctx := withStreamingDirectoryReader(t.Context(), func(
			ctx context.Context, dir string, yield func(os.DirEntry) error,
		) error {
			if samePath(dir, nested) {
				return injected
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := yield(entry); err != nil {
					return err
				}
			}
			return nil
		})
		set := NewJSONLSourceSet(AgentCodex, []string{root}, WithRecursive())
		var yielded []string

		err := set.DiscoverEach(ctx, func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})

		assert.ErrorIs(t, err, injected)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{healthy}, yielded)
	})

	t.Run("yield error aborts immediately", func(t *testing.T) {
		root := t.TempDir()
		writeSourceFile(t, filepath.Join(root, "a.jsonl"), "{}\n")
		writeSourceFile(t, filepath.Join(root, "b.jsonl"), "{}\n")
		set := NewJSONLSourceSet(AgentCodex, []string{root})
		injected := errors.New("stop consumer")
		calls := 0

		err := set.DiscoverEach(t.Context(), func(SourceRef) error {
			calls++
			return injected
		})

		assert.ErrorIs(t, err, injected)
		assert.Equal(t, 1, calls)
	})
}

type failingInfoDirEntry struct {
	name string
	err  error
}

func (entry failingInfoDirEntry) Name() string               { return entry.name }
func (failingInfoDirEntry) IsDir() bool                      { return false }
func (failingInfoDirEntry) Type() os.FileMode                { return 0 }
func (entry failingInfoDirEntry) Info() (os.FileInfo, error) { return nil, entry.err }

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

func TestJSONLSourceSetWatchRootsReturnsBoundedMetadata(t *testing.T) {
	root := t.TempDir()
	otherRoot := filepath.Join(t.TempDir(), "sessions")
	companionCalls := 0
	sources := NewJSONLSourceSet(
		AgentCodex,
		[]string{root, otherRoot},
		WithRecursive(),
		WithCompanionFiles(func(transcriptPath string) []string {
			companionCalls++
			return []string{transcriptPath + ".meta"}
		}),
	)

	roots, err := sources.WatchRoots(context.Background())
	require.NoError(t, err)
	require.Len(t, roots, 2)
	assert.Equal(t, WatchRoot{
		Path:        root,
		Recursive:   true,
		DebounceKey: string(AgentCodex) + ":jsonl:" + root,
	}, roots[0])
	assert.Equal(t, WatchRoot{
		Path:        otherRoot,
		Recursive:   true,
		DebounceKey: string(AgentCodex) + ":jsonl:" + otherRoot,
	}, roots[1])
	assert.Zero(t, companionCalls,
		"root scheduling must not enumerate transcripts or companions")
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
