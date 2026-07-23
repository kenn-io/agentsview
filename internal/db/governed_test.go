package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func explicitMapping(machine, prefix, project string) WorktreeProjectMapping {
	return WorktreeProjectMapping{
		Machine: machine, PathPrefix: prefix,
		Layout: WorktreeMappingLayoutExplicit, Project: project,
		Enabled: true,
	}
}

func repoDotWorktreesMapping(machine, prefix string) WorktreeProjectMapping {
	return WorktreeProjectMapping{
		Machine: machine, PathPrefix: prefix,
		Layout: WorktreeMappingLayoutRepoDotWorktrees, Enabled: true,
	}
}

func TestEvaluateGovernedSessions(t *testing.T) {
	t.Run("boundary foo vs foobar", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "/work/foo", "alpha"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "in", Machine: "ws", Project: "x",
					Cwd: "/work/foo/sub"},
				{SessionID: "out", Machine: "ws", Project: "x",
					Cwd: "/work/foobar/sub"},
			})
		assert.Equal(t, 1, got.GovernedSessions)
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work/foo"}])
	})

	t.Run("windows separators canonicalize", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "C:/work/repo", "alpha"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "win", Machine: "ws", Project: "x",
					Cwd: `C:\work\repo\sub`},
			})
		assert.Equal(t, 1, got.GovernedSessions)
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "C:/work/repo"}])
	})

	t.Run("unresolved repo_dot_worktrees is not governed", func(t *testing.T) {
		// cwd sits directly under the prefix at "<repo>.worktrees" with no
		// branch segment beneath it, so worktreePathMatches passes but
		// resolveRepoDotWorktrees fails (idx < 0 in the branch lookup).
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				repoDotWorktreesMapping("ws", "/work/service"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "branchless", Machine: "ws", Project: "x",
					Cwd: "/work/service/service.worktrees"},
			})
		assert.Equal(t, 0, got.GovernedSessions)
		assert.Empty(t, got.SessionsByRule)
	})

	t.Run("sibling fallback resolves unambiguous empty cwd", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "/work/a", "alpha"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "reference", Machine: "ws", Project: "x",
					Cwd: "/work/a/sub", FilePath: "shared.jsonl"},
				{SessionID: "empty-cwd", Machine: "ws", Project: "x",
					Cwd: "", FilePath: "shared.jsonl"},
			})
		assert.Equal(t, 2, got.GovernedSessions)
		assert.Equal(t, 2, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work/a"}])
	})

	t.Run("conflicting siblings block empty cwd fallback", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "/work/a", "alpha"),
				explicitMapping("ws", "/work/b", "beta"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "ref-a", Machine: "ws", Project: "x",
					Cwd: "/work/a/sub", FilePath: "shared.jsonl"},
				{SessionID: "ref-b", Machine: "ws", Project: "x",
					Cwd: "/work/b/sub", FilePath: "shared.jsonl"},
				{SessionID: "empty-cwd", Machine: "ws", Project: "x",
					Cwd: "", FilePath: "shared.jsonl"},
			})
		assert.Equal(t, 2, got.GovernedSessions)
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work/a"}])
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work/b"}])
	})

	t.Run("cross-archive isolation", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{SourceArchiveID: "A", Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "/work/foo", "alpha"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "wrong-archive", Machine: "ws", Project: "x",
					Cwd: "/work/foo/sub", SourceArchiveID: "B"},
				{SessionID: "empty-archive", Machine: "ws", Project: "x",
					Cwd: "/work/foo/sub", SourceArchiveID: ""},
				{SessionID: "matching-archive", Machine: "ws", Project: "x",
					Cwd: "/work/foo/sub", SourceArchiveID: "A"},
			})
		assert.Equal(t, 1, got.GovernedSessions,
			"only the row whose SourceArchiveID matches the rule set's archive is governed")
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			SourceArchiveID: "A", Machine: "ws", PathPrefix: "/work/foo"}])
	})

	t.Run("longest prefix wins", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				explicitMapping("ws", "/work", "outer"),
				explicitMapping("ws", "/work/repo", "inner"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "nested", Machine: "ws", Project: "x",
					Cwd: "/work/repo/sub"},
			})
		assert.Equal(t, 1, got.GovernedSessions)
		assert.Equal(t, 1, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work/repo"}])
		assert.Equal(t, 0, got.SessionsByRule[GovernedRuleKey{
			Machine: "ws", PathPrefix: "/work"}])
	})

	t.Run("dynamic label rule attribution", func(t *testing.T) {
		got := EvaluateGovernedSessions(
			[]ArchiveMappings{{Mappings: []WorktreeProjectMapping{
				repoDotWorktreesMapping("ws", "/work/service"),
			}}},
			[]MappingEvaluationRow{
				{SessionID: "branch", Machine: "ws", Project: "x",
					Cwd: "/work/service/alpha.worktrees/feature"},
			})
		assert.Equal(t, 1, got.GovernedSessions)
		ruleKey := GovernedRuleKey{Machine: "ws", PathPrefix: "/work/service"}
		require.Contains(t, got.DynamicLabelRules, "alpha")
		assert.Contains(t, got.DynamicLabelRules["alpha"], ruleKey)
	})
}

// TestEvaluateGovernedSessionsMatchesApplyEvaluator seeds a fixture mixing
// inside/outside-prefix cwds, a windows-style cwd, an empty cwd with a
// resolvable sibling file_path, and a foobar boundary neighbor, then checks
// that EvaluateGovernedSessions' GovernedSessions count matches the real
// production apply path's MatchedSessions count on the same rows. Matched
// (production) and governed (this evaluator) share the same definition:
// resolution succeeds, whether or not it changes the stored project.
func TestEvaluateGovernedSessionsMatchesApplyEvaluator(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	machine := "differential.example"
	root := t.TempDir()
	prefix := filepath.Join(root, "foo")
	sharedFilePath := filepath.Join(root, "shared-session.jsonl")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: machine, PathPrefix: prefix, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "create mapping")

	type seed struct {
		id       string
		cwd      string
		project  string
		filePath string
	}
	windowsCwd := strings.ReplaceAll(filepath.Join(prefix, "w"), "/", `\`)
	seeds := []seed{
		{id: "in-fresh", cwd: filepath.Join(prefix, "a"), project: "misc"},
		{id: "in-samelabel", cwd: filepath.Join(prefix, "b"), project: "alpha"},
		{id: "out-boundary", cwd: filepath.Join(root, "foobar", "x"), project: "misc"},
		{id: "out-unrelated", cwd: filepath.Join(root, "elsewhere"), project: "misc"},
		{id: "win-style", cwd: windowsCwd, project: "misc"},
		{id: "sibling-ref", cwd: filepath.Join(prefix, "c"), project: "misc",
			filePath: sharedFilePath},
		{id: "empty-sibling", cwd: "", project: "misc", filePath: sharedFilePath},
		{id: "empty-nosibling", cwd: "", project: "misc"},
	}
	rows := make([]MappingEvaluationRow, 0, len(seeds))
	for _, s := range seeds {
		session := Session{
			ID: s.id, Machine: machine, Agent: "claude", Cwd: s.cwd, Project: s.project,
		}
		if s.filePath != "" {
			filePath := s.filePath
			session.FilePath = &filePath
		}
		require.NoError(t, d.UpsertSession(session), "seed session %s", s.id)
		rows = append(rows, MappingEvaluationRow{
			SessionID: s.id, Machine: machine, Project: s.project,
			Cwd: s.cwd, FilePath: s.filePath,
		})
	}

	applyResult, err := d.ApplyWorktreeProjectMappings(ctx, machine)
	require.NoError(t, err, "apply worktree mappings")

	mappings, err := d.ListActiveWorktreeProjectMappings(ctx, machine)
	require.NoError(t, err, "load active mappings")
	evaluation := EvaluateGovernedSessions(
		[]ArchiveMappings{{Mappings: mappings}}, rows,
	)

	assert.Equal(t, applyResult.MatchedSessions, evaluation.GovernedSessions,
		"governed count must match production apply matched count")
	assert.Equal(t, 5, evaluation.GovernedSessions,
		"in-fresh, in-samelabel, win-style, sibling-ref, empty-sibling are governed")
}

// TestGovernedEvaluationTouchesOnlyRuleMachines is a cardinality-scaling
// regression: the candidate-row fetch that feeds governed evaluation must be
// bounded by the number of machines carrying an ENABLED worktree mapping,
// not by total archive size and not merely by having some mapping (enabled
// or disabled). It seeds one enabled mapping on machine "ws" with 3
// sessions, a DISABLED mapping on "disabled-host" with 200 sessions, and 200
// more unrelated sessions on a machine with no mapping at all, then drives
// the real production filtering step (governedCandidateMachines, fed by
// ListAllWorktreeProjectMappings) into the candidate-row loader and asserts
// it returns exactly the 3 "ws" rows.
func TestGovernedEvaluationTouchesOnlyRuleMachines(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work/repo", Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "create enabled mapping")

	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "disabled-host", PathPrefix: "/work/other", Project: "beta",
		Enabled: false,
	})
	require.NoError(t, err, "create disabled mapping")

	wantIDs := make([]string, 0, 3)
	for i := range 3 {
		id := fmt.Sprintf("ws-%d", i)
		wantIDs = append(wantIDs, id)
		insertSession(t, d, id, "misc", func(s *Session) {
			s.Machine = "ws"
			s.Cwd = fmt.Sprintf("/work/repo/sub-%d", i)
		})
	}
	// 200 sessions on a machine whose only mapping is disabled: these must
	// not enter the candidate set even though the machine has a mapping.
	for i := range 200 {
		insertSession(t, d, fmt.Sprintf("disabled-%04d", i), "misc", func(s *Session) {
			s.Machine = "disabled-host"
		})
	}
	for i := range 200 {
		insertSession(t, d, fmt.Sprintf("norule-%04d", i), "misc", func(s *Session) {
			s.Machine = "no-rule-host"
		})
	}

	mappings, err := d.ListAllWorktreeProjectMappings(ctx)
	require.NoError(t, err, "list mappings")

	machines := governedCandidateMachines(mappings)
	rows, err := d.projectInventoryCandidateRows(ctx, "archive-1", machines)
	require.NoError(t, err, "load candidate rows")
	require.Len(t, rows, 3,
		"fetch must be bounded by machines with an ENABLED rule, "+
			"not by mapped machines or archive size")

	gotIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		assert.Equal(t, "ws", row.Machine)
		assert.Equal(t, "archive-1", row.SourceArchiveID)
		gotIDs = append(gotIDs, row.SessionID)
	}
	assert.ElementsMatch(t, wantIDs, gotIDs)
}

// TestGovernedCandidateMachinesExcludesDisabledMappings is a narrow unit
// test on the enabled-filtering step in isolation: a machine whose only
// mapping is disabled must not appear in the candidate machine set, even
// though it has a mapping row.
func TestGovernedCandidateMachinesExcludesDisabledMappings(t *testing.T) {
	mappings := []WorktreeProjectMapping{
		{Machine: "ws", PathPrefix: "/work/repo", Project: "alpha", Enabled: true},
		{Machine: "disabled-host", PathPrefix: "/work/other", Project: "beta",
			Enabled: false},
	}

	machines := governedCandidateMachines(mappings)

	assert.Equal(t, map[string]struct{}{"ws": {}}, machines,
		"only machines with an enabled mapping belong in the candidate set")
}
