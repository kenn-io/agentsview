package db

import (
	"context"
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

// TestLinkSubagentSessionsResolvesConflictingEdgesDeterministically covers the
// remaining tie: when no candidate has a usable started_at the timestamp
// cannot decide, so resolution falls back to the session id. Without that
// tiebreak a bare LIMIT 1 would return whichever edge SQLite happened to visit
// first, making the parent depend on insertion order.
func TestLinkSubagentSessionsResolvesConflictingEdgesDeterministically(
	t *testing.T,
) {
	d := testDB(t)

	insertSession(t, d, "p2", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertSession(t, d, "p1", "p", func(s *Session) {
		s.MessageCount = 1
	})
	// "kid" currently sits under p2.
	insertSession(t, d, "kid", "p", func(s *Session) {
		s.MessageCount = 1
		s.ParentSessionID = Ptr("p2")
		s.RelationshipType = "subagent"
	})

	// p2's edge is written first (lower rowid); the id tiebreak must still
	// select p1 rather than following insertion order.
	insertMessages(t, d,
		spawnEdgeTo("p2", "kid", "spawn kid (copy B)"),
		spawnEdgeTo("p1", "kid", "spawn kid (copy A)"),
	)

	require.NoError(t, d.LinkSubagentSessions(), "LinkSubagentSessions")

	kid, err := d.GetSession(context.Background(), "kid")
	requireNoError(t, err, "GetSession kid")
	assert.Equal(t, "subagent", kid.RelationshipType, "kid relationship_type")
	assert.Equal(t, "p1", parentOfSession(t, d, "kid"),
		"ties must break on session id so the parent does not depend on "+
			"which edge was inserted first")
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

// queryPlanOf returns the EXPLAIN QUERY PLAN detail lines for sql.
func queryPlanOf(t *testing.T, d *DB, sql string) string {
	t.Helper()
	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN " + sql)
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
