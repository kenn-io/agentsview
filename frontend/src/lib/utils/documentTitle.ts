import { m } from "../i18n/index.js";
import type { Route } from "../stores/router.svelte.js";
import { agentLabel } from "./agents.js";

export function documentTitle({
  route,
  needsAuth,
  sessionId,
  sessionNotFound,
}: {
  route: Route;
  needsAuth: boolean;
  sessionId: string | null;
  sessionNotFound: boolean;
}): string {
  let label: string;
  if (needsAuth && route !== "settings") {
    label = m.app_auth_title();
  } else if (route === "sessions" && sessionId) {
    const separator = sessionId.indexOf("~");
    const rawId = separator >= 0 ? sessionId.slice(separator + 1) : sessionId;
    const providerSeparator = rawId.indexOf(":");
    const provider = providerSeparator >= 0 ? rawId.slice(0, providerSeparator) : "claude";
    const id = providerSeparator >= 0 ? rawId.slice(providerSeparator + 1) : rawId;
    label = [
      m.nav_sessions(),
      ...(separator >= 0 ? [sessionId.slice(0, separator)] : []),
      agentLabel(provider),
      id.slice(-12),
      ...(sessionNotFound ? [m.message_list_session_not_found()] : []),
    ].join(" · ");
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
