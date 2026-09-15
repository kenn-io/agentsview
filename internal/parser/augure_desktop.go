package parser

import (
	"path/filepath"
	"strings"
)

// Augure Desktop v3 (ca.augureai.desktop) embeds a fork of Hermes Agent
// renamed to ~/.augure-desktop. The state.db schema matches Hermes's, so the
// Hermes provider owns parsing and relabels results onto the augure-desktop:
// ID namespace through relabelHermesResultAsAugureDesktop. Only the display
// ID gets the prefix; virtual paths and raw lookups keep the raw id column
// value verbatim.

const augureDesktopIDPrefix = string(AgentAugureDesktop) + ":"

// augureDesktopForkMarker is the directory name an archive root must carry
// for the Augure Desktop provider to claim it. The marker is the store's own
// root name, not the schema: a generic state.db layout stays with Hermes
// (lesson 75: schema shape is never a format marker).
const augureDesktopForkMarker = ".augure-desktop"

// augureDesktopForkMarkerWindows is the marker's Windows spelling, matching
// the fork's %LOCALAPPDATA%\augure-desktop data root (no leading dot; see
// hermes_constants.py's DEFAULT_HERMES_HOME_DIRNAME_WINDOWS).
const augureDesktopForkMarkerWindows = "augure-desktop"

// isAugureDesktopForkRoot reports whether the path resolves under an
// augure-desktop-named directory.
func isAugureDesktopForkRoot(root string) bool {
	cleaned := filepath.Clean(root)
	for {
		base := filepath.Base(cleaned)
		if base == augureDesktopForkMarker ||
			base == augureDesktopForkMarkerWindows {
			return true
		}
		parent := filepath.Dir(cleaned)
		if parent == cleaned {
			return false
		}
		cleaned = parent
	}
}

// relabelHermesResultAsAugureDesktop rewrites a Hermes-format parse result
// onto the Augure Desktop agent: the session and parent IDs gain the
// augure-desktop: prefix (once), the agent label flips, and usage-event
// session IDs follow so per-model rows stay attached to the session.
func relabelHermesResultAsAugureDesktop(result *ParseResult) {
	result.Session.ID = augureDesktopSessionID(result.Session.ID)
	result.Session.ParentSessionID =
		augureDesktopSessionID(result.Session.ParentSessionID)
	result.Session.SourceSessionID = strings.TrimPrefix(
		augureDesktopSessionID(result.Session.SourceSessionID),
		augureDesktopIDPrefix,
	)
	result.Session.Agent = AgentAugureDesktop
	// applyHermesStateMetadata prefixes the project with the producer name;
	// the fork must not advertise itself as a Hermes project.
	result.Session.Project = strings.Replace(
		result.Session.Project, "hermes", "augure-desktop", 1,
	)
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
				call.ResultEvents[k].SubagentSessionID =
					augureDesktopSessionID(
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
