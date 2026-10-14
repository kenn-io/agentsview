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
	insertSession(t, d, "other-host", "proj", func(s *Session) {
		s.PRLinks = []PRLink{{URL: "https://gitlab.com/owner/repo/-/merge_requests/12", Host: "gitlab.com", Repository: "owner/repo", Number: 12}}
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
		{filter: "owner/repo", want: []string{"other-host", "with-prs"}},
		{filter: "owner/repo#12", want: []string{"other-host", "with-prs"}},
		{filter: "#12", want: []string{"other-host", "with-prs"}},
		{filter: "#3", want: []string{"with-prs"}},
		{filter: "GITHUB.COM::OWNER/REPO#12", want: []string{"with-prs"}},
		{filter: "github.com::owner/repo", want: []string{"with-prs"}},
		{filter: "gitlab.com::owner/repo#12", want: []string{"other-host"}},
		{filter: "forge.example::owner/repo#12", want: []string{}},
		{filter: "github.com::owner/repo#3", want: []string{}},
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

func TestSessionLabelsLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Labels may be recorded before sync imports the session.
	pending, err := d.SetSessionLabels(ctx, "worker", []string{
		" role=reviewer ", "ticket=example", "role=reviewer",
	})
	require.NoError(t, err)
	assert.False(t, pending.SessionFound)
	assert.Equal(t, []string{"role=reviewer", "ticket=example"}, pending.Labels)

	insertSession(t, d, "worker", "proj")
	insertSession(t, d, "other", "proj")
	got, err := d.GetSession(ctx, "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"role=reviewer", "ticket=example"}, got.Labels)

	updated, err := d.UpdateSessionLabels(ctx, "worker",
		[]string{"nightly"}, []string{"role=reviewer", "absent"})
	require.NoError(t, err)
	assert.True(t, updated.SessionFound)
	assert.Equal(t, []string{"nightly", "ticket=example"}, updated.Labels)

	// Parser writes never touch labels.
	insertSession(t, d, "worker", "proj")
	stored, err := querySessionLabels(ctx, d.getReader(), "worker")
	require.NoError(t, err)
	assert.Equal(t, []string{"nightly", "ticket=example"}, stored)

	assert.Equal(t, []string{"worker"},
		listSortedIDs(t, d, SessionFilter{Labels: []string{"nightly", "ticket=example"}}))
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

func TestMalformedStoredPRLinksPreserveSessionReads(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "worker", "proj")
	_, err := d.getWriter().ExecContext(t.Context(), "UPDATE sessions SET pr_links = '{' WHERE id = 'worker'")
	require.NoError(t, err)
	assert.Equal(t, []string{"worker"}, listSortedIDs(t, d, SessionFilter{}))
	worker, err := d.GetSession(t.Context(), "worker")
	require.NoError(t, err)
	require.NotNil(t, worker)
	assert.Empty(t, worker.PRLinks)
	links, err := d.GetSessionPRLinkURLs(t.Context(), "worker")
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestPermanentDeletionRemovesOnlyStoredSessionAnnotations(t *testing.T) {
	for _, action := range []string{"single", "batch", "trash", "empty trash", "excluded"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "worker", "proj")
			insertSession(t, d, "manager", "proj")
			for _, id := range []string{"worker", "pending"} {
				_, err := d.SetSessionLabels(ctx, id, []string{"nightly"})
				require.NoError(t, err)
				_, err = d.SetSessionExternalParent(ctx, id, "manager")
				require.NoError(t, err)
			}
			switch action {
			case "single":
				require.NoError(t, d.DeleteSession(ctx, "pending"))
				require.NoError(t, d.DeleteSession(ctx, "worker"))
			case "batch":
				_, err := d.DeleteSessions(ctx, []string{"worker", "pending"})
				require.NoError(t, err)
			case "trash", "empty trash":
				require.NoError(t, d.SoftDeleteSession(ctx, "worker"))
				if action == "trash" {
					_, err := d.DeleteSessionIfTrashed(ctx, "worker")
					require.NoError(t, err)
				} else {
					_, err := d.EmptyTrash(ctx)
					require.NoError(t, err)
				}
			case "excluded":
				require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
					if err := excludeSessionIDTx(ctx, tx, "worker", sql.NullString{}); err != nil {
						return err
					}
					_, err := purgeExcludedSessionsTx(ctx, tx)
					return err
				}))
			}
			for _, table := range []string{"session_labels", "session_external_parents"} {
				var ids string
				require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT group_concat(session_id) FROM "+table).Scan(&ids))
				assert.Equal(t, "pending", ids)
			}
		})
	}
}

func TestRemovedLauncherRestoresParserRelationship(t *testing.T) {
	for _, action := range []string{"delete spawner", "delete launcher", "parser exclusion", "clear vanished parent"} {
		for _, relationship := range []string{"", "subagent"} {
			if action == "clear vanished parent" && relationship != "" {
				continue
			}
			t.Run(action+"/"+relationship, func(t *testing.T) {
				d := testDB(t)
				ctx := t.Context()
				insertSession(t, d, "worker", "proj", func(s *Session) { s.RelationshipType = relationship })
				insertSession(t, d, "manager", "proj")
				if action != "delete launcher" && action != "clear vanished parent" {
					insertMessages(t, d, Message{
						SessionID: "manager", Ordinal: 0, Role: "assistant", Content: "spawn", HasToolUse: true,
						ToolCalls: []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "worker"}},
					})
					require.NoError(t, d.LinkSubagentSessions())
				}
				link, err := d.SetSessionExternalParent(ctx, "worker", "manager")
				require.NoError(t, err)
				assert.Equal(t, action == "delete launcher" || action == "clear vanished parent", link.Applied)
				_, err = d.getWriter().ExecContext(ctx, "UPDATE sessions SET sync_marker='2000-01-01T00:00:00.000Z' WHERE id='worker'")
				require.NoError(t, err)
				switch action {
				case "clear vanished parent":
					_, err = d.getWriter().ExecContext(ctx, "DELETE FROM sessions WHERE id='manager'")
					require.NoError(t, err)
					link, err = d.ClearSessionExternalParent(ctx, "worker")
					require.NoError(t, err)
					assert.True(t, link.Changed)
				case "parser exclusion":
					n, err := d.DeleteParserExcludedSessions(ctx, []string{"manager"})
					require.NoError(t, err)
					require.Equal(t, 1, n)
					require.NoError(t, d.QueueSubagentParentCleanupRepairs(ctx, []string{"worker"}))
					require.NoError(t, d.RepairQueuedSubagentParents())
				default:
					require.NoError(t, d.DeleteSession(ctx, "manager"))
				}
				worker, err := d.GetSession(ctx, "worker")
				require.NoError(t, err)
				require.NotNil(t, worker)
				assert.Nil(t, worker.ParentSessionID)
				assert.Equal(t, relationship, worker.RelationshipType)
				var marker string
				require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT sync_marker FROM sessions WHERE id='worker'").Scan(&marker))
				assert.Greater(t, marker, "2000-01-01T00:00:00.000Z")
				if action != "clear vanished parent" {
					link, err = d.GetSessionExternalParent(ctx, "worker")
					require.NoError(t, err)
					assert.Equal(t, "manager", link.ParentSessionID)
					assert.False(t, link.Applied)
				}
			})
		}
	}
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

	for range 2 {
		insertSession(t, d, "worker", "proj")
		worker, err := d.GetSessionFull(ctx, "worker")
		require.NoError(t, err)
		require.NotNil(t, worker.ParentSessionID)
		assert.Equal(t, "manager", *worker.ParentSessionID)
		assert.Equal(t, "subagent", worker.RelationshipType)
	}

	children, err := d.GetChildSessions(ctx, "manager")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "worker", children[0].ID)

	link, err = d.GetSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.True(t, link.Applied)

	cleared, err := d.ClearSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	assert.False(t, cleared.Applied)
	worker, err := d.GetSessionFull(ctx, "worker")
	require.NoError(t, err)
	assert.Nil(t, worker.ParentSessionID)
	assert.Empty(t, worker.RelationshipType)
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

func TestSessionExternalParentClearAbsent(t *testing.T) {
	d := testDB(t)
	parent := "native-parent"
	insertSession(t, d, "native", "proj", func(s *Session) {
		s.ParentSessionID = &parent
		s.RelationshipType = "continuation"
	})
	for _, id := range []string{"pending", "native"} {
		for range 2 {
			link, err := d.ClearSessionExternalParent(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, id, link.SessionID)
			assert.Equal(t, id == "native", link.SessionFound)
			assert.False(t, link.Changed)
		}
	}
	native, err := d.GetSession(t.Context(), "native")
	require.NoError(t, err)
	assert.Equal(t, &parent, native.ParentSessionID)
	assert.Equal(t, "continuation", native.RelationshipType)
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
	pending, err := querySessionLabels(ctx, destination.getReader(), "not-synced-yet")
	require.NoError(t, err)
	assert.Equal(t, []string{"queued"}, pending)
}

func TestRebuildLauncherRelationshipProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, saved, fresh       string
		pending, copied, trashed bool
	}{
		{name: "pending worker first parsed", fresh: "subagent", pending: true},
		{name: "reparsed native worker", fresh: "subagent"},
		{name: "reparsed ordinary worker", saved: "subagent"},
		{name: "orphan", copied: true},
		{name: "native orphan", saved: "subagent", copied: true},
		{name: "trash", copied: true, trashed: true},
		{name: "native trash", saved: "subagent", copied: true, trashed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			source := testDB(t)
			insertSession(t, source, "manager", "proj")
			if !tc.pending {
				insertSession(t, source, "worker", "proj", func(s *Session) { s.RelationshipType = tc.saved })
			}
			_, err := source.SetSessionExternalParent(ctx, "worker", "manager")
			require.NoError(t, err)
			_, err = source.SetSessionExternalParent(ctx, "absent", "manager")
			require.NoError(t, err)
			if tc.trashed {
				_, err = source.getWriter().ExecContext(ctx, "UPDATE sessions SET deleted_at = '2026-10-01T00:00:00Z' WHERE id = 'worker'")
				require.NoError(t, err)
			}
			destination := testDB(t)
			_, err = destination.CopyTrashedDataFrom(source.Path())
			require.NoError(t, err)
			insertSession(t, destination, "manager", "proj")
			if !tc.copied {
				insertSession(t, destination, "worker", "proj", func(s *Session) {
					s.Agent = "grok"
					s.RelationshipType = tc.fresh
				})
				if !tc.pending {
					_, err = destination.SetSessionExternalParent(ctx, "worker", "manager")
					require.NoError(t, err)
					insertSession(t, destination, "worker", "proj", func(s *Session) { s.RelationshipType = tc.fresh })
				}
			}
			_, err = destination.CopyOrphanedDataFrom(source.Path())
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, destination.CopySessionMetadataFrom(source.Path()))
			}
			link, err := destination.GetSessionExternalParent(ctx, "worker")
			require.NoError(t, err)
			assert.True(t, link.Applied)
			pending, err := destination.GetSessionExternalParent(ctx, "absent")
			require.NoError(t, err)
			assert.False(t, pending.Applied)
			want := tc.fresh
			if tc.copied {
				want = tc.saved
			}
			var parserRelationship string
			require.NoError(t, destination.getReader().QueryRowContext(ctx, "SELECT parser_relationship_type FROM sessions WHERE id = 'worker'").Scan(&parserRelationship))
			assert.Equal(t, want, parserRelationship)
			if !tc.trashed {
				exported, err := destination.GetArtifactExportSession(ctx, "worker")
				require.NoError(t, err)
				assert.Nil(t, exported.ParentSessionID)
				assert.Equal(t, want, exported.RelationshipType)
			}
			_, err = destination.ClearSessionExternalParent(ctx, "worker")
			require.NoError(t, err)
			var relationship string
			var parent *string
			require.NoError(t, destination.getReader().QueryRowContext(ctx, "SELECT relationship_type, parent_session_id FROM sessions WHERE id = 'worker'").Scan(&relationship, &parent))
			assert.Nil(t, parent)
			assert.Equal(t, want, relationship)
		})
	}
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
	_, err = d.SetSessionLabels(ctx, "worker", []string{"ticket=example"})
	require.NoError(t, err)

	pr, err := ParsePRFilter("acme/widgets#42")
	require.NoError(t, err)
	defaults := SessionFilter{ExcludeOneShot: true, ExcludeAutomated: true}
	tests := []struct {
		name   string
		filter func(SessionFilter) SessionFilter
	}{
		{name: "label", filter: func(f SessionFilter) SessionFilter {
			f.Labels = []string{"ticket=example"}
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
			_, err := d.getWriter().ExecContext(ctx, "UPDATE sessions SET is_automated = 0 WHERE id = 'worker'")
			require.NoError(t, err)
			explicit := tt.filter(defaults)
			explicit.OneShotFilterSet = true
			assert.Empty(t, listSortedIDs(t, d, explicit))
			index, err := d.GetSidebarSessionIndex(ctx, explicit)
			require.NoError(t, err)
			assert.Empty(t, index.Sessions)
			explicit.IDs = []string{"worker"}
			assert.Empty(t, listSortedIDs(t, d, explicit))

			_, err = d.getWriter().ExecContext(ctx, "UPDATE sessions SET is_automated = 1, user_message_count = 3 WHERE id = 'worker'")
			require.NoError(t, err)
			automated := tt.filter(defaults)
			automated.AutomatedFilterSet = true
			assert.Empty(t, listSortedIDs(t, d, automated))
			index, err = d.GetSidebarSessionIndex(ctx, automated)
			require.NoError(t, err)
			assert.Empty(t, index.Sessions)
			automated.IDs = []string{"worker"}
			assert.Empty(t, listSortedIDs(t, d, automated))

			_, err = d.getWriter().ExecContext(ctx, "UPDATE sessions SET user_message_count = 1 WHERE id = 'worker'")
			require.NoError(t, err)

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
	defaults.IDs = []string{"worker"}
	assert.Equal(t, []string{"worker"}, listSortedIDs(t, d, defaults))
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
	write.Session.Project = "replacement"
	_, err = d.getWriter().ExecContext(ctx, `CREATE TRIGGER fail_parent_recompute
		BEFORE UPDATE OF parent_session_id ON sessions
		WHEN NEW.id = 'child' AND NEW.parent_session_id = 'manager'
		BEGIN SELECT RAISE(ABORT, 'parent recompute failed'); END`)
	require.NoError(t, err)
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
	require.ErrorContains(t, err, "parent recompute failed")
	child, err = d.GetSession(ctx, "child")
	require.NoError(t, err)
	assert.Equal(t, new("spawner"), child.ParentSessionID, "failed upload must roll back parent recomputation")
	spawner, err := d.GetSession(ctx, "spawner")
	require.NoError(t, err)
	assert.Equal(t, "proj", spawner.Project, "failed recomputation must roll back the session upsert")
	var edges int
	require.NoError(t, d.getReader().QueryRowContext(ctx, "SELECT count(*) FROM tool_calls WHERE session_id = 'spawner'").Scan(&edges))
	assert.Equal(t, 1, edges, "failed recomputation must roll back the uploaded transcript")
	_, err = d.getWriter().ExecContext(ctx, "DROP TRIGGER fail_parent_recompute")
	require.NoError(t, err)
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

func TestParsePRFilter(t *testing.T) {
	for _, tt := range []struct {
		value   string
		want    PRFilter
		invalid bool
	}{
		{"#12", PRFilter{Number: 12}, false},
		{"#0", PRFilter{}, true},
		{"#", PRFilter{}, true},
		{"group/subgroup/repo#12", PRFilter{Repository: "group/subgroup/repo", Number: 12}, false},
		{"owner.name/repo", PRFilter{Repository: "owner.name/repo"}, false},
		{"my.org/sub/repo#12", PRFilter{Repository: "my.org/sub/repo", Number: 12}, false},
		{" GITLAB.COM::group/subgroup/repo#12 ", PRFilter{Host: "gitlab.com", Repository: "group/subgroup/repo", Number: 12}, false},
		{"forge::owner/repo", PRFilter{Host: "forge", Repository: "owner/repo"}, false},
		{"forge.example:8443::owner/repo", PRFilter{Host: "forge.example:8443", Repository: "owner/repo"}, false},
		{"repo-without-owner", PRFilter{}, true},
		{"owner/repo#", PRFilter{}, true},
		{"owner/repo#zero", PRFilter{}, true},
		{"https://github.com/owner/repo", PRFilter{}, true},
		{"::owner/repo", PRFilter{}, true},
		{"github.com::repo", PRFilter{}, true},
	} {
		got, err := ParsePRFilter(tt.value)
		if tt.invalid {
			require.Error(t, err, tt.value)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tt.want, got)
	}
}

func TestSessionExternalParentWaitsForParentArrival(t *testing.T) {
	for _, beforeWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("before-worker=%t", beforeWorker), func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			if !beforeWorker {
				insertSession(t, d, "worker", "proj")
			}
			link, err := d.SetSessionExternalParent(ctx, "worker", "manager")
			require.NoError(t, err)
			assert.False(t, link.Applied)
			assert.False(t, link.Changed)
			insertSession(t, d, "worker", "proj")
			require.NoError(t, d.LinkSubagentSessions())
			worker, err := d.GetSession(ctx, "worker")
			require.NoError(t, err)
			assert.Nil(t, worker.ParentSessionID)
			assert.Empty(t, worker.RelationshipType)
			assert.Equal(t, []string{"worker"}, listSortedIDs(t, d, SessionFilter{}))
			link, err = d.GetSessionExternalParent(ctx, "worker")
			require.NoError(t, err)
			assert.Equal(t, "manager", link.ParentSessionID)
			assert.False(t, link.Applied)

			if !beforeWorker {
				_, err = d.getWriter().ExecContext(ctx, `CREATE TRIGGER fail_pending_parent
				BEFORE UPDATE OF parent_session_id ON sessions
				WHEN NEW.id = 'worker' AND NEW.parent_session_id = 'manager'
				BEGIN SELECT RAISE(ABORT, 'pending parent recompute failed'); END`)
				require.NoError(t, err)
				err = d.UpsertSession(ctx, Session{ID: "manager", Project: "proj", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1})
				require.ErrorContains(t, err, "pending parent recompute failed")
				manager, err := d.GetSession(ctx, "manager")
				require.NoError(t, err)
				assert.Nil(t, manager, "failed child recomputation must roll back parent arrival")
				worker, err = d.GetSession(ctx, "worker")
				require.NoError(t, err)
				assert.Nil(t, worker.ParentSessionID)
				_, err = d.getWriter().ExecContext(ctx, "DROP TRIGGER fail_pending_parent")
				require.NoError(t, err)
			}

			insertSession(t, d, "manager", "proj")
			worker, err = d.GetSession(ctx, "worker")
			require.NoError(t, err)
			require.NotNil(t, worker.ParentSessionID)
			assert.Equal(t, "manager", *worker.ParentSessionID)
			assert.Equal(t, "subagent", worker.RelationshipType)
			link, err = d.GetSessionExternalParent(ctx, "worker")
			require.NoError(t, err)
			assert.True(t, link.Applied)
			assert.Equal(t, []string{"manager"}, listSortedIDs(t, d, SessionFilter{}))
		})
	}
}

func TestAppliedLauncherParentRestoresParserClassification(t *testing.T) {
	for _, classification := range []struct {
		kind, relationship, want string
		legacy, spawned          bool
	}{
		{relationship: "subagent", want: "subagent"},
		{kind: "non-interactive", want: "subagent"},
		{},
		{spawned: true},
		{relationship: "subagent", want: "subagent", spawned: true},
		{legacy: true, spawned: true},
		{relationship: "subagent", want: "subagent", legacy: true},
	} {
		for _, action := range []string{"clear", "replace with pending", "cycle", "pending only"} {
			if (classification.legacy || classification.spawned) && action != "clear" {
				continue
			}
			if (action == "replace with pending" || action == "pending only") && classification.relationship == "" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s/%s/legacy=%t/spawned=%t", action, classification.kind, classification.relationship, classification.legacy, classification.spawned), func(t *testing.T) {
				d := testDB(t)
				ctx := t.Context()
				insertSession(t, d, "manager", "proj")
				parserSession := func(s *Session) {
					if classification.spawned || classification.legacy {
						s.Agent = "grok"
					}
					s.SessionKind = classification.kind
					s.RelationshipType = classification.relationship
				}
				insertSession(t, d, "worker", "proj", parserSession)
				write := SessionBatchWrite{
					Session:         Session{ID: "spawner", Project: "proj", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1},
					ReplaceMessages: true,
					Messages: []Message{{
						SessionID: "spawner", Ordinal: 0, Role: "assistant", Content: "spawn", HasToolUse: true,
						ToolCalls: []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "worker"}},
					}},
				}
				if classification.spawned {
					_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
					require.NoError(t, err)
				}
				if classification.legacy {
					_, err := d.getWriter().ExecContext(ctx, "UPDATE sessions SET parser_relationship_type=NULL WHERE id='worker'")
					require.NoError(t, err)
				}
				parent := "manager"
				if action == "pending only" {
					parent = "missing-a"
				}
				link, err := d.SetSessionExternalParent(ctx, "worker", parent)
				require.NoError(t, err)
				assert.Equal(t, !classification.spawned && action != "pending only", link.Applied)
				if classification.spawned {
					write.Messages[0].HasToolUse = false
					write.Messages[0].ToolCalls = nil
					_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
					require.NoError(t, err)
				} else if !classification.legacy && action != "pending only" {
					insertSession(t, d, "worker", "proj", parserSession)
				}
				exported, err := d.GetArtifactExportSession(ctx, "worker")
				require.NoError(t, err)
				assert.Nil(t, exported.ParentSessionID)
				assert.Equal(t, classification.want, exported.RelationshipType)
				switch action {
				case "clear":
					_, err = d.ClearSessionExternalParent(ctx, "worker")
				case "replace with pending", "pending only":
					link, err = d.SetSessionExternalParent(ctx, "worker", "missing-b")
					assert.False(t, link.Applied)
					if action == "pending only" {
						assert.False(t, link.Changed)
					}
				case "cycle":
					insertSession(t, d, "manager", "proj", func(s *Session) { s.ParentSessionID = new("worker"); s.RelationshipType = "continuation" })
				}
				require.NoError(t, err)
				worker, err := d.GetSession(ctx, "worker")
				require.NoError(t, err)
				assert.Nil(t, worker.ParentSessionID)
				assert.Equal(t, classification.want, worker.RelationshipType)
			})
		}
	}
}
