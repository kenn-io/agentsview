package db

import (
	"context"
	"testing"
)

func TestPruneFilterZeroValue(t *testing.T) {
	f := PruneFilter{}

	if f.HasFilters() {
		t.Error("HasFilters() returned true for zero value")
	}

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
		s.StartedAt = Ptr("2024-01-01T10:00:00Z")
		s.EndedAt = Ptr("2024-01-01T11:00:00Z")
		s.MessageCount = 5
	})

	// Session that started long ago but ended recently.
	insertSession(t, d, "recent-end", "proj", func(s *Session) {
		s.StartedAt = Ptr("2024-01-01T10:00:00Z")
		s.EndedAt = Ptr("2024-06-03T10:00:00Z")
		s.MessageCount = 5
	})

	// Session that started recently, no ended_at.
	insertSession(t, d, "recent-start", "proj", func(s *Session) {
		s.StartedAt = Ptr("2024-06-03T08:00:00Z")
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
		s.ParentSessionID = Ptr("normal")
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
		s.StartedAt = Ptr("2024-06-01T10:00:00Z")
		s.EndedAt = Ptr("2024-06-01T11:00:00Z")
		s.MessageCount = 10
		s.UserMessageCount = 5
	})

	// Subagent child: different agent, different date, 1 message.
	insertSession(t, d, "child-sub", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2024-07-15T10:00:00Z")
		s.EndedAt = Ptr("2024-07-15T11:00:00Z")
		s.MessageCount = 1
		s.UserMessageCount = 1
		s.ParentSessionID = Ptr("parent")
		s.RelationshipType = "subagent"
	})

	// Fork child: same agent but fewer messages than filter.
	insertSession(t, d, "child-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2024-06-02T10:00:00Z")
		s.EndedAt = Ptr("2024-06-02T11:00:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = Ptr("parent")
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
		s.ParentSessionID = Ptr("parentA")
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
		s.ParentSessionID = Ptr("parentB")
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
		s.ParentSessionID = Ptr("parentC")
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
			// childA (agent=codex) matches the filter directly
			// even though its parent (parentA, agent=claude)
			// does not. childB is included because its parent
			// (parentB, agent=codex) matches.
			name: "ChildMatchesDirectlyOrViaParent",
			filter: SessionFilter{
				IncludeChildren: true,
				Agent:           "codex",
			},
			want: []string{"childA", "parentB", "childB"},
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
		s.ParentSessionID = Ptr("root")
		s.RelationshipType = "subagent"
	})
	// One-shot fork (claude) — should be included via parent.
	insertSession(t, d, "fork-1msg", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.ParentSessionID = Ptr("root")
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
		s.StartedAt = Ptr("2024-01-15T10:00:00Z")
		s.EndedAt = Ptr("2024-06-15T10:00:00Z")
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

	all, err := d.GetMachines(context.Background(), false)
	requireNoError(t, err, "GetMachines includeAll")
	if len(all) != 2 {
		t.Fatalf("includeAll: got %d machines, want 2", len(all))
	}

	filtered, err := d.GetMachines(context.Background(), true)
	requireNoError(t, err, "GetMachines excludeOneShot")
	if len(filtered) != 1 {
		t.Fatalf("excludeOneShot: got %d machines, want 1",
			len(filtered))
	}
	if filtered[0] != "desktop" {
		t.Errorf("excludeOneShot: got %q, want desktop",
			filtered[0])
	}
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
	stats, err := d.GetStats(context.Background(), false)
	requireNoError(t, err, "GetStats includeAll")
	if stats.SessionCount != 2 {
		t.Errorf("includeAll: session_count = %d, want 2",
			stats.SessionCount)
	}
	if stats.MessageCount != 15 {
		t.Errorf("includeAll: message_count = %d, want 15",
			stats.MessageCount)
	}
	if stats.ProjectCount != 2 {
		t.Errorf("includeAll: project_count = %d, want 2",
			stats.ProjectCount)
	}

	// Exclude one-shot.
	stats, err = d.GetStats(context.Background(), true)
	requireNoError(t, err, "GetStats excludeOneShot")
	if stats.SessionCount != 1 {
		t.Errorf("excludeOneShot: session_count = %d, want 1",
			stats.SessionCount)
	}
	if stats.MessageCount != 10 {
		t.Errorf("excludeOneShot: message_count = %d, want 10",
			stats.MessageCount)
	}
	if stats.ProjectCount != 1 {
		t.Errorf("excludeOneShot: project_count = %d, want 1",
			stats.ProjectCount)
	}
}
