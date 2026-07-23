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
