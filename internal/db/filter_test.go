package db

import (
	"context"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneFilterZeroValue(t *testing.T) {
	f := PruneFilter{}

	assert.False(t, f.HasFilters(),
		"HasFilters() returned true for zero value")

	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 0
	})
	insertSession(t, d, "s2", "p", func(s *Session) {
		s.MessageCount = 5
	})

	_, err := d.FindPruneCandidates(f)
	requireErrContains(t, err, "at least one filter is required")
}

func TestSessionFilterDateFields(t *testing.T) {
	d := testDB(t)
	sessionSet(t, d)

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name: "ExactDate",
			filter: SessionFilter{
				Date: "2024-06-01",
			},
			want: []string{"s1"},
		},
		{
			name: "DateRange",
			filter: SessionFilter{
				DateFrom: "2024-06-01",
				DateTo:   "2024-06-02",
			},
			want: []string{"s1", "s2"},
		},
		{
			name: "DateFrom",
			filter: SessionFilter{
				DateFrom: "2024-06-02",
			},
			want: []string{"s2", "s3"},
		},
		{
			name: "DateTo",
			filter: SessionFilter{
				DateTo: "2024-06-01",
			},
			want: []string{"s1"},
		},
		{
			name: "MinMessages",
			filter: SessionFilter{
				MinMessages: 10,
			},
			want: []string{"s2", "s3"},
		},
		{
			name: "MaxMessages",
			filter: SessionFilter{
				MaxMessages: 10,
			},
			want: []string{"s1"},
		},
		{
			name: "CombinedDateAndMessages",
			filter: SessionFilter{
				DateFrom:    "2024-06-02",
				MinMessages: 20,
			},
			want: []string{"s3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

func TestSessionFilterActiveSince(t *testing.T) {
	d := testDB(t)

	// Session that started and ended long ago.
	insertSession(t, d, "old", "proj", func(s *Session) {
		s.StartedAt = new("2024-01-01T10:00:00Z")
		s.EndedAt = new("2024-01-01T11:00:00Z")
		s.MessageCount = 5
	})

	// Session that started long ago but ended recently.
	insertSession(t, d, "recent-end", "proj", func(s *Session) {
		s.StartedAt = new("2024-01-01T10:00:00Z")
		s.EndedAt = new("2024-06-03T10:00:00Z")
		s.MessageCount = 5
	})

	// Session that started recently, no ended_at.
	insertSession(t, d, "recent-start", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-03T08:00:00Z")
		s.MessageCount = 5
	})

	// Session with no started_at or ended_at, only created_at
	// (created_at defaults to now in schema, but here we set
	// started_at to nil; the fallback is created_at).
	insertSession(t, d, "no-times", "proj", func(s *Session) {
		s.CreatedAt = "2024-06-04T00:00:00Z"
		s.MessageCount = 5
	})

	// no-times has created_at = 2024-06-04, so it
	// matches any past cutoff.
	tests := []struct {
		name        string
		activeSince string
		want        []string
	}{
		{
			name:        "ExcludesOldEndedAt",
			activeSince: "2024-06-03T00:00:00Z",
			want:        []string{"recent-end", "recent-start", "no-times"}, // old excluded
		},
		{
			name:        "NarrowCutoffOnlyCreatedAtAfterCutoff",
			activeSince: "2024-06-03T12:00:00Z",
			want:        []string{"no-times"}, // only no-times (created_at=2024-06-04) survives
		},
		{
			name:        "IncludesAll",
			activeSince: "2024-01-01T00:00:00Z",
			want:        []string{"old", "recent-end", "recent-start", "no-times"},
		},
		{
			name:        "EmptyMeansNoFilter",
			activeSince: "",
			want:        []string{"old", "recent-end", "recent-start", "no-times"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				ActiveSince: tt.activeSince,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestSessionFilterMinUserMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "one-shot", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "short", "proj", func(s *Session) {
		s.MessageCount = 6
		s.UserMessageCount = 3
	})
	insertSession(t, d, "long", "proj", func(s *Session) {
		s.MessageCount = 20
		s.UserMessageCount = 10
	})

	tests := []struct {
		name            string
		minUserMessages int
		want            []string
	}{
		{"NoFilter", 0, []string{"one-shot", "short", "long"}},
		{"Min1", 1, []string{"one-shot", "short", "long"}},
		{"Min2", 2, []string{"short", "long"}},
		{"Min5", 5, []string{"long"}},
		{"Min10", 10, []string{"long"}},
		{"Min11", 11, []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				MinUserMessages: tt.minUserMessages,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestSessionFilterExcludeProject(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "known", "my_project", func(s *Session) {
		s.MessageCount = 5
	})
	insertSession(t, d, "unknown1", "unknown", func(s *Session) {
		s.MessageCount = 3
	})
	insertSession(t, d, "unknown2", "unknown", func(s *Session) {
		s.MessageCount = 7
	})

	tests := []struct {
		name           string
		excludeProject string
		want           []string
	}{
		{"NoFilter", "", []string{"known", "unknown1", "unknown2"}},
		{"ExcludeUnknown", "unknown", []string{"known"}},
		{"ExcludeMyProject", "my_project", []string{"unknown1", "unknown2"}},
		{"ExcludeNonexistent", "nope", []string{"known", "unknown1", "unknown2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				ExcludeProject: tt.excludeProject,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestSessionFilterMachineMultiSelect(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "laptop", "proj", func(s *Session) {
		s.Machine = "laptop"
		s.MessageCount = 5
	})
	insertSession(t, d, "desktop", "proj", func(s *Session) {
		s.Machine = "desktop"
		s.MessageCount = 5
	})
	insertSession(t, d, "server", "proj", func(s *Session) {
		s.Machine = "server"
		s.MessageCount = 5
	})

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name:   "SingleMachine",
			filter: SessionFilter{Machine: "laptop"},
			want:   []string{"laptop"},
		},
		{
			name:   "MultipleMachines",
			filter: SessionFilter{Machine: "laptop,server"},
			want:   []string{"laptop", "server"},
		},
		{
			name:   "UnknownMachineIgnored",
			filter: SessionFilter{Machine: "desktop,unknown"},
			want:   []string{"desktop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

func TestListSessionsExcludesRelationshipTypes(t *testing.T) {
	d := testDB(t)

	// Regular session (no relationship_type).
	insertSession(t, d, "normal", "proj", func(s *Session) {
		s.MessageCount = 5
	})

	// Subagent session -- should be excluded.
	insertSession(t, d, "sub", "proj", func(s *Session) {
		s.MessageCount = 5
		s.RelationshipType = "subagent"
	})

	// Fork session -- should be excluded.
	insertSession(t, d, "fork1", "proj", func(s *Session) {
		s.MessageCount = 5
		s.ParentSessionID = new("normal")
		s.RelationshipType = "fork"
	})

	f := SessionFilter{}
	requireSessions(t, d, f, []string{"normal"})
}

func TestIncludeChildrenBypassesFilters(t *testing.T) {
	d := testDB(t)

	// Parent session: claude agent, dated 2024-06-01, 10 messages.
	insertSession(t, d, "parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T11:00:00Z")
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	// Subagent child: different agent, different date, 1 message.
	insertSession(t, d, "child-sub", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-07-15T10:00:00Z")
		s.EndedAt = new("2024-07-15T11:00:00Z")
		s.MessageCount = 1
		s.UserMessageCount = 1
		s.ParentSessionID = new("parent")
		s.RelationshipType = "subagent"
	})

	// Fork child: same agent but fewer messages than filter.
	insertSession(t, d, "child-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.EndedAt = new("2024-06-02T11:00:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("parent")
		s.RelationshipType = "fork"
	})

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name: "AgentFilterBypassesChildren",
			filter: SessionFilter{
				IncludeChildren: true,
				Agent:           "claude",
			},
			want: []string{"parent", "child-sub", "child-fork"},
		},
		{
			name: "DateFilterBypassesChildren",
			filter: SessionFilter{
				IncludeChildren: true,
				Date:            "2024-06-01",
			},
			want: []string{"parent", "child-sub", "child-fork"},
		},
		{
			name: "MinMessagesFilterBypassesChildren",
			filter: SessionFilter{
				IncludeChildren: true,
				MinMessages:     5,
			},
			want: []string{"parent", "child-sub", "child-fork"},
		},
		{
			name: "WithoutIncludeChildrenFiltersNormally",
			filter: SessionFilter{
				Agent: "claude",
			},
			// children excluded by default (subagent/fork filtered)
			want: []string{"parent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

func TestIncludeChildrenScopesToMatchingParent(t *testing.T) {
	d := testDB(t)

	// Parent A: claude agent — matches agent filter.
	insertSession(t, d, "parentA", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 5
		s.UserMessageCount = 3
	})
	// Child of parent A — should be included (parent matches).
	insertSession(t, d, "childA", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("parentA")
		s.RelationshipType = "subagent"
	})

	// Parent B: codex agent — does NOT match agent filter.
	insertSession(t, d, "parentB", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 5
		s.UserMessageCount = 3
	})
	// Child of parent B.
	insertSession(t, d, "childB", "proj", func(s *Session) {
		s.Agent = "gemini"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("parentB")
		s.RelationshipType = "subagent"
	})

	// Parent C: gemini agent.
	insertSession(t, d, "parentC", "proj", func(s *Session) {
		s.Agent = "gemini"
		s.MessageCount = 5
		s.UserMessageCount = 3
	})
	// Child of parent C — gemini child of gemini parent.
	// When filtering agent=claude, neither parent nor child
	// match, so both should be excluded.
	insertSession(t, d, "childC", "proj", func(s *Session) {
		s.Agent = "gemini"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("parentC")
		s.RelationshipType = "subagent"
	})

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name: "ChildOfMatchingParentIncluded",
			filter: SessionFilter{
				IncludeChildren: true,
				Agent:           "claude",
			},
			want: []string{"parentA", "childA"},
		},
		{
			// Subagent/fork rows can only be included via their
			// parent, never as direct matches. childA (codex
			// subagent of claude parentA) is excluded because
			// its parent doesn't match Agent=codex. childB is
			// included because its parent parentB matches.
			name: "SubagentOnlyViaMatchingParent",
			filter: SessionFilter{
				IncludeChildren: true,
				Agent:           "codex",
			},
			want: []string{"parentB", "childB"},
		},
		{
			// Neither parentC (gemini) nor childC (gemini)
			// match agent=claude, and neither parent matches
			// either, so both are excluded.
			name: "UnrelatedChildExcluded",
			filter: SessionFilter{
				IncludeChildren: true,
				Agent:           "claude",
			},
			want: []string{"parentA", "childA"},
		},
		{
			name: "NoFilterReturnsAll",
			filter: SessionFilter{
				IncludeChildren: true,
			},
			want: []string{
				"parentA", "childA", "parentB", "childB",
				"parentC", "childC",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

// TestIncludeChildrenExcludesOrphanSubagents reproduces the
// sidebar bug where subagents whose parent was rotated off disk
// by Claude Code (or whose parent is excluded by is_automated)
// surfaced as fake root groups. With IncludeChildren=true and
// ExcludeAutomated active, a subagent row must ONLY appear when
// its parent is loaded and also passes the filter — never on
// its own.
func TestIncludeChildrenExcludesOrphanSubagents(t *testing.T) {
	d := testDB(t)

	// Legitimate case: non-automated root with a subagent child.
	insertSession(t, d, "root", "proj", func(s *Session) {
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	insertSession(t, d, "root-sub", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})

	// Orphan case 1: subagent whose parent doesn't exist in
	// the sessions table (parent JSONL was deleted from disk).
	insertSession(t, d, "orphan-sub", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("missing-parent-id")
		s.RelationshipType = "subagent"
	})

	// Orphan case 2: subagent whose parent IS loaded but is
	// automated, so it fails the ExcludeAutomated filter.
	fm := "You are a code reviewer. Review the code."
	insertSession(t, d, "auto-root", "proj", func(s *Session) {
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "auto-sub", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("auto-root")
		s.RelationshipType = "subagent"
	})

	// Orphan case 3: fork whose parent is missing.
	insertSession(t, d, "orphan-fork", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("also-missing")
		s.RelationshipType = "fork"
	})

	f := SessionFilter{
		IncludeChildren:  true,
		ExcludeAutomated: true,
	}
	// Expected: root + its subagent survive; all three orphans
	// are excluded. auto-root is filtered out by ExcludeAutomated,
	// which also drops auto-sub (its parent is no longer loaded).
	requireSessions(t, d, f, []string{"root", "root-sub"})
}

// TestIncludeChildrenKeepsNestedDescendants guards against a
// regression where a fork spawned inside a subagent thread
// (root → subagent → fork) was dropped. The direct-match side
// excludes fork rows, and a naive subquery that also excluded
// subagent rows would reject the fork's immediate parent. The
// parent-side subquery must therefore drop the relationship
// guard so depth-2+ descendants stay visible.
func TestIncludeChildrenKeepsNestedDescendants(t *testing.T) {
	d := testDB(t)

	// Root: non-automated, multi-turn, claude.
	insertSession(t, d, "root", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	// Subagent child of root.
	insertSession(t, d, "sub", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})
	// Fork spawned inside the subagent thread (depth-2).
	insertSession(t, d, "nested-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 3
		s.UserMessageCount = 1
		s.ParentSessionID = new("sub")
		s.RelationshipType = "fork"
	})

	f := SessionFilter{
		IncludeChildren:  true,
		ExcludeAutomated: true,
	}
	requireSessions(t, d, f, []string{
		"root", "sub", "nested-fork",
	})
}

// TestIncludeChildrenExcludesFilteredNestedRoots guards against
// the case where a user filter fails at every level of a chain.
// Shape: root(agent=claude) → sub(agent=codex, subagent) →
// nested-fork(agent=codex, fork). Under Agent=codex, root fails
// the filter, sub fails the relationship guard, and nested-fork
// fails the relationship guard. A one-level parent subquery
// would see sub passing the agent filter and include nested-fork
// — which then arrives at the frontend without its parent chain
// and renders as a fake root group. The recursive CTE refuses
// the whole subtree because nothing qualifies as a tree root.
func TestIncludeChildrenExcludesFilteredNestedRoots(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "root", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	insertSession(t, d, "sub", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})
	insertSession(t, d, "nested-fork", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 3
		s.UserMessageCount = 1
		s.ParentSessionID = new("sub")
		s.RelationshipType = "fork"
	})

	f := SessionFilter{
		IncludeChildren: true,
		Agent:           "codex",
	}
	// No codex session has relationship_type = '' (root), so
	// the CTE has no seed and returns no rows.
	requireSessions(t, d, f, []string{})
}

// TestIncludeChildrenNoFiltersExcludesOrphanChildren verifies
// that the relationship guard applies even when no user
// filters are active. The prior early-return on !hasFilters
// left orphan subagent/fork rows unguarded; toggling
// "include automated" with nothing else selected could
// resurrect them as fake roots.
func TestIncludeChildrenNoFiltersExcludesOrphanChildren(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "root", "proj", func(s *Session) {
		s.MessageCount = 5
		s.UserMessageCount = 3
	})
	insertSession(t, d, "child", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})
	insertSession(t, d, "orphan", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("nowhere")
		s.RelationshipType = "subagent"
	})

	f := SessionFilter{IncludeChildren: true}
	// No user filters, but the guard still applies: orphan is
	// excluded, legitimate root+child survive.
	requireSessions(t, d, f, []string{"root", "child"})
}

func TestIncludeChildrenExcludeOneShotAgent(t *testing.T) {
	d := testDB(t)

	// Multi-message claude root.
	insertSession(t, d, "root", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	// One-shot subagent (codex) — should be included via parent
	// despite ExcludeOneShot and different agent.
	insertSession(t, d, "sub-codex", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})
	// One-shot fork (claude) — should be included via parent.
	insertSession(t, d, "fork-1msg", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "fork"
	})
	// One-shot standalone (not a child) — should be excluded.
	insertSession(t, d, "standalone", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name: "DefaultSidebar_OneShotChildrenKept",
			filter: SessionFilter{
				IncludeChildren: true,
				ExcludeOneShot:  true,
			},
			want: []string{"root", "sub-codex", "fork-1msg"},
		},
		{
			name: "AgentFilter_OneShotChildrenKept",
			filter: SessionFilter{
				IncludeChildren: true,
				ExcludeOneShot:  true,
				Agent:           "claude",
			},
			want: []string{
				"root", "sub-codex", "fork-1msg",
			},
		},
		{
			name: "WithoutIncludeChildren_OneShotExcluded",
			filter: SessionFilter{
				ExcludeOneShot: true,
			},
			want: []string{"root"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

func TestActiveSinceUsesEndedAtOverStartedAt(t *testing.T) {
	d := testDB(t)

	// Session started in January, ended in June.
	// A date_from filter for June would miss it (started too early),
	// but active_since should catch it via ended_at.
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.StartedAt = new("2024-01-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T10:00:00Z")
		s.MessageCount = 5
	})

	tests := []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{
			name:   "DateFrom misses due to early StartedAt",
			filter: SessionFilter{DateFrom: "2024-06-01"},
			want:   []string{},
		},
		{
			name:   "ActiveSince catches due to later EndedAt",
			filter: SessionFilter{ActiveSince: "2024-06-01T00:00:00Z"},
			want:   []string{"s1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSessions(t, d, tt.filter, tt.want)
		})
	}
}

func TestSessionFilterExcludeOneShot(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "zero", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 0
	})
	insertSession(t, d, "one", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "two", "proj", func(s *Session) {
		s.MessageCount = 5
		s.UserMessageCount = 2
	})
	insertSession(t, d, "ten", "proj", func(s *Session) {
		s.MessageCount = 20
		s.UserMessageCount = 10
	})

	tests := []struct {
		name           string
		excludeOneShot bool
		want           []string
	}{
		{
			"IncludeAll",
			false,
			[]string{"zero", "one", "two", "ten"},
		},
		{
			"ExcludeOneShot",
			true,
			[]string{"two", "ten"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				ExcludeOneShot: tt.excludeOneShot,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestGetMachinesExcludeOneShot(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Machine = "laptop"
		s.UserMessageCount = 1
	})
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Machine = "desktop"
		s.UserMessageCount = 5
	})

	all, err := d.GetMachines(context.Background(), false, false)
	require.NoError(t, err, "GetMachines includeAll")
	require.Len(t, all, 2, "includeAll machines")

	filtered, err := d.GetMachines(context.Background(), true, false)
	require.NoError(t, err, "GetMachines excludeOneShot")
	require.Len(t, filtered, 1, "excludeOneShot machines")
	assert.Equal(t, "desktop", filtered[0], "excludeOneShot machine")
}

func TestGetStatsExcludeOneShot(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj1", func(s *Session) {
		s.MessageCount = 5
		s.UserMessageCount = 1
	})
	insertSession(t, d, "s2", "proj2", func(s *Session) {
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	// Include all.
	stats, err := d.GetStats(context.Background(), false, false)
	require.NoError(t, err, "GetStats includeAll")
	assert.Equal(t, 2, stats.SessionCount, "includeAll: session_count")
	assert.Equal(t, 15, stats.MessageCount, "includeAll: message_count")
	assert.Equal(t, 2, stats.ProjectCount, "includeAll: project_count")

	// Exclude one-shot.
	stats, err = d.GetStats(context.Background(), true, false)
	require.NoError(t, err, "GetStats excludeOneShot")
	assert.Equal(t, 1, stats.SessionCount, "excludeOneShot: session_count")
	assert.Equal(t, 10, stats.MessageCount, "excludeOneShot: message_count")
	assert.Equal(t, 1, stats.ProjectCount, "excludeOneShot: project_count")
}

func TestSessionFilterExcludeAutomated(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "normal", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code changes shown below.\n\n## Changes"
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "fix", "proj", func(s *Session) {
		fm := "# Fix Request\nAn analysis was performed"
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "multi", "proj", func(s *Session) {
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	tests := []struct {
		name             string
		excludeAutomated bool
		want             []string
	}{
		{
			"IncludeAll",
			false,
			[]string{"normal", "review", "fix", "multi"},
		},
		{
			"ExcludeAutomated",
			true,
			[]string{"normal", "multi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				ExcludeAutomated: tt.excludeAutomated,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestExcludeOneShotWithIncludeAutomated(t *testing.T) {
	d := testDB(t)

	// Normal multi-turn session.
	insertSession(t, d, "multi", "proj", func(s *Session) {
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	// Normal single-turn session.
	insertSession(t, d, "oneshot", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Automated single-turn session.
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	tests := []struct {
		name             string
		excludeOneShot   bool
		excludeAutomated bool
		want             []string
	}{
		{
			"BothOff",
			false, false,
			[]string{"multi", "oneshot", "review"},
		},
		{
			"ExcludeOneShotOnly",
			true, false,
			// Automated sessions survive one-shot exclusion.
			[]string{"multi", "review"},
		},
		{
			"ExcludeBoth",
			true, true,
			[]string{"multi"},
		},
		{
			"ExcludeAutomatedOnly",
			false, true,
			[]string{"multi", "oneshot"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := SessionFilter{
				ExcludeOneShot:   tt.excludeOneShot,
				ExcludeAutomated: tt.excludeAutomated,
			}
			requireSessions(t, d, f, tt.want)
		})
	}
}

func TestSidebarSessionIndexExcludeAutomated(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "normal", "proj", func(s *Session) {
		s.MessageCount = 5
		s.UserMessageCount = 2
	})
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{
		ExcludeAutomated: true,
	})
	requireNoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(t, index.Sessions, []string{"normal"})
	require.Equal(t, 1, index.Total, "total")
}

func TestSidebarSessionIndexIncludeAutomated(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "normal", "proj", func(s *Session) {
		s.MessageCount = 5
		s.UserMessageCount = 2
	})
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{
		ExcludeAutomated: false,
	})
	requireNoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(t, index.Sessions, []string{"normal", "review"})
	require.Equal(t, 2, index.Total, "total")
}

func TestSidebarSessionIndexExcludeOneShotWithAutomatedIncluded(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "multi", "proj", func(s *Session) {
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	insertSession(t, d, "oneshot", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{
		ExcludeOneShot:   true,
		ExcludeAutomated: false,
	})
	requireNoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(t, index.Sessions, []string{"multi", "review"})
}

func TestSidebarSessionIndexIncludesChildrenForMatchingRoot(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "root", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	insertSession(t, d, "sub", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("root")
		s.RelationshipType = "subagent"
	})
	insertSession(t, d, "fork", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = new("sub")
		s.RelationshipType = "fork"
	})
	insertSession(t, d, "other", "proj", func(s *Session) {
		s.Agent = "codex"
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{
		Agent: "claude",
	})
	requireNoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(t, index.Sessions, []string{"root", "sub", "fork"})
}

func TestSidebarSessionIndexReturnsDisplayName(t *testing.T) {
	d := testDB(t)

	sessionName := "Named session"
	insertSession(t, d, "named", "proj", func(s *Session) {
		s.SessionName = &sessionName
		s.MessageCount = 3
		s.UserMessageCount = 2
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{})
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, index.Sessions, 1)
	require.NotNil(t, index.Sessions[0].DisplayName, "display_name via COALESCE")
	assert.Equal(t, sessionName, *index.Sessions[0].DisplayName, "display_name via COALESCE")
}

func TestSidebarIndexDisplayNameResolvesViaCoalesce(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session has an agent-provided name but no user rename.
	insertSession(t, d, "s1", "p", func(s *Session) {
		name := "Agent Name"
		s.SessionName = &name
	})

	index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{})
	requireNoError(t, err, "sidebar index")
	require.Len(t, index.Sessions, 1)
	// display_name is resolved via COALESCE(display_name, session_name).
	require.NotNil(t, index.Sessions[0].DisplayName, "display_name must be present via COALESCE")
	assert.Equal(t, "Agent Name", *index.Sessions[0].DisplayName, "COALESCE returns session_name")

	// User renames override session_name.
	requireNoError(t, d.RenameSession("s1", Ptr("My Name")), "rename")
	index, err = d.GetSidebarSessionIndex(ctx, SessionFilter{})
	requireNoError(t, err, "sidebar index after rename")
	require.Len(t, index.Sessions, 1)
	require.NotNil(t, index.Sessions[0].DisplayName, "display_name must be present after rename")
	assert.Equal(t, "My Name", *index.Sessions[0].DisplayName, "display_name wins over session_name")
}

func TestSidebarSessionIndexComputesIsTeammate(t *testing.T) {
	d := testDB(t)

	teammateFirstMessage := "<teammate-message from=\"reviewer\">hi"
	insertSession(t, d, "teammate", "proj", func(s *Session) {
		s.FirstMessage = &teammateFirstMessage
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	insertSession(t, d, "normal", "proj", func(s *Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})

	index, err := d.GetSidebarSessionIndex(context.Background(), SessionFilter{})
	requireNoError(t, err, "GetSidebarSessionIndex")

	rows := sidebarIndexByID(index.Sessions)
	require.True(t, rows["teammate"].IsTeammate,
		"teammate IsTeammate = false, want true")
	require.False(t, rows["normal"].IsTeammate,
		"normal IsTeammate = true, want false")
}

func requireSidebarIndexIDs(
	t *testing.T, sessions []SidebarSessionIndexRow, wantIDs []string,
) {
	t.Helper()
	gotIDs := make([]string, len(sessions))
	for i, s := range sessions {
		gotIDs[i] = s.ID
	}
	wantSorted := make([]string, len(wantIDs))
	copy(wantSorted, wantIDs)
	slices.Sort(wantSorted)

	gotSorted := make([]string, len(gotIDs))
	copy(gotSorted, gotIDs)
	slices.Sort(gotSorted)

	diff := cmp.Diff(wantSorted, gotSorted)
	assert.Empty(t, diff, "sidebar index sessions mismatch (-want +got)")
}

func sidebarIndexByID(
	sessions []SidebarSessionIndexRow,
) map[string]SidebarSessionIndexRow {
	rows := make(map[string]SidebarSessionIndexRow, len(sessions))
	for _, s := range sessions {
		rows[s.ID] = s
	}
	return rows
}

func TestIsAutomatedSetOnUpsert(t *testing.T) {
	d := testDB(t)

	// Normal session.
	insertSession(t, d, "normal", "proj", func(s *Session) {
		fm := "fix the login bug"
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Single-turn automated review session.
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Multi-turn session with review prompt — NOT automated.
	insertSession(t, d, "multi-review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	// Single-turn with roborev substring marker.
	insertSession(t, d, "roborev-sub", "proj", func(s *Session) {
		fm := "IMPORTANT: You are being invoked by roborev to perform this review directly."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	ctx := context.Background()
	normal, err := d.GetSession(ctx, "normal")
	require.NoError(t, err, "get normal")
	assert.False(t, normal.IsAutomated,
		"normal session should not be automated")

	review, err := d.GetSession(ctx, "review")
	require.NoError(t, err, "get review")
	assert.True(t, review.IsAutomated,
		"single-turn review should be automated")

	multi, err := d.GetSession(ctx, "multi-review")
	require.NoError(t, err, "get multi-review")
	assert.False(t, multi.IsAutomated,
		"multi-turn review should not be automated")

	sub, err := d.GetSession(ctx, "roborev-sub")
	require.NoError(t, err, "get roborev-sub")
	assert.True(t, sub.IsAutomated,
		"single-turn roborev substring should be automated")
}

func TestListSessionsHasSecret(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "leaky", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	// secret_leak_count is owned solely by the findings path; UpsertSession
	// (used by insertSession) does NOT persist it, so set it via the findings
	// API rather than the Session mutator.
	require.NoError(t, d.ReplaceSessionSecretFindings("leaky", nil, 3, "v"),
		"ReplaceSessionSecretFindings")
	insertSession(t, d, "clean", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	page, err := d.ListSessions(context.Background(), SessionFilter{HasSecret: true})
	require.NoError(t, err, "ListSessions")
	require.Len(t, page.Sessions, 1, "HasSecret filter")
	require.Equal(t, "leaky", page.Sessions[0].ID, "HasSecret filter")

	insertSession(t, d, "stale", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	require.NoError(t, d.ReplaceSessionSecretFindings("stale", nil, 2, "old-rules"),
		"ReplaceSessionSecretFindings stale")
	current, err := d.ListSessions(context.Background(), SessionFilter{
		HasSecret:            true,
		SecretsRulesVersions: []string{"v"},
	})
	require.NoError(t, err, "ListSessions current rules")
	require.Len(t, current.Sessions, 1, "versioned HasSecret filter")
	require.Equal(t, "leaky", current.Sessions[0].ID, "versioned HasSecret filter")
}

func TestIncrementalUpdateClearsAutomated(t *testing.T) {
	d := testDB(t)

	// Start as single-turn automated session.
	fm := "You are a code reviewer. Review the code."
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	ctx := context.Background()
	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err, "get before")
	require.True(t, s.IsAutomated, "should start as automated")

	// Simulate a second user turn via incremental update.
	err = d.UpdateSessionIncremental(
		"s1", nil, 6, 2, 100, 12345, 0, 0, false, false,
	)
	require.NoError(t, err, "incremental update")

	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err, "get after")
	assert.False(t, s.IsAutomated,
		"should no longer be automated after second user turn")
}
