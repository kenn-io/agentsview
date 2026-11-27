import type { Session } from "../api/types.js";

/** CodeBuddy IDE builds that open a stored conversation by URL. */
export const CODEBUDDY_IDE_OPENER_IDS = new Set(["codebuddy-ide", "codebuddy-ide-insiders"]);

/**
 * Reports whether a CodeBuddy session has a CLI runtime session to resume or
 * an IDE conversation to open. Only CLI and IDE 5.x conversations record one;
 * preserved branches, side questions, and unconverted 4.x manifests do not,
 * and a subagent's run belongs to its parent.
 */
export function codebuddyCanContinue(
  session: Pick<Session, "agent" | "relationship_type" | "session_kind" | "source_session_id">,
): boolean {
  if (session.agent !== "codebuddy") return true;
  return (
    !!session.source_session_id && !session.session_kind && session.relationship_type !== "subagent"
  );
}
