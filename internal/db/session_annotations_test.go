package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPRLinksRoundTripAndFilter(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "with-prs", "proj", func(s *Session) {
		s.PRLinks = []PRLink{
			{
				URL: "https://github.com/Owner/Repo/pull/12", Host: "github.com",
				Repository: "Owner/Repo", Number: 12,
			},
			{
				URL: "https://github.com/owner/other/pull/3", Host: "github.com",
				Repository: "owner/other", Number: 3,
			},
		}
	})
	insertSession(t, d, "without-prs", "proj")
	insertSession(t, d, "bitbucket", "proj", func(s *Session) {
		s.PRLinks = []PRLink{{
			URL: "https://bitbucket.org/team/repo/pull-requests/5", Host: "bitbucket.org",
			Repository: "team/repo", Number: 5,
		}}
	})
	insertSession(t, d, "gerrit", "proj", func(s *Session) {
		s.PRLinks = []PRLink{{
			URL:        "https://example-review.googlesource.com/c/example/repo/+/7",
			Host:       "example-review.googlesource.com",
			Repository: "example/repo", Number: 7,
		}}
	})

	got, err := d.GetSession(t.Context(), "with-prs")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.PRLinks, 2)
	assert.Equal(t, "Owner/Repo", got.PRLinks[0].Repository)

	full, err := d.GetSessionFull(t.Context(), "with-prs")
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, got.PRLinks, full.PRLinks)

	tests := []struct {
		filter string
		want   []string
	}{
		{filter: "owner/repo", want: []string{"with-prs"}},
		{filter: "owner/repo#12", want: []string{"with-prs"}},
		{filter: "owner/repo#13", want: []string{}},

		{filter: "owner/missing", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			pr, err := ParsePRFilter(tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, listSortedIDs(t, d, SessionFilter{PR: pr}))
		})
	}

	// A later parse without links clears them: the column is parser-owned.
	insertSession(t, d, "with-prs", "proj")
	got, err = d.GetSession(t.Context(), "with-prs")
	require.NoError(t, err)
	assert.Empty(t, got.PRLinks)
}

func TestParsePRFilterRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{
		"repo-without-owner", "owner/repo#", "owner/repo#zero",
		"https://github.com/owner/repo",
	} {
		_, err := ParsePRFilter(value)
		assert.Error(t, err, value)
	}
}

func TestSessionLabelsLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Labels may be recorded before sync imports the session.
	pending, err := d.SetSessionLabels(ctx, "worker", []string{
		" role=reviewer ", "ticket=ABC-123", "role=reviewer",
	})
	require.NoError(t, err)
	assert.False(t, pending.SessionFound)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, pending.Labels)

	insertSession(t, d, "worker", "proj")
	insertSession(t, d, "other", "proj")
	got, err := d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, got.Labels)

	before, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	updated, err := d.UpdateSessionLabels(ctx, "worker",
		[]string{"nightly"}, []string{"role=reviewer", "absent"})
	require.NoError(t, err)
	assert.True(t, updated.SessionFound)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, updated.Labels)
	after, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, after.LocalModifiedAt,
		"label changes must mark the session modified for mirror pushes")
	assert.NotEqual(t, before.LocalModifiedAt, after.LocalModifiedAt)

	// Parser writes never touch labels.
	insertSession(t, d, "worker", "proj")
	stored, err := d.GetSessionLabels(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, stored.Labels)

	assert.Equal(t, []string{"worker"},
		listSortedIDs(t, d, SessionFilter{Labels: []string{"nightly", "ticket=ABC-123"}}))
	assert.Equal(t, []string{},
		listSortedIDs(t, d, SessionFilter{Labels: []string{"nightly", "role=reviewer"}}))

	cleared, err := d.SetSessionLabels(ctx, "worker", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{}, cleared.Labels)
	got, err = d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Empty(t, got.Labels)
}

func TestSessionLabelsRejectInvalidInput(t *testing.T) {
	d := testDB(t)
	tooMany := make([]string, MaxSessionLabels+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("label-%d", i)
	}
	for name, labels := range map[string][]string{
		"empty":       {"  "},
		"empty key":   {"=value"},
		"control":     {"bad\nlabel"},
		"too long":    {strings.Repeat("a", MaxSessionLabelBytes+1)},
		"too many":    tooMany,
		"invalid utf": {"\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.SetSessionLabels(t.Context(), "s", labels)
			require.ErrorIs(t, err, ErrSessionLabelsInvalid)
			_, err = d.UpdateSessionLabels(t.Context(), "s", labels, nil)
			assert.ErrorIs(t, err, ErrSessionLabelsInvalid)
		})
	}
}

func TestUpsertSessionAppliesLauncherParent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")
	_, err := d.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	for range 2 {
		insertSession(t, d, "worker", "proj")
		worker, err := d.GetSession(ctx, "worker")
		require.NoError(t, err)
		require.NotNil(t, worker.ParentSessionID)
		assert.Equal(t, "manager", *worker.ParentSessionID)
		assert.Equal(t, "subagent", worker.RelationshipType)
	}
}

func TestScopedSpawnLinkRecomputesMovedChildLauncherCycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"q", "p", "b", "a"} {
		insertSession(t, d, id, "proj")
	}
	spawn := func(id string) {
		t.Helper()
		require.NoError(t, d.InsertMessages(ctx, []Message{{
			SessionID: id, Ordinal: 0, Role: "assistant", Content: "spawn", HasToolUse: true,
			ToolCalls: []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "p"}},
		}}))
	}
	spawn("q")
	_, err := d.LinkSubagentSessionsForSessions(ctx, []string{"q"})
	require.NoError(t, err)
	_, err = d.SetSessionExternalParent(ctx, "b", "p")
	require.NoError(t, err)
	_, err = d.SetSessionExternalParent(ctx, "a", "b")
	require.NoError(t, err)
	spawn("a")
	_, err = d.LinkSubagentSessionsForSessions(ctx, []string{"q"})
	require.NoError(t, err)
	for _, id := range []string{"a", "b"} {
		s, err := d.GetSession(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, s.ParentSessionID, id)
	}
	assert.ElementsMatch(t, []string{"q", "a", "b"}, listSortedIDs(t, d, SessionFilter{}))
}

func TestSessionExternalParentApplication(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")

	// A launcher can link a worker before sync imports it.
	link, err := d.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	assert.False(t, link.SessionFound)
	assert.False(t, link.Applied)
	assert.Equal(t, ExternalRelationshipType, link.RelationshipType)

	// Uploads link sessions inside the batch transaction.
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
		Session: Session{ID: "worker", Project: "proj", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1},
	}})
	require.NoError(t, err)
	worker, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	assert.Equal(t, "subagent", worker.RelationshipType)

	children, err := d.GetChildSessions(ctx, "manager")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "worker", children[0].ID)

	// Sync linking restores the launcher parent after a parser rewrite.
	insertSession(t, d, "worker", "proj")
	_, err = d.LinkSubagentSessionsForSessions(ctx, []string{"worker"})
	require.NoError(t, err)
	worker, err = d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	assert.Equal(t, "subagent", worker.RelationshipType)

	// Batch writes also replace usage events, which stamp local_modified_at.
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
		Session: Session{ID: "worker", Project: "proj", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1},
	}})
	require.NoError(t, err)
	worker, err = d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	assert.Equal(t, "subagent", worker.RelationshipType)

	link, err = d.GetSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.True(t, link.Applied)

	cleared, err := d.ClearSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.False(t, cleared.Applied)
	worker, err = d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	assert.Nil(t, worker.ParentSessionID)
	assert.Empty(t, worker.RelationshipType)
	_, err = d.ClearSessionExternalParent(ctx, "worker")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestSessionExternalParentYieldsToParserParent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")
	insertSession(t, d, "real-parent", "proj")
	parsedParent := "real-parent"
	insertSession(t, d, "child", "proj", func(s *Session) {
		s.ParentSessionID = &parsedParent
		s.RelationshipType = "continuation"
	})

	link, err := d.SetSessionExternalParent(ctx, "child", "manager")
	require.NoError(t, err)
	assert.True(t, link.SessionFound)
	assert.False(t, link.Applied)

	child, err := d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "real-parent", *child.ParentSessionID)
	assert.Equal(t, "continuation", child.RelationshipType)
}

func TestSessionExternalParentSkipsLinkThatClosesCycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.SetSessionExternalParent(ctx, "a", "b")
	require.NoError(t, err)
	insertSession(t, d, "a", "proj")
	b := Session{
		ID: "b", Project: "proj", Machine: defaultMachine, Agent: defaultAgent,
		MessageCount: 1, ParentSessionID: new("a"), RelationshipType: "continuation",
	}
	_, err = d.getWriter().ExecContext(ctx, upsertSessionBaseSQL, upsertSessionArgs(b)...)
	require.NoError(t, err)

	assertParents := func(t *testing.T) {
		t.Helper()
		a, err := d.GetSession(ctx, "a")
		require.NoError(t, err)
		assert.Nil(t, a.ParentSessionID, "the launcher link must yield to the native parent")
		b, err := d.GetSession(ctx, "b")
		require.NoError(t, err)
		require.NotNil(t, b.ParentSessionID)
		assert.Equal(t, "a", *b.ParentSessionID)
		link, err := d.GetSessionExternalParent(ctx, "a")
		require.NoError(t, err)
		assert.False(t, link.Applied)
		assert.Contains(t, listSortedIDs(t, d, SessionFilter{}), "a")
	}
	require.NoError(t, d.LinkSubagentSessions())
	assertParents(t)

	// A reparse of a must not re-apply the link.
	insertSession(t, d, "a", "proj")
	require.NoError(t, d.LinkSubagentSessions())
	assertParents(t)

	// Scoped linking restores the launcher parent after the native cycle disappears.
	insertSession(t, d, "b", "proj")
	linked, err := d.LinkSubagentSessionsForSessions(ctx, []string{"b"})
	require.NoError(t, err)
	assert.Equal(t, 0, linked)
	a, err := d.GetSession(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, a.ParentSessionID)
	assert.Equal(t, "b", *a.ParentSessionID)
	children, err := d.GetChildSessions(ctx, "b")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "a", children[0].ID)
}

func TestSessionExternalParentRejectsInvalidLinks(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "proj")
	insertSession(t, d, "b", "proj")
	_, err := d.SetSessionExternalParent(ctx, "b", "a")
	require.NoError(t, err)
	_, err = d.SetSessionExternalParent(ctx, "pending-c", "b")
	require.NoError(t, err)

	tests := []struct {
		name, session, parent string
	}{
		{name: "self", session: "a", parent: "a"},
		{name: "cycle through effective parent", session: "a", parent: "b"},
		{name: "cycle through pending link", session: "a", parent: "pending-c"},
		{name: "missing parent", session: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.SetSessionExternalParent(ctx, tt.session, tt.parent)
			assert.ErrorIs(t, err, ErrSessionExternalParentInvalid)
		})
	}
}

func TestCopySessionMetadataFromPreservesLabelsAndExternalParents(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(ctx, sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	insertSession(t, source, "manager", "proj")
	insertSession(t, source, "worker", "proj")
	_, err = source.SetSessionLabels(ctx, "worker", []string{"role=reviewer"})
	require.NoError(t, err)
	_, err = source.SetSessionLabels(ctx, "not-synced-yet", []string{"queued"})
	require.NoError(t, err)
	_, err = source.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)

	destinationPath := filepath.Join(dir, "destination.db")
	destination, err := Open(ctx, destinationPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = destination.Close() })
	insertSession(t, destination, "manager", "proj")
	insertSession(t, destination, "worker", "proj")

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	worker, err := destination.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"role=reviewer"}, worker.Labels)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, "manager", *worker.ParentSessionID)
	pending, err := destination.GetSessionLabels(ctx, "not-synced-yet")
	require.NoError(t, err)
	assert.Equal(t, []string{"queued"}, pending.Labels)
}

func TestAnnotationFiltersFindLaunchedWorkers(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj", func(s *Session) {
		s.UserMessageCount = 3
	})
	insertSession(t, d, "idle", "proj", func(s *Session) {
		s.UserMessageCount = 3
	})
	// A launcher-run worker is a headless single-prompt child session,
	// which the default list and sidebar exclusions would hide.
	insertSession(t, d, "worker", "proj", func(s *Session) {
		s.UserMessageCount = 1
		s.IsAutomated = true
		s.PRLinks = []PRLink{{
			URL: "https://github.com/acme/widgets/pull/42", Host: "github.com",
			Repository: "acme/widgets", Number: 42,
		}}
	})
	_, err := d.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	_, err = d.SetSessionLabels(ctx, "worker", []string{"ticket=ABC-123"})
	require.NoError(t, err)

	pr, err := ParsePRFilter("acme/widgets#42")
	require.NoError(t, err)
	defaults := SessionFilter{ExcludeOneShot: true, ExcludeAutomated: true}
	tests := []struct {
		name   string
		filter func(SessionFilter) SessionFilter
	}{
		{name: "label", filter: func(f SessionFilter) SessionFilter {
			f.Labels = []string{"ticket=ABC-123"}
			return f
		}},
		{name: "pr", filter: func(f SessionFilter) SessionFilter {
			f.PR = pr
			return f
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The flat list selects the matching worker itself.
			assert.Equal(t, []string{"worker"},
				listSortedIDs(t, d, tt.filter(defaults)))

			// The sidebar keeps the launcher's tree and drops unrelated roots.
			for _, limit := range []int{0, 10} {
				f := tt.filter(defaults)
				f.Limit = limit
				index, err := d.GetSidebarSessionIndex(ctx, f)
				require.NoError(t, err)
				ids := []string{}
				for _, row := range index.Sessions {
					ids = append(ids, row.ID)
				}
				assert.ElementsMatch(t, []string{"manager", "worker"}, ids,
					"limit %d", limit)
				assert.Equal(t, 1, index.Total, "limit %d", limit)
			}
		})
	}

	// Without an annotation filter the defaults still hide the worker.
	assert.ElementsMatch(t, []string{"manager", "idle"}, listSortedIDs(t, d, defaults))
}

func TestSessionExternalParentYieldsToSpawnEdge(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "manager", "proj")
	insertSession(t, d, "child", "proj")
	link, err := d.SetSessionExternalParent(ctx, "child", "manager")
	require.NoError(t, err)
	assert.True(t, link.Applied)
	write := SessionBatchWrite{
		Session:         Session{ID: "spawner", Project: "proj", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1},
		ReplaceMessages: true,
		Messages: []Message{{
			SessionID: "spawner", Ordinal: 0, Role: "assistant",
			Content: "spawn child", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", SubagentSessionID: "child",
			}},
		}},
	}
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
	require.NoError(t, err)
	child, err := d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "spawner", *child.ParentSessionID)
	link, err = d.GetSessionExternalParent(ctx, "child")
	require.NoError(t, err)
	assert.False(t, link.Applied)
	require.NoError(t, d.LinkSubagentSessions())

	// A launcher naming the spawner itself must not take ownership of the
	// spawn-derived parent.
	link, err = d.SetSessionExternalParent(ctx, "child", "spawner")
	require.NoError(t, err)
	assert.False(t, link.Applied)
	_, err = d.ClearSessionExternalParent(ctx, "child")
	require.NoError(t, err)
	child, err = d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "spawner", *child.ParentSessionID,
		"clearing a launcher link must keep the spawn-derived parent")

	link, err = d.SetSessionExternalParent(ctx, "child", "manager")
	require.NoError(t, err)
	assert.False(t, link.Applied)
	// A parser rewrite clears the column; the spawn edge, not the launcher
	// link, decides the parent afterwards.
	insertSession(t, d, "child", "proj")
	require.NoError(t, d.LinkSubagentSessions())
	child, err = d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "spawner", *child.ParentSessionID)

	// Removing the spawn edge restores the saved launcher parent during upload.
	write.Messages[0].HasToolUse = false
	write.Messages[0].ToolCalls = nil
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
	require.NoError(t, err)
	child, err = d.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "manager", *child.ParentSessionID)
	assert.Equal(t, "subagent", child.RelationshipType)
	link, err = d.GetSessionExternalParent(ctx, "child")
	require.NoError(t, err)
	assert.True(t, link.Applied)
}

func TestAnnotationTreeSkipsHiddenAncestors(t *testing.T) {
	tests := []struct {
		name   string
		mid    func(*Session)
		trash  bool
		filter SessionFilter
	}{
		{name: "trashed middle", trash: true},
		{name: "empty middle", mid: func(s *Session) { s.MessageCount = 0 }},
		{
			name:   "middle outside the automation scope",
			mid:    func(s *Session) { s.IsAutomated = true },
			filter: SessionFilter{AutomatedScope: "human"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "root", "proj")
			insertSession(t, d, "mid", "proj", func(s *Session) {
				s.ParentSessionID = new("root")
				s.RelationshipType = "subagent"
				if tt.mid != nil {
					tt.mid(s)
				}
			})
			insertSession(t, d, "leaf", "proj", func(s *Session) {
				s.ParentSessionID = new("mid")
				s.RelationshipType = "subagent"
			})
			if tt.trash {
				require.NoError(t, d.SoftDeleteSession(ctx, "mid"))
			}
			_, err := d.SetSessionLabels(ctx, "leaf", []string{"x"})
			require.NoError(t, err)

			// The tree views never reach leaf, so the label keeps no root.
			for _, limit := range []int{0, 10} {
				f := tt.filter
				f.Labels = []string{"x"}
				f.Limit = limit
				index, err := d.GetSidebarSessionIndex(ctx, f)
				require.NoError(t, err)
				assert.Empty(t, index.Sessions, "limit %d", limit)
				assert.Zero(t, index.Total, "limit %d", limit)
			}
		})
	}
}
