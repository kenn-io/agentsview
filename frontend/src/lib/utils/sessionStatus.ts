import { m } from "../i18n/index.js";
import type { SessionStatus } from "../stores/sessions.svelte.js";

/** Localized label for a session status, for the kit-ui StatusDot
 * (which takes a status string and a pre-translated label). */
export function sessionStatusLabel(status: SessionStatus): string {
  switch (status) {
    case "working":
      return m.status_dot_working();
    case "waiting":
      return m.status_dot_waiting();
    case "idle":
      return m.status_dot_idle();
    case "stale":
      return m.status_dot_stale();
    case "unclean":
      return m.status_dot_unclean();
    case "quiet":
      return "";
  }
}
