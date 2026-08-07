package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/timeutil"
)

// TestLinkSubagentSessionsReParentsNestedGrandchild reproduces the
// nested-subagent bug: when a subagent spawns its own subagent (depth >= 2),
// the grandchild is parsed with a path-derived parent pointing at the MAIN
// session (all subagents live in the same flat <main>/subagents/ dir) and is
// tagged relationship_type='subagent'. LinkSubagentSessions must re-point it
// to the intermediate subagent that actually spawned it, using the
// authoritative tool_calls edge.
//
// Tree under test:  main -> orchestrator -> grandchild
func TestLinkSubagentSessionsReParentsNestedGrandchild(t *testing.T) {
	d := testDB(t)

	mainID := "main"
	orchestratorID := "orchestrator"
	grandchildID := "grandchild"

	// Main session (root).
	insertSession(t, d, mainID, "p", func(s *Session) {
		s.MessageCount = 1
	})

	// Orchestrator: a depth-1 subagent. Path derivation put its parent at
	// the main session (correct here) and tagged it 'subagent'.
	insertSession(t, d, orchestratorID, "p", func(s *Session) {
		s.MessageCount = 1
		parent := mainID
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})

	// Grandchild: a depth-2 subagent. Path derivation ALSO put its parent at
	// the main session (WRONG — it should be the orchestrator) and tagged it
	// 'subagent'. This is the buggy stored state we expect linking to fix.
	insertSession(t, d, grandchildID, "p", func(s *Session) {
		s.MessageCount = 1
		wrongParent := mainID
		s.ParentSessionID = &wrongParent
		s.RelationshipType = "subagent"
	})

	// The authoritative spawn edges, exactly as the parser records them in
	// tool_calls from toolUseResult.agentId:
	//   main         --Task--> orchestrator
	//   orchestrator --Task--> grandchild
	insertMessages(t,
		d,
		Message{
			SessionID: mainID, Ordinal: 0, Role: "assistant",
			Content: "spawn orchestrator", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task",
				SubagentSessionID: orchestratorID,
			}},
		},
		Message{
			SessionID: orchestratorID, Ordinal: 0, Role: "assistant",
			Content: "spawn grandchild", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task",
				SubagentSessionID: grandchildID,
			}},
		},
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	// Orchestrator stays under main.
	orch, err := d.GetSession(context.Background(), orchestratorID)
	requireNoError(t, err, "GetSession orchestrator")
	if assert.NotNil(t, orch.ParentSessionID, "orchestrator parent") {
		assert.Equal(t, mainID, *orch.ParentSessionID,
			"orchestrator.parent_session_id")
	}

	// Grandchild must be re-parented to the orchestrator, NOT the main
	// session. This is the assertion that fails on the current
	// `WHERE relationship_type != 'subagent'` guard.
	gc, err := d.GetSession(context.Background(), grandchildID)
	requireNoError(t, err, "GetSession grandchild")
	assert.Equal(t, "subagent", gc.RelationshipType,
		"grandchild relationship_type")
	if assert.NotNil(t, gc.ParentSessionID, "grandchild parent") {
		assert.Equal(t, orchestratorID, *gc.ParentSessionID,
			"grandchild.parent_session_id must be the orchestrator, "+
				"not the flat main session")
	}
}

// TestLinkSubagentSessionsUpgradesTypeWhenParentAlreadyMatches guards the
// regression flagged in review: LinkSubagentSessions sets BOTH parent_session_id
// and relationship_type='subagent'. A session can already carry the correct
// (authoritative) parent while still being misclassified as continuation / fork
// / empty. The type upgrade must run even when the parent does not change, or
// the session is grouped wrong.
func TestLinkSubagentSessionsUpgradesTypeWhenParentAlreadyMatches(t *testing.T) {
	d := testDB(t)

	// Parent session with a tool call referencing the child.
	insertSession(t, d, "parent", "p", func(s *Session) {
		s.MessageCount = 1
	})

	// Child ALREADY has the correct parent (== the tool-call spawner) but is
	// misclassified as a continuation (e.g. a header parentId that coincides
	// with the spawner). parent_session_id won't change; relationship_type
	// must still be upgraded to 'subagent'.
	insertSession(t, d, "child", "p", func(s *Session) {
		s.MessageCount = 1
		parent := "parent"
		s.ParentSessionID = &parent
		s.RelationshipType = "continuation"
	})

	insertMessages(t, d, Message{
		SessionID: "parent", Ordinal: 0, Role: "assistant",
		Content: "spawn child", HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Agent", Category: "Task",
			SubagentSessionID: "child",
		}},
	})

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	child, err := d.GetSession(context.Background(), "child")
	requireNoError(t, err, "GetSession child")
	assert.Equal(t, "subagent", child.RelationshipType,
		"relationship_type must upgrade to 'subagent' even when the parent "+
			"already matches the tool-call spawner")
	if assert.NotNil(t, child.ParentSessionID, "child parent") {
		assert.Equal(t, "parent", *child.ParentSessionID,
			"child.parent_session_id")
	}
}

// TestLinkSubagentSessionsLinksNullParentSubagent guards the null-safe `IS NOT`
// predicate. A session already tagged 'subagent' but with a NULL parent (and a
// tool_calls spawn edge) must be linked to its spawner. Replacing `IS NOT` with
// `!=` would leave the parent NULL (`NULL != 'x'` is NULL, not true), so this
// test fails under that mutation.
func TestLinkSubagentSessionsLinksNullParentSubagent(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})

	// Already tagged 'subagent' (so the type branch is false) but its parent
	// was never set. Only the null-safe parent branch can link it.
	insertSession(t, d, "orphan", "p", func(s *Session) {
		s.MessageCount = 1
		s.RelationshipType = "subagent"
		// ParentSessionID left nil -> NULL in the DB.
	})

	insertMessages(t, d, Message{
		SessionID: "spawner", Ordinal: 0, Role: "assistant",
		Content: "spawn orphan", HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Agent", Category: "Task",
			SubagentSessionID: "orphan",
		}},
	})

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	orphan, err := d.GetSession(context.Background(), "orphan")
	requireNoError(t, err, "GetSession orphan")
	if assert.NotNil(t, orphan.ParentSessionID,
		"NULL-parent subagent must be linked to its spawner (null-safe IS NOT)") {
		assert.Equal(t, "spawner", *orphan.ParentSessionID,
			"orphan.parent_session_id")
	}
}

// spawnEdgeTo builds the assistant message whose tool call records
// `from` spawning `child` — the authoritative edge LinkSubagentSessions
// resolves against.
func spawnEdgeTo(from, child, note string) Message {
	return Message{
		SessionID: from, Ordinal: 0, Role: "assistant",
		Content: note, HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Agent", Category: "Task",
			SubagentSessionID: child,
		}},
	}
}

// parentOfSession returns the stored parent of id, failing the test when it
// is unset.
func parentOfSession(t *testing.T, d *DB, id string) string {
	t.Helper()
	s, err := d.GetSession(context.Background(), id)
	requireNoError(t, err, "GetSession "+id)
	require.NotNil(t, s.ParentSessionID, "%s parent must be set", id)
	return *s.ParentSessionID
}

// TestLinkSubagentSessionsConvergesAcrossIngestionOrder covers the
// ingestion-order case raised in review. Conflicting spawn edges (reachable
// through copied or forked history) can arrive in any order, and either one
// may be the ONLY stored edge when a sync runs — so a link made from that
// partial view is provisional. Resolution must therefore be a pure function
// of the stored edges and never of the order they were written, so the
// provisional link self-corrects on the next sync instead of being locked in.
//
// A fork derives from its source and so always starts after it: the
// earliest-started spawner is the real one, which makes the resolution
// deterministic in both ingestion orders below.
func TestLinkSubagentSessionsConvergesAcrossIngestionOrder(t *testing.T) {
	const (
		realSpawner = "real-spawner"
		copySpawner = "copied-spawner"
		child       = "kid"
	)

	// setup builds two spawners plus a child already correctly parented
	// under the real (earliest-started) one.
	setup := func(t *testing.T) *DB {
		t.Helper()
		d := testDB(t)
		insertSession(t, d, realSpawner, "p", func(s *Session) {
			s.MessageCount = 1
			s.StartedAt = Ptr("2026-01-01T00:00:00.000Z")
		})
		// The copy derives from the real session, so it starts later.
		insertSession(t, d, copySpawner, "p", func(s *Session) {
			s.MessageCount = 1
			s.StartedAt = Ptr("2026-06-01T00:00:00.000Z")
		})
		insertSession(t, d, child, "p", func(s *Session) {
			s.MessageCount = 1
			s.ParentSessionID = Ptr(realSpawner)
			s.RelationshipType = "subagent"
		})
		return d
	}

	t.Run("copied edge ingested first", func(t *testing.T) {
		d := setup(t)

		// The copied edge is briefly the only stored edge: this link runs
		// with no record of the real spawn yet, so whatever it writes is
		// provisional.
		insertMessages(t, d, spawnEdgeTo(copySpawner, child, "copied spawn"))
		require.NoError(t, d.LinkSubagentSessions(), "link (copied edge only)")

		// The real edge lands on a later sync.
		insertMessages(t, d, spawnEdgeTo(realSpawner, child, "real spawn"))
		require.NoError(t, d.LinkSubagentSessions(), "link (both edges)")

		assert.Equal(t, realSpawner, parentOfSession(t, d, child),
			"child must converge to the earliest-started (real) spawner "+
				"even though the copied edge was linked first")

		// ...and stay there: linking is idempotent, not oscillating.
		require.NoError(t, d.LinkSubagentSessions(), "relink")
		assert.Equal(t, realSpawner, parentOfSession(t, d, child),
			"child must remain under the real spawner on later syncs")
	})

	t.Run("real edge ingested first", func(t *testing.T) {
		d := setup(t)

		insertMessages(t, d, spawnEdgeTo(realSpawner, child, "real spawn"))
		require.NoError(t, d.LinkSubagentSessions(), "link (real edge only)")
		assert.Equal(t, realSpawner, parentOfSession(t, d, child),
			"real spawner must be linked")

		// A later-arriving copied edge must not steal the child.
		insertMessages(t, d, spawnEdgeTo(copySpawner, child, "copied spawn"))
		require.NoError(t, d.LinkSubagentSessions(), "link (both edges)")

		assert.Equal(t, realSpawner, parentOfSession(t, d, child),
			"a later-arriving copied edge must not re-parent the child")
	})
}

// TestLinkSubagentSessionsPrefersSpawnerWithKnownStartTime guards the
// null-handling in the ordering. started_at is nullable TEXT (and the empty
// string is treated as unset elsewhere in this package), and a plain ORDER BY
// started_at sorts NULL FIRST in SQLite — which would hand the child to the
// spawner whose start time is unknown. Sessions with a usable started_at must
// win instead.
func TestLinkSubagentSessionsPrefersSpawnerWithKnownStartTime(t *testing.T) {
	d := testDB(t)

	// No usable start time: NULL and '' respectively.
	insertSession(t, d, "unknown-null", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "unknown-empty", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr("")
	})
	insertSession(t, d, "known", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr("2026-03-01T00:00:00.000Z")
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.RelationshipType = "subagent"
	})

	// The unknown-start edges are written first, so neither rowid order nor
	// NULL-first ordering may decide the winner.
	insertMessages(t, d,
		spawnEdgeTo("unknown-null", "kid", "spawn (null start)"),
		spawnEdgeTo("unknown-empty", "kid", "spawn (empty start)"),
		spawnEdgeTo("known", "kid", "spawn (known start)"),
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	assert.Equal(t, "known", parentOfSession(t, d, "kid"),
		"a spawner with a usable started_at must outrank one whose start "+
			"time is NULL or empty")
}

// TestLinkSubagentSessionsResolvesConflictingEdgesDeterministically covers
// ties: when the candidates' start times cannot rank them (here: all
// unknown), the child's established parent wins if it is among the
// candidates — an information-free tiebreak must not steal the child — and
// only otherwise does resolution fall back to the session id. Without the id
// fallback a bare LIMIT 1 would return whichever edge SQLite happened to
// visit first, making the parent depend on insertion order.
func TestLinkSubagentSessionsResolvesConflictingEdgesDeterministically(
	t *testing.T,
) {
	setup := func(t *testing.T, kidParent *string) *DB {
		t.Helper()
		d := testDB(t)
		insertSession(t, d, "p2", "p", func(s *Session) {
			s.MessageCount = 1
		})
		insertSession(t, d, "p1", "p", func(s *Session) {
			s.MessageCount = 1
		})
		insertSession(t, d, "kid", "p", func(s *Session) {
			s.MessageCount = 1
			s.ParentSessionID = kidParent
			s.RelationshipType = "subagent"
		})
		// p2's edge is written first (lower rowid), so neither outcome
		// below can come from insertion order.
		insertMessages(t, d,
			spawnEdgeTo("p2", "kid", "spawn kid (copy B)"),
			spawnEdgeTo("p1", "kid", "spawn kid (copy A)"),
		)
		return d
	}

	t.Run("established parent among tied candidates is kept", func(
		t *testing.T,
	) {
		d := setup(t, Ptr("p2"))
		require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")
		assert.Equal(t, "p2", parentOfSession(t, d, "kid"),
			"a tie must not steal the child from its established parent "+
				"just because another session id sorts first")
	})

	t.Run("no candidate parent falls back to session id", func(
		t *testing.T,
	) {
		d := setup(t, nil)
		require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")
		kid, err := d.GetSession(context.Background(), "kid")
		requireNoError(t, err, "GetSession kid")
		assert.Equal(t, "subagent", kid.RelationshipType,
			"kid relationship_type")
		assert.Equal(t, "p1", parentOfSession(t, d, "kid"),
			"with no established parent among the candidates, ties must "+
				"break on session id, not insertion order")
	})
}

// TestLinkSubagentSessionsEqualStartTieKeepsEstablishedParent covers the
// copied-history tie: a copy shares its source's started_at, so the
// earliest-started rule cannot rank them and resolution reaches the
// tiebreak. The child's established parent must win the tie even when the
// copy's session id sorts first.
func TestLinkSubagentSessionsEqualStartTieKeepsEstablishedParent(
	t *testing.T,
) {
	d := testDB(t)

	const sharedStart = "2026-04-01T00:00:00.000Z"
	// The copy's id sorts BEFORE the real spawner's, so a bare id
	// tiebreak would steal the child.
	insertSession(t, d, "aaa-copied-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(sharedStart)
	})
	insertSession(t, d, "zzz-real-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(sharedStart)
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("zzz-real-spawner")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d,
		spawnEdgeTo("aaa-copied-spawner", "kid", "copied spawn"),
		spawnEdgeTo("zzz-real-spawner", "kid", "real spawn"),
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	assert.Equal(t, "zzz-real-spawner", parentOfSession(t, d, "kid"),
		"identical start times are an information-free tie: the "+
			"established parent must be kept, not the lowest session id")
}

// TestLinkSubagentSessionsCopyFirstTieUsesParserParent covers the case where a
// copied transcript's spawn edge is the only edge present during the first
// linker pass. That pass necessarily writes a provisional copied parent. When
// the real edge arrives later with an equal or unknown start time, resolution
// must use parser-owned provenance rather than treating that mutable
// provisional parent as evidence. Both the global and watcher-scoped linkers
// must converge to the real spawner.
func TestLinkSubagentSessionsCopyFirstTieUsesParserParent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start *string
	}{
		{name: "equal start", start: Ptr("2026-04-01T00:00:00.000Z")},
		{name: "unknown start"},
	} {
		for _, linker := range []struct {
			name string
			link func(*DB, string) error
		}{
			{
				name: "global",
				link: func(d *DB, _ string) error {
					return d.LinkSubagentSessions()
				},
			},
			{
				name: "scoped",
				link: func(d *DB, spawner string) error {
					return d.LinkSubagentSessionsForSessions([]string{spawner})
				},
			},
		} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, linker.name), func(t *testing.T) {
				d := testDB(t)
				insertSession(t, d, "aaa-copy", "p", func(s *Session) {
					s.MessageCount = 1
					s.StartedAt = tc.start
				})
				insertSession(t, d, "zzz-real", "p", func(s *Session) {
					s.MessageCount = 1
					s.StartedAt = tc.start
				})
				insertSession(t, d, "kid", "p", func(s *Session) {
					s.MessageCount = 1
					s.ParentSessionID = Ptr("zzz-real")
					s.RelationshipType = "subagent"
				})

				insertMessages(t, d,
					spawnEdgeTo("aaa-copy", "kid", "copied spawn"),
				)
				require.NoError(t, linker.link(d, "aaa-copy"),
					"link copied edge only")
				assert.Equal(t, "aaa-copy", parentOfSession(t, d, "kid"),
					"the only stored edge wins provisionally")

				insertMessages(t, d,
					spawnEdgeTo("zzz-real", "kid", "real spawn"),
				)
				require.NoError(t, linker.link(d, "zzz-real"),
					"link both tied edges")
				assert.Equal(t, "zzz-real", parentOfSession(t, d, "kid"),
					"a tied real edge must recover the parser-established parent "+
						"instead of preserving the provisional copied parent")
			})
		}
	}
}

// TestLinkSubagentSessionsSubMillisecondTieKeepsEstablishedParent covers the
// precision edge of the chronological ordering: strftime's %f truncates to
// milliseconds, so two spawners whose starts differ by less than a
// millisecond normalize to the SAME value. Such a difference is below the
// resolution the ordering can see, so it must be treated as a tie that
// keeps the established parent — not decided by the sub-millisecond digits
// (which the raw strings would sort on) or by the session id.
func TestLinkSubagentSessionsSubMillisecondTieKeepsEstablishedParent(
	t *testing.T,
) {
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	copyStart := timeutil.Format(base.Add(100 * time.Microsecond))
	realStart := timeutil.Format(base.Add(400 * time.Microsecond))

	d := testDB(t)

	// Guard: both values must truncate to the same millisecond, or this
	// test would pass without exercising the tie at all.
	var normCopy, normReal string
	require.NoError(t, d.getReader().QueryRow(
		`SELECT strftime('%Y-%m-%dT%H:%M:%fZ', ?),
			strftime('%Y-%m-%dT%H:%M:%fZ', ?)`,
		copyStart, realStart,
	).Scan(&normCopy, &normReal), "normalize start times")
	require.Equal(t, normCopy, normReal,
		"guard: sub-millisecond starts must normalize to a tie")
	require.NotEqual(t, copyStart, realStart,
		"guard: the raw strings must differ so raw ordering could decide")

	// The copy starts 300 microseconds EARLIER and its id sorts first:
	// both raw-string chronology and the id tiebreak would hand it the
	// child.
	insertSession(t, d, "aaa-copied-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(copyStart)
	})
	insertSession(t, d, "zzz-real-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(realStart)
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("zzz-real-spawner")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d,
		spawnEdgeTo("aaa-copied-spawner", "kid", "copied spawn"),
		spawnEdgeTo("zzz-real-spawner", "kid", "real spawn"),
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	assert.Equal(t, "zzz-real-spawner", parentOfSession(t, d, "kid"),
		"a sub-millisecond difference is below the ordering's resolution "+
			"and must keep the established parent")
}

// TestLinkSubagentSessionsPlanScalesWithSpawnEdges pins the cost shape of the
// linking statement. Every sync calls LinkSubagentSessions, and it must stay
// cheap on a large archive in which nothing changed, so the row set has to be
// driven from the spawn edges (the idx_tool_calls_subagent partial index)
// rather than from a scan of sessions.
//
// The invariant is asserted on the query plan rather than on a wall-clock
// comparison between a small and a large archive, because the plan is what
// actually decides the scaling and it is deterministic in CI (see
// TestListActiveSessionSourceOwnershipScopesUsesBoundedSeeks for the same
// approach). The regression this guards is silent: swapping the IN back to
// the equivalent-reading EXISTS(...) keeps every behavioural test green while
// re-introducing a full sessions scan on every sync.
//
// Both archive sizes are exercised so the assertion cannot pass merely
// because the planner had no statistics to work with.
func TestLinkSubagentSessionsPlanScalesWithSpawnEdges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sessions int
	}{
		{name: "small archive", sessions: 4},
		{name: "large archive", sessions: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for i := range tc.sessions {
				insertSession(t, d, "bulk-"+strconv.Itoa(i), "p",
					func(s *Session) { s.MessageCount = 1 })
			}
			// Exactly one spawn edge, regardless of archive size.
			insertSession(t, d, "spawner", "p", func(s *Session) {
				s.MessageCount = 1
				s.StartedAt = Ptr("2026-01-01T00:00:00.000Z")
			})
			insertSession(t, d, "kid", "p", func(s *Session) {
				s.MessageCount = 1
				s.RelationshipType = "subagent"
			})
			insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))
			require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

			plan := queryPlanOf(t, d, linkSubagentSessionsQuery)

			assert.NotContains(t, plan, "SCAN s",
				"linking must not scan sessions: its cost has to track the "+
					"number of spawn edges, not the size of the archive\n"+plan)
			assert.Contains(t, plan, "idx_tool_calls_subagent",
				"linking must be driven from the spawn-edge partial index\n"+plan)
		})
	}
}

// TestLinkSubagentSessionsForSessionsScopesToBatch pins the scoped variant's
// contract: only children reachable from the given batch — via a batch
// member's spawn edges (spawner side) or as a batch member that is itself a
// child (child side) — are re-resolved. A mislinked child outside the batch
// must be left alone; that skip is what bounds a single-session watcher sync
// by the changed batch instead of the archive.
func TestLinkSubagentSessionsForSessionsScopesToBatch(t *testing.T) {
	setup := func(t *testing.T) *DB {
		t.Helper()
		d := testDB(t)
		for _, pair := range [][2]string{
			{"spawner-a", "child-a"},
			{"spawner-b", "child-b"},
		} {
			spawner, child := pair[0], pair[1]
			insertSession(t, d, spawner, "p", func(s *Session) {
				s.MessageCount = 1
			})
			// Both children carry the buggy path-derived state: wrong
			// parent, already tagged 'subagent'.
			insertSession(t, d, child, "p", func(s *Session) {
				s.MessageCount = 1
				s.ParentSessionID = Ptr("wrong-parent")
				s.RelationshipType = "subagent"
			})
			insertMessages(t, d, spawnEdgeTo(spawner, child, "spawn "+child))
		}
		return d
	}

	t.Run("spawner in batch links its child", func(t *testing.T) {
		d := setup(t)
		require.NoError(t,
			d.LinkSubagentSessionsForSessions([]string{"spawner-a"}))

		assert.Equal(t, "spawner-a", parentOfSession(t, d, "child-a"),
			"child of a batch spawner must be re-linked")
		assert.Equal(t, "wrong-parent", parentOfSession(t, d, "child-b"),
			"a mislinked child outside the batch must not be touched")
	})

	t.Run("child in batch links itself", func(t *testing.T) {
		d := setup(t)
		require.NoError(t,
			d.LinkSubagentSessionsForSessions([]string{"child-b"}))

		assert.Equal(t, "spawner-b", parentOfSession(t, d, "child-b"),
			"a batch member that is itself a child must be re-linked")
		assert.Equal(t, "wrong-parent", parentOfSession(t, d, "child-a"),
			"a mislinked child outside the batch must not be touched")
	})

	t.Run("empty batch is a no-op", func(t *testing.T) {
		d := setup(t)
		require.NoError(t, d.LinkSubagentSessionsForSessions(nil))
		assert.Equal(t, "wrong-parent", parentOfSession(t, d, "child-a"))
		assert.Equal(t, "wrong-parent", parentOfSession(t, d, "child-b"))
	})
}

// TestLinkSubagentSessionsForSessionsConvergesAcrossIngestionOrder replays
// the ingestion-order sequence of TestLinkSubagentSessionsConvergesAcross-
// IngestionOrder through the scoped variant, batching exactly the session a
// single-file watcher sync would have written each time. A conflicting edge
// always arrives through its spawner's transcript, so the spawner is in that
// sync's batch and the child it claims is re-resolved — scoping must not
// reintroduce the provisional-parent lock-in the earliest-started rule fixed.
func TestLinkSubagentSessionsForSessionsConvergesAcrossIngestionOrder(
	t *testing.T,
) {
	const (
		realSpawner = "real-spawner"
		copySpawner = "copied-spawner"
		child       = "kid"
	)

	d := testDB(t)
	insertSession(t, d, realSpawner, "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr("2026-01-01T00:00:00.000Z")
	})
	insertSession(t, d, copySpawner, "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr("2026-06-01T00:00:00.000Z")
	})
	insertSession(t, d, child, "p", func(s *Session) {
		s.MessageCount = 1
		s.RelationshipType = "subagent"
	})

	// The copied spawner's transcript syncs first: its edge is the only one
	// stored, so the link it writes is provisional.
	insertMessages(t, d, spawnEdgeTo(copySpawner, child, "copied spawn"))
	require.NoError(t,
		d.LinkSubagentSessionsForSessions([]string{copySpawner}),
		"link (copied edge only)")
	assert.Equal(t, copySpawner, parentOfSession(t, d, child),
		"the only stored edge wins provisionally")

	// The real spawner's transcript syncs later. Only the real spawner is
	// in this batch, but its edge claims the child, so the child must be
	// re-resolved against BOTH edges and converge to the earliest-started
	// (real) spawner.
	insertMessages(t, d, spawnEdgeTo(realSpawner, child, "real spawn"))
	require.NoError(t,
		d.LinkSubagentSessionsForSessions([]string{realSpawner}),
		"link (both edges)")
	assert.Equal(t, realSpawner, parentOfSession(t, d, child),
		"a scoped link must still converge to the real spawner once its "+
			"edge lands")
}

// TestLinkSubagentSessionsForSessionsChunksLargeBatches drives a batch past
// the per-statement chunk size (each id binds twice, so chunks are halved) to
// prove linking still reaches ids beyond the first chunk.
func TestLinkSubagentSessionsForSessionsChunksLargeBatches(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("wrong-parent")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))

	// Front-load enough filler ids that the real spawner lands in a later
	// chunk.
	ids := make([]string, 0, maxSQLVars+1)
	for i := range maxSQLVars {
		ids = append(ids, "no-such-session-"+strconv.Itoa(i))
	}
	ids = append(ids, "spawner")

	require.NoError(t, d.LinkSubagentSessionsForSessions(ids))
	assert.Equal(t, "spawner", parentOfSession(t, d, "kid"),
		"an id beyond the first chunk must still drive linking")
}

// TestLinkSubagentSessionsForSessionsClearsDanglingParent covers removal of
// a child's SOLE spawn edge together with its spawner: neither UNION branch
// of the linking statement can reach an edge-less child, and its parent now
// points at a deleted session, so the captured child must be un-parented
// instead of keeping the dangling reference indefinitely. The
// relationship_type stays 'subagent' so a reappearing edge re-links it.
func TestLinkSubagentSessionsForSessionsClearsDanglingParent(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("spawner")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))

	// The spawner is parser-excluded: the delete cascades its messages
	// and with them the only edge claiming the kid.
	n, err := d.DeleteParserExcludedSessions([]string{"spawner"})
	require.NoError(t, err, "DeleteParserExcludedSessions")
	require.Equal(t, 1, n, "spawner must be deleted")

	// The engine captures the kid as a pre-write child of the deleted
	// spawner and persists cleanup intent before the destructive write.
	require.NoError(t, d.QueueSubagentParentCleanupRepairs([]string{"kid"}))
	require.NoError(t, d.RepairQueuedSubagentParents())

	kid, err := d.GetSession(context.Background(), "kid")
	requireNoError(t, err, "GetSession kid")
	assert.Nil(t, kid.ParentSessionID,
		"a child whose sole edge and spawner are gone must be un-parented, "+
			"not left pointing at a deleted session")
	assert.Equal(t, "subagent", kid.RelationshipType,
		"the subagent classification survives so a reappearing edge "+
			"re-links the child")
}

// TestLinkSubagentSessionsForSessionsKeepsUnresolvedPathParent proves that a
// normal changed-session seed is not evidence that its parent was deleted.
// Providers can ingest a path-derived child before its parent, with no spawn
// edge yet available; scoped edge linking must preserve that parser claim so
// the later parent row completes the hierarchy instead of leaving the child
// permanently un-parented.
func TestLinkSubagentSessionsForSessionsKeepsUnresolvedPathParent(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("parent-not-ingested-yet")
		s.RelationshipType = "subagent"
	})

	require.NoError(t, d.LinkSubagentSessionsForSessions([]string{"kid"}))
	assert.Equal(t, "parent-not-ingested-yet", parentOfSession(t, d, "kid"),
		"a generic changed-session seed must preserve parser-derived parentage")
	require.NoError(t, d.QueueSubagentParentRepairs([]string{"kid"}))
	require.NoError(t, d.RepairQueuedSubagentParents())
	assert.Equal(t, "parent-not-ingested-yet", parentOfSession(t, d, "kid"),
		"a durable generic repair must remain relink-only")

	insertSession(t, d, "parent-not-ingested-yet", "p", func(s *Session) {
		s.MessageCount = 1
	})
	require.NoError(t, d.LinkSubagentSessionsForSessions(
		[]string{"parent-not-ingested-yet"},
	))
	assert.Equal(t, "parent-not-ingested-yet", parentOfSession(t, d, "kid"),
		"ingesting the parent later must leave the valid path hierarchy intact")
}

// TestLinkSubagentSessionsForSessionsKeepsParentWhenSpawnerRemains pins the
// deliberate limit of the dangling-parent repair: when only the EDGE is
// gone but the stored parent session still exists, nothing distinguishes an
// edge-derived parent (now stale) from a path-derived one that is still
// valid (e.g. a Claude subagent whose directory placement proves
// membership), so the safer failure mode is to keep the historical claim.
func TestLinkSubagentSessionsForSessionsKeepsParentWhenSpawnerRemains(
	t *testing.T,
) {
	d := testDB(t)

	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("spawner")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))

	// Remove only the edge (a rewrite dropping the Task call); the
	// spawner session itself remains.
	_, err := d.getWriter().Exec(
		`DELETE FROM messages WHERE session_id = 'spawner'`,
	)
	require.NoError(t, err, "delete spawner messages")

	require.NoError(t, d.LinkSubagentSessionsForSessions(
		[]string{"spawner", "kid"},
	))

	assert.Equal(t, "spawner", parentOfSession(t, d, "kid"),
		"an existing parent session must be kept when only the edge is "+
			"gone: the parent may be path-derived and still valid")
}

// TestSubagentChildSessionIDs covers the pre-write capture helper: it must
// return the distinct children the given sessions' spawn edges reference,
// ignore sessions with no edges, and treat an empty input as a no-op.
func TestSubagentChildSessionIDs(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"s1", "s2", "quiet"} {
		insertSession(t, d, id, "p", func(s *Session) { s.MessageCount = 1 })
	}
	insertMessages(t, d,
		spawnEdgeTo("s1", "kid-a", "spawn a"),
		spawnEdgeTo("s2", "kid-b", "spawn b"),
	)

	children, err := d.SubagentChildSessionIDs([]string{"s1", "s2", "quiet"})
	require.NoError(t, err, "SubagentChildSessionIDs")
	assert.ElementsMatch(t, []string{"kid-a", "kid-b"}, children)

	none, err := d.SubagentChildSessionIDs(nil)
	require.NoError(t, err, "empty input")
	assert.Empty(t, none)
}

// TestQueueSubagentParentRepairsAdditionWorkIsQueueSizeIndependent protects
// the watcher/bulk-sync cost shape: adding one affected child must not decode
// and rewrite every child already waiting for repair. Repeating an existing
// ID keeps the observable queue unchanged while isolating the per-add work.
func TestQueueSubagentParentRepairsAdditionWorkIsQueueSizeIndependent(
	t *testing.T,
) {
	queueAllocs := func(size int) float64 {
		t.Helper()
		d := testDB(t)
		ids := make([]string, 0, size)
		for i := range size {
			ids = append(ids, "queued-child-"+strconv.Itoa(i))
		}
		require.NoError(t, d.QueueSubagentParentRepairs(ids))
		return testing.AllocsPerRun(3, func() {
			require.NoError(t, d.QueueSubagentParentRepairs(
				[]string{"queued-child-0"},
			))
		})
	}

	small := queueAllocs(1)
	large := queueAllocs(500)
	assert.Less(t, large, small*3,
		"adding one ID must not allocate in proportion to queued IDs: "+
			"small=%0.0f large=%0.0f", small, large)
}

func TestRepairQueuedSubagentParentsMigratesLegacyJSONQueue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("wrong-parent")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))
	require.NoError(t, d.SetSyncState(
		subagentParentRepairQueueStateKey, `["kid"]`,
	))

	require.NoError(t, d.RepairQueuedSubagentParents())

	assert.Equal(t, "spawner", parentOfSession(t, d, "kid"))
	legacy, err := d.GetSyncState(subagentParentRepairQueueStateKey)
	require.NoError(t, err)
	assert.Empty(t, legacy, "successful migration must remove the JSON queue")
	var queued int
	require.NoError(t, d.Reader().QueryRow(
		"SELECT count(*) FROM subagent_parent_repair_queue",
	).Scan(&queued))
	assert.Zero(t, queued, "successful repair must clear migrated rows")
}

func TestRepairQueuedSubagentParentsMigratesLegacyCleanupIntent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "spawner", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("spawner")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))
	require.NoError(t, d.SetSyncState(
		subagentParentRepairQueueStateKey, `["kid"]`,
	))
	_, err := d.DeleteParserExcludedSessions([]string{"spawner"})
	require.NoError(t, err)

	require.NoError(t, d.RepairQueuedSubagentParents())

	kid, err := d.GetSession(context.Background(), "kid")
	require.NoError(t, err)
	require.NotNil(t, kid)
	assert.Nil(t, kid.ParentSessionID,
		"the legacy queue contained pre-write children and must retain cleanup intent")
}

// TestLinkSubagentSessionsForSessionsPlanIsBatchBounded pins the cost shape
// of the scoped statement, mirroring TestLinkSubagentSessionsPlanScalesWith-
// SpawnEdges: the watcher calls this once per changed file, so neither
// sessions nor tool_calls may be scanned — both UNION branches have to seek
// their index and the archive's size must never enter the plan.
func TestLinkSubagentSessionsForSessionsPlanIsBatchBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sessions int
	}{
		{name: "small archive", sessions: 4},
		{name: "large archive", sessions: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for i := range tc.sessions {
				insertSession(t, d, "bulk-"+strconv.Itoa(i), "p",
					func(s *Session) { s.MessageCount = 1 })
			}
			insertSession(t, d, "spawner", "p", func(s *Session) {
				s.MessageCount = 1
			})
			insertSession(t, d, "kid", "p", func(s *Session) {
				s.MessageCount = 1
				s.RelationshipType = "subagent"
			})
			insertMessages(t, d, spawnEdgeTo("spawner", "kid", "spawn"))
			require.NoError(t,
				d.LinkSubagentSessionsForSessions([]string{"spawner"}))

			plan := queryPlanOf(
				t, d, linkSubagentSessionsForSessionsQuery("(?)"),
				"spawner", "spawner",
			)

			assert.NotContains(t, plan, "SCAN s",
				"scoped linking must not scan sessions\n"+plan)
			assert.NotContains(t, plan, "SCAN tc",
				"scoped linking must not scan tool_calls: per-event cost "+
					"has to track the batch, not the archive's edges\n"+plan)
			assert.Regexp(t, `idx_tool_calls_(session|dedup)`, plan,
				"the spawner-side branch must seek a session-prefixed index\n"+
					plan)
			assert.Contains(t, plan, "idx_tool_calls_subagent",
				"the child-side branch must seek the subagent partial "+
					"index\n"+plan)
		})
	}
}

// queryPlanOf returns the EXPLAIN QUERY PLAN detail lines for sql.
func queryPlanOf(t *testing.T, d *DB, sql string, args ...any) string {
	t.Helper()
	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+sql, args...)
	requireNoError(t, err, "EXPLAIN QUERY PLAN")
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}

// TestLinkSubagentSessionsOrdersStartTimesChronologically covers the
// timestamp-ordering case raised in review. started_at is TEXT written by
// timeutil.Format (time.RFC3339Nano), which strips trailing zeros from the
// fractional second, so production values are NOT fixed width: a whole-second
// start is stored '...T00:00:00Z' and a later one '...T00:00:00.1Z'. Because
// '.' sorts before 'Z', raw lexical order puts the LATER timestamp FIRST —
// which would resolve the child to the copy. The ordering must normalize
// started_at so it compares chronologically.
func TestLinkSubagentSessionsOrdersStartTimesChronologically(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	realStart := timeutil.Format(base)
	copyStart := timeutil.Format(base.Add(100 * time.Millisecond))

	// Pin the production shapes this test exists for: the real spawner starts
	// 100ms EARLIER, yet its stored string sorts LATER lexically.
	require.Equal(t, "2026-01-01T00:00:00Z", realStart, "whole-second form")
	require.Equal(t, "2026-01-01T00:00:00.1Z", copyStart, "fractional form")
	require.Less(t, copyStart, realStart,
		"guard: raw lexical order must be inverted here, otherwise this test "+
			"would pass even without normalizing started_at")

	d := testDB(t)
	insertSession(t, d, "real-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(realStart)
	})
	insertSession(t, d, "copied-spawner", "p", func(s *Session) {
		s.MessageCount = 1
		s.StartedAt = Ptr(copyStart)
	})
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.RelationshipType = "subagent"
	})

	insertMessages(t, d,
		spawnEdgeTo("copied-spawner", "kid", "copied spawn"),
		spawnEdgeTo("real-spawner", "kid", "real spawn"),
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	assert.Equal(t, "real-spawner", parentOfSession(t, d, "kid"),
		"the chronologically earliest spawner must win; ordering the raw "+
			"RFC3339Nano strings would pick the later copied spawner")
}
