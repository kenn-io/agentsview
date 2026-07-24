package db

import (
	"context"
	"strconv"
	"testing"

	"go.kenn.io/agentsview/internal/export"

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

func TestSanitizeDailyUsageProjectLabelsKeepsBranchIdentity(t *testing.T) {
	first := "/Users/example/one/private/repo"
	second := "/Users/example/two/private/repo"
	result := DailyUsageResult{Daily: []DailyUsageEntry{{
		BranchBreakdowns: []BranchBreakdown{
			{Project: first, Branch: "main", Cost: 1},
			{Project: second, Branch: "main", Cost: 2},
		},
	}}}
	projects := map[string]export.ProjectMapEntry{
		first:  {ProjectKey: "pl1:sha256:first"},
		second: {ProjectKey: "pl1:sha256:second"},
	}

	SanitizeDailyUsageProjectLabelsWithCatalog(&result, projects)

	require.Len(t, result.Daily, 1)
	require.Len(t, result.Daily[0].BranchBreakdowns, 2)
	assert.Equal(t, "pl1:sha256:first", result.Daily[0].BranchBreakdowns[0].ProjectKey)
	assert.Equal(t, "pl1:sha256:second", result.Daily[0].BranchBreakdowns[1].ProjectKey)
	assert.Empty(t, result.Daily[0].BranchBreakdowns[0].Project)
	assert.Empty(t, result.Daily[0].BranchBreakdowns[1].Project)
}

func TestGetDailyUsageBranchBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedBranchUsageFixture(t, d)

	withoutBranches, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage without branch breakdowns")
	require.Len(t, withoutBranches.Daily, 1, "one day")
	assert.Empty(t, withoutBranches.Daily[0].BranchBreakdowns)
	assert.NotEmpty(t, withoutBranches.Daily[0].ProjectBreakdowns)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From:             "2026-05-14",
		To:               "2026-05-14",
		Breakdowns:       true,
		BranchBreakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "one day")
	assert.Equal(t, withoutBranches.Totals, daily.Totals)

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

// API contract: a git_branch value whose tokens all lack the pair
// separator decodes to an empty pair set and fails closed to zero rows.
// The frontend relies on this by sending a deliberately separator-less
// token when the sidebar branch filter and the usage page's local
// selection have no overlap, so the disjoint filters must yield an
// empty result rather than an error or unfiltered totals.
func TestGetDailyUsageMalformedGitBranchTokenFailsClosed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedBranchUsageFixture(t, d)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From:      "2026-05-14",
		To:        "2026-05-14",
		GitBranch: NoBranchMatchToken,
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.Empty(t, daily.Daily,
		"separator-less branch token must fail closed, not broaden")
	assert.Zero(t, daily.Totals.InputTokens)
	assert.Zero(t, daily.Totals.TotalCost)
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

	all, err := d.GetBranches(context.Background(), BranchQuery{
		Scope: BranchScopeRoots,
		Limit: 100,
	})
	require.NoError(t, err, "GetBranches includeAll")
	assert.Equal(t, BranchResult{Branches: []BranchOption{
		{Branch: "feat/x"},
		{Branch: "main"},
		{Branch: ""},
		{Branch: "solo"},
	}}, all, "branch names deduplicated by latest activity, empty branch included")

	withForks, err := d.GetBranches(context.Background(), BranchQuery{
		Scope: BranchScopeAll,
		Limit: 100,
	})
	require.NoError(t, err, "GetBranches scope all")
	assert.Contains(t, withForks.Branches, BranchOption{Branch: "fork-only"},
		"fork-only branch included when scope is all")

	filtered, err := d.GetBranches(context.Background(), BranchQuery{
		Scope:          BranchScopeRoots,
		ExcludeOneShot: true,
		Limit:          100,
	})
	require.NoError(t, err, "GetBranches excludeOneShot")
	assert.NotContains(t, filtered.Branches, BranchOption{Branch: "solo"},
		"one-shot branch excluded when excludeOneShot is set")
}

func TestGetBranchesProjectsSearchAndLimit(t *testing.T) {
	d := testDB(t)
	seed := func(id, project, branch, endedAt string) {
		insertSession(t, d, id, project, func(s *Session) {
			s.GitBranch = branch
			s.UserMessageCount = 5
			s.EndedAt = new(endedAt)
		})
	}

	seed("alpha-new", "alpha", "feature-new", "2026-06-13T10:00:00Z")
	seed("beta-last", "beta", "feature-last", "2026-06-11T10:00:00Z")
	seed("alpha-shared", "alpha", "feature-shared", "2026-06-10T10:00:00Z")
	seed("beta-shared", "beta", "feature-shared", "2026-06-09T10:00:00Z")
	seed("gamma-shared", "gamma", "feature-shared", "2026-06-20T10:00:00Z")
	seed("beta-miss", "beta", "bugfix", "2026-06-30T10:00:00Z")
	seed("project-name-match", "feature-project", "main", "2026-07-01T10:00:00Z")
	seed("alpha-empty", "alpha", "", "2026-06-08T10:00:00Z")

	got, err := d.GetBranches(context.Background(), BranchQuery{
		Projects: []string{"alpha", "beta", "feature-project"},
		Search:   "FEATURE",
		Limit:    2,
	})
	require.NoError(t, err)
	assert.Equal(t, BranchResult{
		Branches: []BranchOption{{Branch: "feature-new"}, {Branch: "feature-last"}},
		HasMore:  true,
	}, got, "project filter applies before dedupe and recency ordering")

	empty, err := d.GetBranches(context.Background(), BranchQuery{
		Projects: []string{"alpha"},
		Limit:    100,
	})
	require.NoError(t, err)
	assert.Contains(t, empty.Branches, BranchOption{Branch: ""})
}

func TestNormalizeBranchQueryDefaultsAndCapsLimit(t *testing.T) {
	assert.Equal(t, 100, NormalizeBranchQuery(BranchQuery{}).Limit)
	assert.Equal(t, 100, NormalizeBranchQuery(BranchQuery{Limit: 101}).Limit)
	assert.Equal(t, 1, NormalizeBranchQuery(BranchQuery{Limit: 1}).Limit)
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
