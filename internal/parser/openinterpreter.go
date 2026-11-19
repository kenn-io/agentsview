package parser

import "strings"

// Open Interpreter (openinterpreter.com) rewrote itself as a fork of codex-rs
// and writes the same rollout JSONL under ~/.openinterpreter, but sessions are
// exposed as a distinct agent with the open-interpreter: ID prefix so they
// resume with `interpreter resume` and never collide with a Codex UUID. The
// Codex-format provider owns parsing and relabels results through
// relabelCodexResultAsOpenInterpreter.

const openInterpreterIDPrefix = string(AgentInterpreter) + ":"

// relabelCodexResultAsOpenInterpreter rewrites a Codex-format parse result
// onto the Open Interpreter agent. sess is nil on the provider's incremental
// path, which keeps the stored session ID and only needs the appended message
// rows and late tool-result updates relabeled.
func relabelCodexResultAsOpenInterpreter(
	sess *ParsedSession, msgs []ParsedMessage, updates []ParsedToolCallUpdate,
) {
	if sess != nil {
		sess.ID = openInterpreterSessionID(sess.ID)
		sess.ParentSessionID = openInterpreterSessionID(sess.ParentSessionID)
		sess.SourceSessionID = openInterpreterSessionID(sess.SourceSessionID)
		sess.Agent = AgentInterpreter
	}
	// The Codex parser stamps subagent links with the codex: prefix
	// (codexSubagentSessionID). Without this rewrite a parent would point its
	// tool calls at codex:<uuid> rows the open-interpreter: namespace never
	// stores.
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			call := &msgs[i].ToolCalls[j]
			call.SubagentSessionID = openInterpreterSessionID(call.SubagentSessionID)
			relabelCodexResultEventsAsOpenInterpreter(call.ResultEvents)
		}
	}
	for i := range updates {
		relabelCodexResultEventsAsOpenInterpreter(updates[i].ResultEvents)
	}
}

func relabelCodexResultEventsAsOpenInterpreter(events []ParsedToolResultEvent) {
	for k := range events {
		events[k].SubagentSessionID = openInterpreterSessionID(
			events[k].SubagentSessionID,
		)
	}
}

// openInterpreterSessionID swaps the codex: prefix for open-interpreter:,
// leaving empty and already-relabeled IDs untouched. Only the first
// occurrence is replaced, matching traeXSessionID, so a raw ID that itself
// repeats "codex:" keeps the rest of its text verbatim.
func openInterpreterSessionID(id string) string {
	if id == "" {
		return id
	}
	return strings.Replace(id, codexIDPrefix, openInterpreterIDPrefix, 1)
}
