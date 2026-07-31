package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListProjectRulesGovernedCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(t, err, "create /work mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work/repo", Layout: WorktreeMappingLayoutExplicit,
		Project: "inner", Enabled: true,
	})
	require.NoError(t, err, "create /work/repo mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work/disabled", Layout: WorktreeMappingLayoutExplicit,
		Project: "disabled-target", Enabled: false,
	})
	require.NoError(t, err, "create disabled mapping")

	// Two sessions under /work/repo: longest-prefix winner is /work/repo.
	insertSession(t, d, "repo-1", "misc", func(s *Session) {
		s.Machine = "ws"
		s.Cwd = "/work/repo/a"
	})
	insertSession(t, d, "repo-2", "misc", func(s *Session) {
		s.Machine = "ws"
		s.Cwd = "/work/repo/b"
	})
	// One session under /work but outside /work/repo: winner is /work.
	insertSession(t, d, "other-1", "misc", func(s *Session) {
		s.Machine = "ws"
		s.Cwd = "/work/other"
	})
	// A session on a machine with no mapping rules, only to seed the
	// typeahead machine list.
	insertSession(t, d, "solo-1", "misc", func(s *Session) {
		s.Machine = "solo-machine"
		s.Cwd = "/solo"
	})

	result, err := d.ListProjectRules(ctx, "ws")
	require.NoError(t, err)

	assert.Equal(t, "ws", result.Machine)
	assert.Contains(t, result.Machines, "ws")
	assert.Contains(t, result.Machines, "solo-machine",
		"session-only machine must appear in the typeahead list")

	require.Len(t, result.Rules, 3, "enabled and disabled rules both included")
	byPrefix := map[string]ProjectRule{}
	for _, r := range result.Rules {
		byPrefix[r.PathPrefix] = r
	}

	require.Contains(t, byPrefix, "/work/repo")
	assert.Equal(t, 2, byPrefix["/work/repo"].GovernedSessions,
		"nested rule wins both nested sessions by longest prefix")
	require.Contains(t, byPrefix, "/work")
	assert.Equal(t, 1, byPrefix["/work"].GovernedSessions,
		"outer rule only wins the session outside the nested prefix")
	require.Contains(t, byPrefix, "/work/disabled")
	assert.False(t, byPrefix["/work/disabled"].Enabled)
	assert.Equal(t, 0, byPrefix["/work/disabled"].GovernedSessions,
		"disabled rule never enters the evaluator")

	archiveID, err := d.GetArchiveID(ctx)
	require.NoError(t, err)
	for _, r := range result.Rules {
		assert.Equal(t, archiveID, r.SourceArchiveID)
	}
}

func TestListProjectRulesUnknownMachine(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(t, err, "create mapping on a different machine")
	insertSession(t, d, "ws-1", "misc", func(s *Session) {
		s.Machine = "ws"
		s.Cwd = "/work/a"
	})

	result, err := d.ListProjectRules(ctx, "nonexistent-machine")
	require.NoError(t, err)

	assert.Equal(t, "nonexistent-machine", result.Machine)
	assert.Empty(t, result.Rules)
	assert.Contains(t, result.Machines, "ws",
		"machine list is populated independent of the selected machine")
}
