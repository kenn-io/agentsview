package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/worktree"
)

func TestLoadConfiguredWorktreeMappings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[[worktree_mappings]]
path_prefix = "~/code/sample-app/.claude/worktrees/"
project = " sample-app "

[[worktree_mappings]]
path_prefix = "/worktrees"
layout = "repo_dot_worktrees"

[[worktree_mappings]]
path_prefix = 'C:\code\'
layout = "repo_dot_suffix"
`), 0o600))

	expected := []worktree.Rule{
		{PathPrefix: filepath.ToSlash(filepath.Join(home, "code", "sample-app", ".claude", "worktrees")), Layout: worktree.LayoutExplicit, Project: "sample_app"},
		{PathPrefix: "/worktrees", Layout: worktree.LayoutRepoDotWorktrees},
		{PathPrefix: "C:/code", Layout: worktree.LayoutRepoDotSuffix},
	}
	// Generating local runtime settings rewrites config.toml; preserve the
	// array of rule tables on that path as well as on a read-only load.
	writable, err := LoadMinimal()
	require.NoError(t, err)
	assert.Equal(t, expected, writable.WorktreeMappings)
	readOnly, err := LoadReadOnlyAt(dir)
	require.NoError(t, err)
	assert.Equal(t, expected, readOnly.WorktreeMappings)
}

func TestRejectInvalidConfiguredWorktreeMappings(t *testing.T) {
	for _, tc := range []struct {
		name, body, problem string
	}{
		{"layout", "path_prefix = '/code'\nlayout = 'unknown'", "layout must be"},
		{"project missing", "path_prefix = '/code'", "project is required"},
		{"prefix missing", "project = 'app'", "path_prefix is required"},
		{"dot worktrees project", "path_prefix = '/code'\nlayout = 'repo_dot_worktrees'\nproject = 'app'", "project must be empty"},
		{"dot suffix project", "path_prefix = '/code'\nlayout = 'repo_dot_suffix'\nproject = 'app'", "project must be empty"},
		{"duplicate normalized prefix", "path_prefix = '/code/'\nproject = 'app'\n[[worktree_mappings]]\npath_prefix = '/code/.'\nproject = 'other'", "duplicate path_prefix"},
		{"unknown field", "path_prefix = '/code'\nproject = 'app'\nenabled = false", "worktree_mappings.enabled"},
		{"not an array", "", "worktree_mappings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			data := "[[worktree_mappings]]\n" + tc.body + "\n"
			if tc.body == "" {
				data = "[worktree_mappings]\npath_prefix = '/code'\nproject = 'app'\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(data), 0o600))
			_, err := LoadReadOnlyAt(dir)
			assert.ErrorContains(t, err, tc.problem)
		})
	}
}
