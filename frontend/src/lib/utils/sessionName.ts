import { ui } from "../stores/ui.svelte.js";

/**
 * Whether agent-provided session names (name_source === "agent") are
 * currently shown. Isolated here so a future migration to always-on
 * (removing the toggle) is a one-line change at this single call site;
 * every display path routes through visibleSessionName below.
 */
function agentNamesVisible(): boolean {
  return ui.showSessionNames;
}

/**
 * The session display name to show, or null when it should be suppressed.
 * Manual renames (name_source "user") and legacy/imported names (null)
 * always show; agent-provided names are gated on the preference.
 */
export function visibleSessionName(
  session: { display_name?: string | null; name_source?: string | null },
): string | null {
  if (!session.display_name) return null;
  if (session.name_source === "agent" && !agentNamesVisible()) return null;
  return session.display_name;
}
