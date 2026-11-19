import { m } from "../i18n/index.js";
import type { Route } from "../stores/router.svelte.js";
import type { Session } from "../api/types/core.js";
import { normalizeMessagePreview } from "./messages.js";
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
    if (sessionNotFound) {
      label = `${m.message_list_session_not_found()} (${sessionDisplayId(sessionId)})`;
    } else if (session?.id === sessionId) {
      const name = (session.display_name || normalizeMessagePreview(session.first_message))
        .replace(/\s+/g, " ")
        .trim();
      label = name ? `${name} · ${session.project}` : session.project;
    } else {
      label = sessionDisplayId(sessionId);
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
