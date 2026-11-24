import { m } from "../i18n/index.js";
import type { Route } from "../stores/router.svelte.js";

export function documentTitle({
  route,
  needsAuth,
  sessionId,
}: {
  route: Route;
  needsAuth: boolean;
  sessionId: string | null;
}): string {
  let label: string;
  if (needsAuth && route !== "settings") {
    label = m.app_auth_title();
  } else if (route === "sessions" && sessionId) {
    let hash = 0x811c9dc5;
    for (const byte of new TextEncoder().encode(sessionId)) {
      hash = Math.imul(hash ^ byte, 0x01000193) >>> 0;
    }
    label = `${m.activity_session()} (${hash.toString(16).padStart(8, "0")})`;
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
  return `${label} · Agentic HQ`;
}
