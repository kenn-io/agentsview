import { SessionsService, type DbSession } from "./api/generated/index.js";
import { m } from "./i18n/index.js";
import { events } from "./stores/events.svelte.js";

type NotificationBridge = {
  sendNotification: (options: { title: string; body: string }) => void;
};

function bridge() {
  return (window as Window & { __TAURI__?: { notification?: NotificationBridge } }).__TAURI__
    ?.notification;
}

export function notificationsAvailable(): boolean {
  return !!bridge();
}

function finished(row: DbSession): boolean {
  return row.termination_status === "awaiting_user" && !row.turn_open && !!row.last_reply_id;
}

const FRESHNESS_MS = 10 * 60_000;
const SAFETY_NET_REFRESH_MS = 5 * 60_000;

export function startNotificationWatcher(viewingId: () => string | null): () => void {
  const seen = new Map<string, { replyId?: string }>();
  const startedAt = Date.now();
  let coveredSince = startedAt;
  let stopped = false;
  let running = false;
  let pending = false;

  function silent(row: DbSession): boolean {
    return (
      row.relationship_type === "subagent" ||
      (viewingId() === row.id && document.visibilityState === "visible" && document.hasFocus())
    );
  }

  function send(row: DbSession) {
    if (silent(row)) return;
    const name = row.display_name || row.project || row.agent;
    bridge()?.sendNotification({
      title: m.notification_turn_end_title_suffix({ name }),
      body: m.notification_turn_end_body(),
    });
  }

  async function refresh() {
    if (running) {
      pending = true;
      return;
    }
    running = true;
    try {
      if (stopped) return;
      const fetchedAt = Date.now();
      const rows: DbSession[] = [];
      let cursor: string | undefined;
      do {
        const result = await SessionsService.getApiV1Sessions({
          active_since: new Date(Math.min(fetchedAt - FRESHNESS_MS, coveredSince)).toISOString(),
          each_row: true,
          include_one_shot: true,
          cursor,
        });
        rows.push(...result.sessions);
        cursor = result.next_cursor;
      } while (cursor && !stopped);
      if (stopped) return;
      for (const row of rows) {
        let previous = seen.get(row.id);
        if (!previous) {
          previous = {
            replyId:
              Date.parse(row.ended_at || "") >= startedAt || !finished(row)
                ? undefined
                : row.last_reply_id,
          };
          seen.set(row.id, previous);
        }
        if (!finished(row) || previous.replyId === row.last_reply_id) continue;
        try {
          send(row);
          previous.replyId = row.last_reply_id;
        } catch (err) {
          console.warn("notification delivery failed", err);
        }
      }
      coveredSince = fetchedAt;
    } catch (err) {
      console.warn("notification session read failed", err);
    } finally {
      running = false;
      if (pending && !stopped) {
        pending = false;
        void refresh();
      }
    }
  }

  const unsubscribe = events.subscribeDebounced(() => void refresh());
  const interval = setInterval(() => void refresh(), SAFETY_NET_REFRESH_MS);
  void refresh();
  return () => {
    stopped = true;
    clearInterval(interval);
    unsubscribe();
  };
}
