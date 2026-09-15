package parser

import "strings"

// Augure CLI (augureai.ca) is a closed-source rebrand of codex-rs and writes
// the same rollout JSONL, but sessions are exposed as a distinct agent with
// the augure: ID prefix so they resume with `augure resume` and never collide
// with a Codex or TraeX UUID. The Codex-format provider owns parsing and
// relabels results through relabelCodexResultAsAugure.

const augureIDPrefix = string(AgentAugure) + ":"

// relabelCodexResultAsAugure rewrites a Codex-format parse result onto the
// Augure agent. sess is nil on the provider's incremental path, which keeps
// the stored session ID and only needs the appended message rows relabeled.
func relabelCodexResultAsAugure(
	sess *ParsedSession, msgs []ParsedMessage, updates []ParsedToolCallUpdate,
) {
	if sess != nil {
		relabelCodexSessionAsAugure(sess)
	}
	relabelCodexMessagesAsAugure(msgs)
	relabelCodexToolCallUpdatesAsAugure(updates)
}

func relabelCodexSessionAsAugure(sess *ParsedSession) {
	if sess == nil {
		return
	}
	sess.ID = augureSessionID(sess.ID)
	sess.ParentSessionID = augureSessionID(sess.ParentSessionID)
	sess.SourceSessionID = augureSessionID(sess.SourceSessionID)
	sess.Agent = AgentAugure
}

// relabelCodexMessagesAsAugure rewrites the subagent links the Codex parser
// stamps with the codex: prefix (codexSubagentSessionID). Without this an
// Augure parent would point its tool calls at codex:<uuid> rows that the
// augure: namespace never stores.
func relabelCodexMessagesAsAugure(msgs []ParsedMessage) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			call := &msgs[i].ToolCalls[j]
			call.SubagentSessionID = augureSessionID(call.SubagentSessionID)
			relabelCodexResultEventsAsAugure(call.ResultEvents)
		}
	}
}

// relabelCodexToolCallUpdatesAsAugure applies the same subagent-link rewrite
// to the incremental path's late tool-result updates, whose events never pass
// through the message relabel. Skipping them leaves codex:-prefixed links on
// freshly appended tool results.
func relabelCodexToolCallUpdatesAsAugure(
	updates []ParsedToolCallUpdate,
) {
	for i := range updates {
		relabelCodexResultEventsAsAugure(updates[i].ResultEvents)
	}
}

func relabelCodexResultEventsAsAugure(events []ParsedToolResultEvent) {
	for k := range events {
		event := &events[k]
		event.SubagentSessionID = augureSessionID(
			event.SubagentSessionID,
		)
	}
}

// augureSessionID swaps the codex: prefix for augure:, leaving empty and
// already-relabeled IDs untouched. Only the first occurrence is replaced,
// matching traeXSessionID, so a raw ID that itself repeats "codex:" keeps
// the rest of its text verbatim.
func augureSessionID(id string) string {
	if id == "" {
		return id
	}
	return strings.Replace(id, codexIDPrefix, augureIDPrefix, 1)
}
