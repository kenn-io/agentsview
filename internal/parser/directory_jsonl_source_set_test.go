package parser

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryJSONLSourceSetDiscoversProjectFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "project-b", "session-b.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project-a", "session-a.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project-a", "nested", "skip.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "root.jsonl"), "{}\n")

	sources := NewDirectoryJSONLSourceSet(AgentQwen, []string{root})

	discovered, err := sources.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.Equal([]string{"project-a", "project-b"}, sourceProjects(discovered))
	assert.Equal([]string{
		filepath.Join(root, "project-a", "session-a.jsonl"),
		filepath.Join(root, "project-b", "session-b.jsonl"),
	}, sourceDisplayPaths(discovered))

	found, ok, err := sources.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "session-b",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(filepath.Join(root, "project-b", "session-b.jsonl"), found.DisplayPath)
}

func TestDirectoryJSONLSourceSetComposesPathFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "project", "session-keep.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project", "ignore.jsonl"), "{}\n")

	sources := NewDirectoryJSONLSourceSet(AgentIflow, []string{root},
		WithIncludePath(func(root, path string) bool {
			return strings.HasPrefix(filepath.Base(path), "session-")
		}),
		WithProjectHint(func(root, path string) string {
			return "custom-" + filepath.Base(filepath.Dir(path))
		}),
	)

	discovered, err := sources.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal("custom-project", discovered[0].ProjectHint)
	assert.Equal(filepath.Join(root, "project", "session-keep.jsonl"), discovered[0].DisplayPath)
}

func TestDirectoryJSONLSourceSetClassifiesDeletedProjectFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sources := NewDirectoryJSONLSourceSet(AgentCommandCode, []string{root})

	changed, err := sources.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "project", "deleted.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal("project", changed[0].ProjectHint)
	assert.Equal("project/deleted.jsonl", changed[0].Opaque.(JSONLSource).RelPath)

	deep, err := sources.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "project", "nested", "ignored.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	assert.Empty(deep)
}
