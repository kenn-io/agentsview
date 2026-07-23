package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
