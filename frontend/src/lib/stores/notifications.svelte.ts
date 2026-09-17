import type { DesktopNotification } from "../api/client.js";
import { m } from "../i18n/index.js";

// If the user is already reading the session a notification is
// about, the on-screen transcript wins and no OS toast fires.
// Clicking an OS notification activates the app window without
// delivering a click payload to the webview, so activation within
// this window after a delivered toast is treated as that toast's
// click. The window only ever opens for a toast that was really
// shown: see deliver, which arms the pending click after the
// native call reports success.
const FOCUS_NAVIGATION_WINDOW_MS = 30_000;

type NotificationListener = (n: DesktopNotification) => void;

/**
 * Delivers backend-decided desktop notifications. The backend's
 * notify hub owns thresholds, dedup, and filtering; this store is
 * the delivery leg: it receives frames through the shared events
 * store connection, forwards them to the native (Tauri)
 * notification center when running in the desktop shell, and
 * never treats the SSE stream as durable — a reconnect simply
 * resumes deliveries.
 */
class NotificationsStore {
  private listeners = new Map<symbol, NotificationListener>();
  private seen = new Set<string>();
  private seenOrder: string[] = [];
  private maxSeen = 256;
  private activeSessionId: string | null = null;
  private lastNotification: DesktopNotification | null = null;
  private lastFiredAt = 0;

  /** Subscribe to every notification frame. Returns unsubscribe. */
  subscribe(fn: NotificationListener): () => void {
    const key = Symbol();
    this.listeners.set(key, fn);
    return () => {
      this.listeners.delete(key);
    };
  }

  /** Called by the session view so toasts can be suppressed while
   * the user is already reading that conversation. */
  setActiveSession(sessionId: string | null): void {
    this.activeSessionId = sessionId;
  }

  /** Entry point used by the shared events store for each
   * notification frame it receives. */
  deliver(n: DesktopNotification) {
    // SSE reconnects can deliver bursts; a cheap identity dedup
    // sits behind the archive-backed dedup on the backend.
    const id = `${n.created_at}|${n.session_id}|${n.kind}`;
    if (this.seen.has(id)) return;
    this.seen.add(id);
    this.seenOrder.push(id);
    if (this.seenOrder.length > this.maxSeen) {
      const drop = this.seenOrder.shift();
      if (drop !== undefined) this.seen.delete(drop);
    }

    // If the user is already reading this session, the on-screen
    // transcript is the better surface; skip the OS toast.
    if (this.activeSessionId === n.session_id) return;
    // Arm the focus-click target only once the OS toast is known to
    // have been delivered. Arming it here would make every ordinary
    // window focus within the window look like a notification click
    // and yank the user to a session they never clicked — including
    // in the browser, where no OS notification is ever sent.
    void showNativeNotification(n).then((delivered) => {
      if (!delivered) return;
      this.lastNotification = n;
      this.lastFiredAt = Date.now();
    });
    for (const fn of this.listeners.values()) fn(n);
  }

  /** Window-focus hook: activation shortly after an OS toast was
   * actually delivered is treated as that toast's click and opens
   * the notified session's last message. `navigate` is injected so
   * the store stays decoupled from the router. */
  handleFocus(navigate: (sessionId: string) => void): void {
    const n = this.lastNotification;
    if (n === null) return;
    if (Date.now() - this.lastFiredAt > FOCUS_NAVIGATION_WINDOW_MS) return;
    if (this.activeSessionId === n.session_id) return;
    this.lastNotification = null;
    navigate(n.session_id);
  }
}

export const notifications = new NotificationsStore();

// --- Native (Tauri) delivery -------------------------------------------------

type TauriNotificationPermission = "granted" | "denied" | "default";

type TauriNotificationBridge = {
  isPermissionGranted?: () => Promise<boolean>;
  requestPermission?: () => Promise<TauriNotificationPermission>;
  sendNotification?: (options: { title: string; body: string }) => void;
};

function tauriNotificationPlugin(): TauriNotificationBridge | null {
  if (typeof window === "undefined") return null;
  const t = (
    window as Window & {
      __TAURI__?: { notification?: TauriNotificationBridge };
    }
  ).__TAURI__;
  return t?.notification ?? null;
}

/** Fire the OS notification when running inside the Tauri shell.
 * Browsers get nothing: the web UI already surfaces live updates
 * and website notifications are out of scope for this feature.
 *
 * Resolves to whether a toast was actually sent, so callers can
 * tell "delivered" apart from "skipped" (no plugin, permission
 * denied, or a plugin error) instead of assuming it. */
export async function showNativeNotification(n: DesktopNotification): Promise<boolean> {
  const plugin = tauriNotificationPlugin();
  if (!plugin?.sendNotification) return false;
  try {
    let granted = await plugin.isPermissionGranted?.();
    if (!granted && plugin.requestPermission) {
      granted = (await plugin.requestPermission()) === "granted";
    }
    if (!granted) return false;
    plugin.sendNotification?.(notificationText(n));
    return true;
  } catch (err) {
    console.warn("native notification failed", err);
    return false;
  }
}

/** Render the localized title/body for a decided notification. The
 * backend ships structured fields only; the display copy is built
 * here from paraglide messages so OS toasts follow the active UI
 * locale. The title prefers the session's own name and falls back to
 * the project. `new_reply` shows the backend-truncated excerpt. */
export function notificationText(n: DesktopNotification): { title: string; body: string } {
  const name = n.display_name || n.project;
  if (n.kind === "new_reply") {
    return {
      title: `${name} — ${m.notification_new_reply_title_suffix()}`,
      body: n.excerpt ?? "",
    };
  }
  return {
    title: `${name} — ${m.notification_turn_end_title_suffix()}`,
    body: m.notification_turn_end_body(),
  };
}
