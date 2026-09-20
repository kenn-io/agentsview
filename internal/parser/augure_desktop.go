package parser

import (
	"strings"
)

// Augure Desktop v3 (ca.augureai.desktop) embeds a fork of Hermes Agent
// renamed to ~/.augure-desktop. The state.db schema matches Hermes's, so the
// Hermes provider owns parsing and relabels results onto the augure-desktop:
// ID namespace through relabelHermesResultAsAugureDesktop. Only the display
// ID gets the prefix; virtual paths and raw lookups keep the raw id column
// value verbatim.

const augureDesktopIDPrefix = string(AgentAugureDesktop) + ":"

// relabelHermesResultAsAugureDesktop rewrites a Hermes-format parse result
// onto the Augure Desktop agent: the session and parent IDs gain the
// augure-desktop: prefix (once), the agent label flips, and usage-event
// session IDs follow so per-model rows stay attached to the session.
func relabelHermesResultAsAugureDesktop(result *ParseResult) {
	result.Session.ID = augureDesktopSessionID(result.Session.ID)
	result.Session.ParentSessionID = augureDesktopSessionID(result.Session.ParentSessionID)
	result.Session.SourceSessionID = strings.TrimPrefix(
		augureDesktopSessionID(result.Session.SourceSessionID),
		augureDesktopIDPrefix,
	)
	result.Session.Agent = AgentAugureDesktop
	// applyHermesStateMetadata synthesizes the project from the producer
	// name ("hermes" / "hermes-<source>"); the fork must not advertise
	// itself as a Hermes project. Explicit project hints pass through
	// untouched.
	if result.Session.projectSynthesizedByHermes {
		result.Session.Project = augureDesktopProject(result.Session.Project)
	}
	for i := range result.UsageEvents {
		result.UsageEvents[i].SessionID = augureDesktopSessionID(
			result.UsageEvents[i].SessionID,
		)
	}
	for i := range result.Messages {
		msg := &result.Messages[i]
		for j := range msg.ToolCalls {
			call := &msg.ToolCalls[j]
			call.SubagentSessionID = augureDesktopSessionID(
				call.SubagentSessionID,
			)
			for k := range call.ResultEvents {
				call.ResultEvents[k].SubagentSessionID = augureDesktopSessionID(
					call.ResultEvents[k].SubagentSessionID,
				)
			}
		}
	}
}

// augureDesktopSessionID swaps the hermes: prefix for augure-desktop:,
// leaving empty and already-relabeled IDs untouched. Only the first
// occurrence is replaced, matching traeXSessionID and augureSessionID.
func augureDesktopSessionID(id string) string {
	if id == "" {
		return id
	}
	return strings.Replace(id, hermesIDPrefix, augureDesktopIDPrefix, 1)
}

// augureDesktopProject rebrands a synthesized Hermes project name
// ("hermes" or "hermes-<source>") to the fork's producer name.
func augureDesktopProject(project string) string {
	if project == "hermes" {
		return "augure-desktop"
	}
	return "augure-desktop-" + strings.TrimPrefix(project, "hermes-")
}
