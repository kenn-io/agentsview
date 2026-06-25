package parser

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryJSONLSourceSetDiscoversProjectFiles(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "project-b", "session-b.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project-a", "session-a.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project-a", "nested", "skip.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "root.jsonl"), "{}\n")

	sources := NewDirectoryJSONLSourceSet(
		AgentQwen, []string{root}, JSONLSourceSetOptions{},
	)

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, []string{"project-a", "project-b"}, sourceProjects(discovered))
	assert.Equal(t, []string{
		filepath.Join(root, "project-a", "session-a.jsonl"),
		filepath.Join(root, "project-b", "session-b.jsonl"),
	}, sourceDisplayPaths(discovered))

	found, ok, err := sources.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "session-b",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(root, "project-b", "session-b.jsonl"), found.DisplayPath)
}

func TestDirectoryJSONLSourceSetComposesPathFilters(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "project", "session-keep.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "project", "ignore.jsonl"), "{}\n")

	sources := NewDirectoryJSONLSourceSet(AgentIflow, []string{root}, JSONLSourceSetOptions{
		IncludePath: func(root, path string) bool {
			return strings.HasPrefix(filepath.Base(path), "session-")
		},
		ProjectHint: func(root, path string) string {
			return "custom-" + filepath.Base(filepath.Dir(path))
		},
	})

	discovered, err := sources.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, "custom-project", discovered[0].ProjectHint)
	assert.Equal(t, filepath.Join(root, "project", "session-keep.jsonl"), discovered[0].DisplayPath)
}

func TestDirectoryJSONLSourceSetClassifiesDeletedProjectFiles(t *testing.T) {
	root := t.TempDir()
	sources := NewDirectoryJSONLSourceSet(
		AgentCommandCode, []string{root}, JSONLSourceSetOptions{},
	)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "project", "deleted.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, "project", changed[0].ProjectHint)
	assert.Equal(t, "project/deleted.jsonl", changed[0].Opaque.(JSONLSource).RelPath)

	deep, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "project", "nested", "ignored.jsonl"),
			EventKind: "remove",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, deep)
}
