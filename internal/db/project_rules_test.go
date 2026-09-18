package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListProjectRulesGovernedCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(err, "create /work mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work/repo", Layout: WorktreeMappingLayoutExplicit,
		Project: "inner", Enabled: true,
	})
	require.NoError(err, "create /work/repo mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work/disabled", Layout: WorktreeMappingLayoutExplicit,
		Project: "disabled-target", Enabled: false,
	})
	require.NoError(err, "create disabled mapping")

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
	require.NoError(err)

	assert.Equal("ws", result.Machine)
	assert.Contains(result.Machines, "ws")
	assert.Contains(result.Machines, "solo-machine",
		"session-only machine must appear in the typeahead list")

	require.Len(result.Rules, 3, "enabled and disabled rules both included")
	byPrefix := map[string]ProjectRule{}
	for _, r := range result.Rules {
		byPrefix[r.PathPrefix] = r
	}

	require.Contains(byPrefix, "/work/repo")
	assert.Equal(2, byPrefix["/work/repo"].GovernedSessions,
		"nested rule wins both nested sessions by longest prefix")
	require.Contains(byPrefix, "/work")
	assert.Equal(1, byPrefix["/work"].GovernedSessions,
		"outer rule only wins the session outside the nested prefix")
	require.Contains(byPrefix, "/work/disabled")
	assert.False(byPrefix["/work/disabled"].Enabled)
	assert.Equal(0, byPrefix["/work/disabled"].GovernedSessions,
		"disabled rule never enters the evaluator")

	archiveID, err := d.GetArchiveID(ctx)
	require.NoError(err)
	for _, r := range result.Rules {
		assert.Equal(archiveID, r.SourceArchiveID)
	}
}

func TestListProjectRulesUnknownMachine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(err, "create mapping on a different machine")
	insertSession(t, d, "ws-1", "misc", func(s *Session) {
		s.Machine = "ws"
		s.Cwd = "/work/a"
	})

	result, err := d.ListProjectRules(ctx, "nonexistent-machine")
	require.NoError(err)

	assert.Equal("nonexistent-machine", result.Machine)
	assert.Empty(result.Rules)
	assert.Contains(result.Machines, "ws",
		"machine list is populated independent of the selected machine")
}
