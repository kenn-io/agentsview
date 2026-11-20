import { m } from "../i18n/index.js";
import type { Route } from "../stores/router.svelte.js";
import type { Session } from "../api/types/core.js";
import { sessionLabel } from "./messages.js";
import { sessionDisplayId } from "./sessionDisplayId.js";

export function documentTitle({
  route,
  needsAuth,
  sessionId,
  session,
  sessionNotFound,
}: {
  route: Route;
  needsAuth: boolean;
  sessionId: string | null;
  session?: Session;
  sessionNotFound: boolean;
}): string {
  let label: string;
  if (needsAuth && route !== "settings") {
    label = m.app_auth_title();
  } else if (route === "sessions" && sessionId) {
    const separator = sessionId.indexOf("~");
    const rawId = separator >= 0 ? sessionId.slice(separator + 1) : sessionId;
    const displayId = `${separator >= 0 ? `${sessionId.slice(0, separator)}:` : ""}${sessionDisplayId(rawId).slice(0, 8)}`;
    if (sessionNotFound) {
      label = `${m.message_list_session_not_found()} (${displayId})`;
    } else if (session) {
      const name = sessionLabel(session.display_name?.trim(), session.first_message, "")
        .text.replace(/\s+/g, " ")
        .trim();
      const project = session.project.trim();
      label = name ? `${name} (${displayId})` : project ? `${project} (${displayId})` : displayId;
      if (name && project) label += ` · ${project}`;
    } else {
      label = displayId;
    }
  } else {
    const labels: Record<Route, () => string> = {
      sessions: m.nav_sessions,
      usage: m.nav_usage,
      "token-usage": m.nav_usage,
      activity: m.nav_activity,
      recall: m.nav_recall,
      quality: m.nav_quality,
      friction: m.nav_friction,
      pinned: m.nav_pinned,
      trash: m.nav_trash,
      "recent-edits": m.nav_recent_edits,
      data: m.nav_data,
      settings: m.header_actions_settings,
    };
    label = labels[route]();
  }
  return `${label} · AgentsView`;
}
