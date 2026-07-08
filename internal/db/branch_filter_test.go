package db

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func branchInfoForTest(project, branch string) BranchInfo {
	return BranchInfo{
		Project: project,
		Branch:  branch,
		Token:   EncodeBranchFilterToken(project, branch),
	}
}

// seedBranchUsageFixture seeds "" and "unknown" as distinct branch buckets.
// Shared by TestGetDailyUsageBranchBreakdowns and TestGetDailyUsageGitBranchFilter
// so it only needs updating in one place.
func seedBranchUsageFixture(t *testing.T, d *DB) {
	t.Helper()
	seed := []struct {
		id, project, branch string
		input, output       int
	}{
		{"a", "proj-a", "main", 100, 10},
		{"b", "proj-a", "feature-x", 200, 20},
		{"c", "proj-b", "main", 300, 30},
		{"d", "proj-a", "", 400, 40},
		{"e", "proj-a", "unknown", 500, 50},
	}
	for _, s := range seed {
		input, output := s.input, s.output
		insertSession(t, d, s.id, s.project, func(sess *Session) {
			sess.GitBranch = s.branch
			sess.StartedAt = new("2026-05-14T10:00:00Z")
			sess.UserMessageCount = 2
		})
		require.NoError(t, d.ReplaceSessionUsageEvents(s.id, []UsageEvent{{
			SessionID:    s.id,
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  input,
			OutputTokens: output,
			DedupKey:     s.id + "-key",
		}}), "replace usage event for %s", s.id)
	}
}

func TestGetDailyUsageBranchBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedBranchUsageFixture(t, d)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "one day")

	byKey := map[BranchInfo]BranchBreakdown{}
	for _, b := range daily.Daily[0].BranchBreakdowns {
		byKey[BranchInfo{Project: b.Project, Branch: b.Branch}] = b
	}
	require.Len(t, byKey, 5, "one bucket per distinct (project, branch)")
	assert.Equal(t, 100, byKey[BranchInfo{Project: "proj-a", Branch: "main"}].InputTokens)
	assert.Equal(t, 200, byKey[BranchInfo{Project: "proj-a", Branch: "feature-x"}].InputTokens)
	assert.Equal(t, 300, byKey[BranchInfo{Project: "proj-b", Branch: "main"}].InputTokens)
	assert.Equal(t, 400, byKey[BranchInfo{Project: "proj-a", Branch: ""}].InputTokens)
	assert.Equal(t, 500, byKey[BranchInfo{Project: "proj-a", Branch: "unknown"}].InputTokens)
}

func TestGetDailyUsageGitBranchFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedBranchUsageFixture(t, d)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From:      "2026-05-14",
		To:        "2026-05-14",
		GitBranch: EncodeBranchFilterToken("proj-a", "main"),
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "one day")
	assert.Equal(t, 100, daily.Daily[0].InputTokens,
		"usage filter uses scoped (project, branch), not branch name alone")
}

func TestGetDailyUsageExcludeGitBranchFilter(t *testing.T) {
	tests := []struct {
		name             string
		gitBranch        string
		excludeGitBranch string
		wantInput        int
	}{
		{
			name:             "single pair excluded",
			excludeGitBranch: EncodeBranchFilterToken("proj-a", "main"),
			wantInput:        1400,
		},
		{
			name: "multiple pairs excluded",
			excludeGitBranch: encodeBranchFilterTokensForTest(
				BranchInfo{Project: "proj-a", Branch: "main"},
				BranchInfo{Project: "proj-b", Branch: "main"},
			),
			wantInput: 1100,
		},
		{
			name:             "same-named branch in another project stays",
			excludeGitBranch: EncodeBranchFilterToken("proj-b", "main"),
			wantInput:        1200,
		},
		{
			name:             "malformed token excludes nothing",
			excludeGitBranch: "no-separator-here",
			wantInput:        1500,
		},
		{
			name: "combined with include filter",
			gitBranch: encodeBranchFilterTokensForTest(
				BranchInfo{Project: "proj-a", Branch: "main"},
				BranchInfo{Project: "proj-a", Branch: "feature-x"},
			),
			excludeGitBranch: EncodeBranchFilterToken("proj-a", "main"),
			wantInput:        200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedBranchUsageFixture(t, d)

			daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
				From:             "2026-05-14",
				To:               "2026-05-14",
				GitBranch:        tt.gitBranch,
				ExcludeGitBranch: tt.excludeGitBranch,
			})
			require.NoError(t, err, "GetDailyUsage")
			require.Len(t, daily.Daily, 1, "one day")
			assert.Equal(t, tt.wantInput, daily.Daily[0].InputTokens)
		})
	}
}

func TestSplitBranchFilterTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []BranchInfo
	}{
		{"empty", "", []BranchInfo{}},
		{
			name: "round trip single",
			in:   EncodeBranchFilterToken("alpha", "main"),
			want: []BranchInfo{branchInfoForTest("alpha", "main")},
		},
		{
			name: "multiple",
			in: encodeBranchFilterTokensForTest(
				BranchInfo{Project: "alpha", Branch: "feat/x"},
				BranchInfo{Project: "beta", Branch: "main"},
			),
			want: []BranchInfo{
				branchInfoForTest("alpha", "feat/x"),
				branchInfoForTest("beta", "main"),
			},
		},
		{
			name: "comma in branch name round-trips",
			in:   EncodeBranchFilterToken("proj", "wip,test"),
			want: []BranchInfo{branchInfoForTest("proj", "wip,test")},
		},
		{
			name: "drops blank and separator-less tokens",
			in:   branchListSep + EncodeBranchFilterToken("alpha", "main") + branchListSep + "noseparator",
			want: []BranchInfo{branchInfoForTest("alpha", "main")},
		},
		{
			name: "empty branch component survives",
			in:   EncodeBranchFilterToken("alpha", ""),
			want: []BranchInfo{branchInfoForTest("alpha", "")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SplitBranchFilterTokens(tt.in))
		})
	}
}

func TestBranchFilterToken(t *testing.T) {
	tok, err := BranchFilterToken("proj", "")
	require.NoError(t, err)
	assert.Empty(t, tok, "empty branch means no filter")

	_, err = BranchFilterToken("", "main")
	assert.ErrorIs(t, err, ErrBranchWithoutProject)

	tok, err = BranchFilterToken("proj", "main")
	require.NoError(t, err)
	assert.Equal(t, EncodeBranchFilterToken("proj", "main"), tok)
}

func TestGetBranches(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "alpha", func(s *Session) {
		s.GitBranch = "main"
		s.UserMessageCount = 5
		s.EndedAt = new("2026-06-10T10:00:00Z")
	})
	// Older session on the same pair: MAX() keeps alpha/main at its most
	// recent activity, not this one.
	insertSession(t, d, "s1-old", "alpha", func(s *Session) {
		s.GitBranch = "main"
		s.UserMessageCount = 5
		s.EndedAt = new("2026-06-01T10:00:00Z")
	})
	insertSession(t, d, "s2", "alpha", func(s *Session) {
		s.GitBranch = "feat/x"
		s.UserMessageCount = 5
		s.EndedAt = new("2026-06-12T10:00:00Z")
	})
	// No ended_at: recency falls back to started_at.
	insertSession(t, d, "s3", "beta", func(s *Session) {
		s.GitBranch = "main"
		s.UserMessageCount = 5
		s.StartedAt = new("2026-06-11T10:00:00Z")
	})
	insertSession(t, d, "s4", "alpha", func(s *Session) {
		s.GitBranch = ""
		s.UserMessageCount = 5
		s.EndedAt = new("2026-06-08T10:00:00Z")
	})
	// Same timestamp as s4: the tie breaks alphabetically by pair.
	insertSession(t, d, "s5", "gamma", func(s *Session) {
		s.GitBranch = "solo"
		s.UserMessageCount = 1
		s.EndedAt = new("2026-06-08T10:00:00Z")
	})

	// Subagent/fork-only pair: visible only under BranchScopeAll, so the
	// activity/usage filter controls can offer branches their rollups count.
	insertSession(t, d, "s6", "delta", func(s *Session) {
		s.GitBranch = "fork-only"
		s.UserMessageCount = 5
		s.RelationshipType = "fork"
		s.EndedAt = new("2026-06-13T10:00:00Z")
	})

	all, err := d.GetBranches(context.Background(), BranchScopeRoots, false, false)
	require.NoError(t, err, "GetBranches includeAll")
	assert.Equal(t, []BranchInfo{
		branchInfoForTest("alpha", "feat/x"),
		branchInfoForTest("beta", "main"),
		branchInfoForTest("alpha", "main"),
		branchInfoForTest("alpha", ""),
		branchInfoForTest("gamma", "solo"),
	}, all, "pairs ordered by most recent activity, empty branch included")
	assert.NotContains(t, all, branchInfoForTest("delta", "fork-only"),
		"fork-only branch hidden from the root scope")

	withForks, err := d.GetBranches(
		context.Background(), BranchScopeAll, false, false)
	require.NoError(t, err, "GetBranches scope all")
	assert.Contains(t, withForks, branchInfoForTest("delta", "fork-only"),
		"fork-only branch included when scope is all")

	filtered, err := d.GetBranches(context.Background(), BranchScopeRoots, true, false)
	require.NoError(t, err, "GetBranches excludeOneShot")
	assert.NotContains(t, filtered, branchInfoForTest("gamma", "solo"),
		"one-shot branch excluded when excludeOneShot is set")
}

func TestSessionFilterGitBranchComposite(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "alpha-main", "alpha", func(s *Session) {
		s.GitBranch = "main"
	})
	insertSession(t, d, "alpha-feat", "alpha", func(s *Session) {
		s.GitBranch = "feat/x"
	})
	insertSession(t, d, "beta-main", "beta", func(s *Session) {
		s.GitBranch = "main"
	})
	insertSession(t, d, "alpha-empty", "alpha", func(s *Session) {
		s.GitBranch = ""
	})
	insertSession(t, d, "alpha-unknown", "alpha", func(s *Session) {
		s.GitBranch = "unknown"
	})

	// Filtering by (alpha, main) must not match (beta, main): the grain is
	// (project, branch), so same-named branches across projects stay distinct.
	requireSessions(t, d, SessionFilter{
		GitBranch: EncodeBranchFilterToken("alpha", "main"),
	}, []string{"alpha-main"})

	requireSessions(t, d, SessionFilter{
		GitBranch: encodeBranchFilterTokensForTest(
			BranchInfo{Project: "alpha", Branch: "feat/x"},
			BranchInfo{Project: "beta", Branch: "main"},
		),
	}, []string{"alpha-feat", "beta-main"})

	requireSessions(t, d, SessionFilter{
		GitBranch: EncodeBranchFilterToken("alpha", ""),
	}, []string{"alpha-empty"})

	requireSessions(t, d, SessionFilter{
		GitBranch: EncodeBranchFilterToken("alpha", "unknown"),
	}, []string{"alpha-unknown"})
}

// A usage-page deselect-all excludes every known (project, branch)
// pair; over ~1000 pairs a flat OR chain exceeds SQLite's expression
// depth limit ("Expression tree is too large"), so the predicate must
// nest as a balanced tree. Exercised end-to-end through GetDailyUsage
// on both the include and exclude sides.
func TestGetDailyUsageManyBranchPairs(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedBranchUsageFixture(t, d)

	tokens := make([]BranchInfo, 0, 1500)
	for i := range 1500 {
		tokens = append(tokens, BranchInfo{
			Project: "proj-a",
			Branch:  "branch-" + strconv.Itoa(i),
		})
	}
	tokens = append(tokens, branchInfoForTest("proj-a", "main"))
	list := encodeBranchFilterTokensForTest(tokens...)

	included, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14", To: "2026-05-14",
		GitBranch: list,
	})
	require.NoError(t, err)
	assert.Equal(t, 110, included.Totals.InputTokens+included.Totals.OutputTokens,
		"include list should match only (proj-a, main)")

	excluded, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-05-14", To: "2026-05-14",
		ExcludeGitBranch: list,
	})
	require.NoError(t, err)
	assert.Equal(t, 1540, excluded.Totals.InputTokens+excluded.Totals.OutputTokens,
		"exclude list should drop only (proj-a, main)")
}
