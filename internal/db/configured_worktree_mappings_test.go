package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/worktree"
)

func TestConfiguredAndArchiveWorktreeMappingPrecedence(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := filepath.ToSlash(t.TempDir())
	d.SetConfiguredWorktreeMappings(defaultMachine, []worktree.Rule{
		{PathPrefix: root, Layout: worktree.LayoutRepoDotSuffix},
		{PathPrefix: root + "/same.dev", Layout: worktree.LayoutExplicit, Project: "configured_same"},
		{PathPrefix: root + "/disabled.dev", Layout: worktree.LayoutExplicit, Project: "configured_disabled"},
		{PathPrefix: root + "/short.dev/nested", Layout: worktree.LayoutExplicit, Project: "configured_longer"},
	})
	for _, rule := range []WorktreeProjectMapping{
		{PathPrefix: root + "/same.dev", Project: "archive_same", Enabled: true},
		{PathPrefix: root + "/disabled.dev", Project: "archive_disabled", Enabled: false},
		{PathPrefix: root + "/short.dev", Project: "archive_shorter", Enabled: true},
		{PathPrefix: root + "/long.dev/nested", Project: "archive_longer", Enabled: true},
	} {
		rule.Machine = defaultMachine
		_, err := d.CreateWorktreeProjectMapping(ctx, rule)
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		id, suffix, machine, want string
	}{
		{"config only", "/unlisted.dev/feature", defaultMachine, "unlisted"},
		{"exact collision", "/same.dev/feature", defaultMachine, "archive_same"},
		{"disabled collision", "/disabled.dev/feature", defaultMachine, "configured_disabled"},
		{"longer config", "/short.dev/nested/feature", defaultMachine, "configured_longer"},
		{"longer archive", "/long.dev/nested/feature", defaultMachine, "archive_longer"},
		{"different machine", "/unlisted.dev/feature", "remote", "initial"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			insertSession(t, d, tc.id, "initial", func(s *Session) {
				s.Machine, s.Cwd = tc.machine, root+tc.suffix
			})
			_, err := d.ApplyWorktreeProjectMappings(ctx, tc.machine)
			require.NoError(t, err)
			row, err := d.GetSession(ctx, tc.id)
			require.NoError(t, err)
			require.NotNil(t, row)
			assert.Equal(t, tc.want, row.Project)
		})
	}

	// The Rules page reports only archive rules and never attributes a
	// configured rule's sessions to a shadowed or disabled archive rule.
	rules, err := d.ListProjectRules(ctx, defaultMachine)
	require.NoError(t, err)
	gotGovernedCounts := map[string]int{}
	for _, rule := range rules.Rules {
		gotGovernedCounts[rule.Project] = rule.GovernedSessions
	}
	assert.Equal(t, map[string]int{
		"archive_same": 1, "archive_disabled": 0, "archive_shorter": 0, "archive_longer": 1,
	}, gotGovernedCounts)

	inventory, err := d.GetProjectInventory(ctx, ProjectDateFilter{})
	require.NoError(t, err)
	assert.Equal(t, 5, inventory.GovernedSessions)
	targetingCounts := map[string]int{}
	for _, project := range inventory.Projects {
		targetingCounts[project.Label] = project.EnabledRulesTargeting
	}
	assert.Equal(t, 1, targetingCounts["unlisted"], "the winning suffix rule targets its derived project")
	assert.Zero(t, targetingCounts["initial"])
}
