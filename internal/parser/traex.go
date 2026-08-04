package parser

import "strings"

// TraeX (TRAE CLI 2.0) is a closed-source fork of codex-rs and writes the
// same rollout JSONL, but sessions are exposed as a distinct agent with the
// traex: ID prefix so they resume with `traex resume` and never collide with
// a Codex UUID. The Codex-format provider owns parsing and relabels results
// through relabelCodexResultAsTraeX.

const (
	codexIDPrefix = string(AgentCodex) + ":"
	traeXIDPrefix = string(AgentTraeX) + ":"
)

// relabelCodexResultAsTraeX rewrites a Codex-format parse result onto the
// TraeX agent. sess is nil on the provider's incremental path, which keeps
// the stored session ID and only needs the appended message rows relabeled.
func relabelCodexResultAsTraeX(sess *ParsedSession, msgs []ParsedMessage) {
	if sess != nil {
		relabelCodexSessionAsTraeX(sess)
	}
	relabelCodexMessagesAsTraeX(msgs)
}

func relabelCodexSessionAsTraeX(sess *ParsedSession) {
	if sess == nil {
		return
	}
	sess.ID = traeXSessionID(sess.ID)
	sess.ParentSessionID = traeXSessionID(sess.ParentSessionID)
	sess.SourceSessionID = traeXSessionID(sess.SourceSessionID)
	sess.Agent = AgentTraeX
}

// relabelCodexMessagesAsTraeX rewrites the subagent links the Codex parser
// stamps with the codex: prefix (codexSubagentSessionID). Without this a
// TraeX parent would point its tool calls at codex:<uuid> rows that the
// traex: namespace never stores.
func relabelCodexMessagesAsTraeX(msgs []ParsedMessage) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			call := &msgs[i].ToolCalls[j]
			call.SubagentSessionID = traeXSessionID(call.SubagentSessionID)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.SubagentSessionID = traeXSessionID(
					event.SubagentSessionID,
				)
			}
		}
	}
}

// traeXSessionID swaps the codex: prefix for traex:, leaving empty and
// already-relabeled IDs untouched. Only the first occurrence is replaced,
// matching relabelOpenCodeSessionAsKilo, so a raw ID that itself repeats
// "codex:" keeps the rest of its text verbatim.
func traeXSessionID(id string) string {
	if id == "" {
		return id
	}
	return strings.Replace(id, codexIDPrefix, traeXIDPrefix, 1)
}
