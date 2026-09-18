package parser

import "strings"

// Augure Code (augureai.ca) is a closed-source rebrand of codex-rs and writes
// the same rollout JSONL, but sessions are exposed as a distinct agent with
// the augure-code: ID prefix so they resume with `augure resume` and never
// collide with a Codex or TraeX UUID. The Codex-format provider owns parsing
// and relabels results through relabelCodexResultAsAugureCode.

const augureCodeIDPrefix = string(AgentAugureCode) + ":"

// relabelCodexResultAsAugureCode rewrites a Codex-format parse result onto the
// Augure Code agent. sess is nil on the provider's incremental path, which
// keeps the stored session ID and only needs the appended message rows
// relabeled.
func relabelCodexResultAsAugureCode(
	sess *ParsedSession, msgs []ParsedMessage, updates []ParsedToolCallUpdate,
) {
	if sess != nil {
		relabelCodexSessionAsAugureCode(sess)
	}
	relabelCodexMessagesAsAugureCode(msgs)
	relabelCodexToolCallUpdatesAsAugureCode(updates)
}

func relabelCodexSessionAsAugureCode(sess *ParsedSession) {
	if sess == nil {
		return
	}
	sess.ID = augureCodeSessionID(sess.ID)
	sess.ParentSessionID = augureCodeSessionID(sess.ParentSessionID)
	sess.SourceSessionID = augureCodeSessionID(sess.SourceSessionID)
	sess.Agent = AgentAugureCode
}

// relabelCodexMessagesAsAugureCode rewrites the subagent links the Codex
// parser stamps with the codex: prefix (codexSubagentSessionID). Without this
// an Augure Code parent would point its tool calls at codex:<uuid> rows that
// the augure-code: namespace never stores.
func relabelCodexMessagesAsAugureCode(msgs []ParsedMessage) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			call := &msgs[i].ToolCalls[j]
			call.SubagentSessionID = augureCodeSessionID(
				call.SubagentSessionID,
			)
			relabelCodexResultEventsAsAugureCode(call.ResultEvents)
		}
	}
}

// relabelCodexToolCallUpdatesAsAugureCode applies the same subagent-link
// rewrite to the incremental path's late tool-result updates, whose events
// never pass through the message relabel. Skipping them leaves codex:-prefixed
// links on freshly appended tool results.
func relabelCodexToolCallUpdatesAsAugureCode(
	updates []ParsedToolCallUpdate,
) {
	for i := range updates {
		relabelCodexResultEventsAsAugureCode(updates[i].ResultEvents)
	}
}

func relabelCodexResultEventsAsAugureCode(events []ParsedToolResultEvent) {
	for k := range events {
		event := &events[k]
		event.SubagentSessionID = augureCodeSessionID(
			event.SubagentSessionID,
		)
	}
}

// augureCodeSessionID swaps the codex: prefix for augure-code:, leaving empty
// and already-relabeled IDs untouched. Only the first occurrence is replaced,
// matching traeXSessionID, so a raw ID that itself repeats "codex:" keeps
// the rest of its text verbatim.
func augureCodeSessionID(id string) string {
	if id == "" {
		return id
	}
	return strings.Replace(id, codexIDPrefix, augureCodeIDPrefix, 1)
}
