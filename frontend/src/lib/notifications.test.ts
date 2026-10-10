import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { SessionsService, type DbSession } from "./api/generated/index.js";
import { startNotificationWatcher } from "./notifications.js";

const mocks = vi.hoisted(() => ({ callback: () => {}, unsubscribe: vi.fn() }));
vi.mock("./stores/events.svelte.js", () => ({
  events: {
    subscribeDebounced: vi.fn((callback) => {
      mocks.callback = callback;
      return mocks.unsubscribe;
    }),
  },
}));
vi.mock("./api/generated/index.js", () => ({
  SessionsService: {
    getApiV1Sessions: vi.fn(),
  },
}));
const plugin = {
  sendNotification: vi.fn(),
};
let stop: (() => void) | undefined;
let row: DbSession;
const list = vi.mocked(SessionsService.getApiV1Sessions);
async function flush() {
  for (let i = 0; i < 15; i++) await Promise.resolve();
}
async function start(viewing: string | null = null) {
  stop = startNotificationWatcher(() => viewing);
  await flush();
  expect(list).toHaveBeenCalled();
}
async function change(patch: Partial<DbSession> = {}) {
  row = { ...row, ...patch };
  mocks.callback();
  await flush();
}
beforeEach(() => {
  vi.resetAllMocks();
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-10-07T12:00:00Z"));
  vi.stubGlobal("__TAURI__", { notification: plugin });
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  row = {
    id: "session",
    agent: "claude",
    project: "demo",
    display_name: "Fix login",
    created_at: "2026-10-07T11:00:00Z",
    started_at: "2026-10-07T11:00:00Z",
    ended_at: "2026-10-07T12:00:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 100,
    last_reply_id: "reply-1",
  } as DbSession;
  list.mockImplementation(async () => ({ sessions: [row], total: 1 }));
});
afterEach(() => {
  stop?.();
  stop = undefined;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
describe("desktop notification watcher", () => {
  it.each(["2026-10-07T11:00:00Z", "2026-10-07T12:00:00Z", "2026-10-07T12:00:01Z"])(
    "toasts a newly completed turn in a session started at %s once",
    async (started_at) => {
      list.mockResolvedValueOnce({ sessions: [], total: 0 });
      await start();
      row = {
        ...row,
        started_at,
        ended_at: "2026-10-07T12:01:00Z",
        termination_status: "awaiting_user",
      };
      await vi.advanceTimersByTimeAsync(5 * 60_000);
      await flush();
      await change();
      expect(plugin.sendNotification).toHaveBeenCalledExactlyOnceWith({
        title: "Fix login: turn finished",
        body: "The agent finished this turn and is waiting for you.",
      });
      stop?.();
      vi.setSystemTime(new Date("2026-10-07T12:06:00Z"));
      await start();
      expect(plugin.sendNotification).toHaveBeenCalledOnce();
    },
  );
  it("toasts a new session completed during the initial refresh", async () => {
    row.started_at = "2026-10-07T12:00:00Z";
    row.termination_status = "awaiting_user";
    await start();
    await change();
    expect(plugin.sendNotification).toHaveBeenCalledOnce();
  });
  it.each([undefined, ""])("stays silent without a reply ID: %s", async (last_reply_id) => {
    row.last_reply_id = last_reply_id;
    await start();
    await change({ termination_status: "awaiting_user" });
    expect(plugin.sendNotification).not.toHaveBeenCalled();
  });
  it("records a finished import after enabling silently", async () => {
    list.mockResolvedValueOnce({ sessions: [], total: 0 });
    await start();
    await change({
      created_at: "2026-10-07T12:01:00Z",
      started_at: "2026-10-07T11:00:00Z",
      ended_at: "2026-10-07T11:59:59Z",
      termination_status: "awaiting_user",
    });
    await change();
    expect(plugin.sendNotification).not.toHaveBeenCalled();
  });
  it.each([undefined, "awaiting_user"])(
    "notifies only after pending work closes with saved status %s",
    async (termination_status) => {
      row.termination_status = termination_status;
      row.turn_open = true;
      await start();
      await change();
      expect(plugin.sendNotification).not.toHaveBeenCalled();
      await change({ turn_open: false, termination_status: "awaiting_user" });
      await change();
      expect(plugin.sendNotification).toHaveBeenCalledOnce();
    },
  );

  it.each([{ relationship_type: "subagent" }, {}])(
    "remembers silent completions: %j",
    async (patch) => {
      await start("session");
      await change({ ...patch, termination_status: "awaiting_user" });
      vi.spyOn(document, "hasFocus").mockReturnValue(false);
      await change();
      expect(plugin.sendNotification).not.toHaveBeenCalled();
    },
  );
  it("notifies for the viewed session while the window is hidden", async () => {
    await start("session");
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await change({ termination_status: "awaiting_user" });
    expect(plugin.sendNotification).toHaveBeenCalledOnce();
  });
  it("discovers a completion after refresh failures longer than ten minutes", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    await start();
    list.mockRejectedValue(new Error("unavailable"));
    vi.setSystemTime(new Date("2026-10-07T12:01:00Z"));
    await change({ termination_status: "awaiting_user", ended_at: "2026-10-07T12:01:00Z" });
    vi.setSystemTime(new Date("2026-10-07T12:15:00Z"));
    await change();
    list.mockImplementation(async (params) => ({
      sessions: Date.parse(row.ended_at!) >= Date.parse(params!.active_since!) ? [row] : [],
      total: 1,
    }));
    vi.setSystemTime(new Date("2026-10-07T12:20:00Z"));
    await change();
    expect(list).toHaveBeenLastCalledWith({
      active_since: "2026-10-07T12:00:00.000Z",
      each_row: true,
      include_one_shot: true,
      cursor: undefined,
    });
    expect(plugin.sendNotification).toHaveBeenCalledOnce();
    await change();
    expect(list).toHaveBeenLastCalledWith({
      active_since: "2026-10-07T12:10:00.000Z",
      each_row: true,
      include_one_shot: true,
      cursor: undefined,
    });
    expect(plugin.sendNotification).toHaveBeenCalledOnce();
  });
  it("reads every page", async () => {
    list.mockImplementation(async (params) =>
      params?.cursor
        ? { sessions: [row], total: 2 }
        : { sessions: [{ ...row, id: "other" }], total: 2, next_cursor: "page-2" },
    );
    await start();
    expect(list).toHaveBeenNthCalledWith(1, {
      active_since: "2026-10-07T11:50:00.000Z",
      each_row: true,
      include_one_shot: true,
      cursor: undefined,
    });
    expect(list).toHaveBeenNthCalledWith(2, {
      active_since: "2026-10-07T11:50:00.000Z",
      each_row: true,
      include_one_shot: true,
      cursor: "page-2",
    });
    await change({ termination_status: "awaiting_user" });
    expect(plugin.sendNotification).toHaveBeenCalledTimes(2);
  });
  it("remembers re-entry after ten idle minutes", async () => {
    row.ended_at = "2026-10-07T11:59:59Z";
    row.termination_status = "awaiting_user";
    await start();
    list.mockResolvedValue({ sessions: [], total: 0 });
    vi.setSystemTime(new Date("2026-10-07T12:11:00Z"));
    await change();
    list.mockImplementation(async () => ({ sessions: [row], total: 1 }));
    await change({ message_count: 3 });
    expect(plugin.sendNotification).not.toHaveBeenCalled();
    await change({ last_reply_id: "reply-2" });
    expect(plugin.sendNotification).toHaveBeenCalledOnce();
  });

  it("unsubscribes and cancels refreshes on stop", async () => {
    await start();
    stop?.();
    expect(mocks.unsubscribe).toHaveBeenCalledOnce();
    const calls = list.mock.calls.length;
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    await change({ termination_status: "awaiting_user" });
    expect(list).toHaveBeenCalledTimes(calls);
    expect(plugin.sendNotification).not.toHaveBeenCalled();
  });
});
