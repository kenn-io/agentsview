import { m } from "../i18n/index.js";
import type { Route } from "../stores/router.svelte.js";
import type { Session } from "../api/types/core.js";
import { sessionLabel } from "./messages.js";

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
    if (sessionNotFound) {
      label = `${m.message_list_session_not_found()} (${sessionId})`;
    } else if (session?.id === sessionId) {
      const name = sessionLabel(session.display_name?.trim(), session.first_message, "")
        .text.replace(/\s+/g, " ")
        .trim();
      const project = session.project.trim();
      label = name ? `${name} (${sessionId})` : project ? `${project} (${sessionId})` : sessionId;
      if (name && project) label += ` · ${project}`;
    } else {
      label = sessionId;
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
