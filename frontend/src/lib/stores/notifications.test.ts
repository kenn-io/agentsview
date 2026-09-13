import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { notifications } from "./notifications.svelte.js";
import type { DesktopNotification } from "../api/client.js";
import { m } from "../paraglide/messages.js";
import * as runtime from "../paraglide/runtime.js";

// The native boundary is window.__TAURI__.notification; the store's
// showNativeNotification talks to whatever bridge the stub provides.
const sendNotification = vi.fn();

function stubWindow(bridge = true) {
  vi.stubGlobal("window", {
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    __TAURI__: bridge
      ? {
          notification: {
            isPermissionGranted: async () => true,
            requestPermission: async () => "granted",
            sendNotification,
          },
        }
      : {},
  });
}

let seq = 0;

function frame(overrides: Partial<DesktopNotification> = {}): DesktopNotification {
  seq += 1;
  return {
    kind: "turn_end",
    session_id: `s${seq}`,
    project: "proj",
    agent: "claude",
    excerpt: "Done.",
    deep_link_path: "/sessions/s1?msg=last",
    created_at: `2026-09-06T12:01:0${seq % 10}Z`,
    ...overrides,
  };
}

describe("notifications store", () => {
  beforeEach(() => {
    sendNotification.mockClear();
    stubWindow();
    // Rendering happens against the active paraglide locale; pin it
    // so the English assertions below are deterministic.
    runtime.setLocale("en", { reload: false });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("delivers frames to subscribers", () => {
    const seen: DesktopNotification[] = [];
    const unsub = notifications.subscribe((n) => seen.push(n));
    notifications.deliver(frame());
    unsub();
    notifications.deliver(frame());
    expect(seen).toHaveLength(1);
    expect(seen[0]!.session_id).toBe("s1");
  });

  it("deduplicates identical frames after a reconnect burst", () => {
    const seen: DesktopNotification[] = [];
    const unsub = notifications.subscribe((n) => seen.push(n));
    const f = frame();
    notifications.deliver(f);
    notifications.deliver(f);
    unsub();
    expect(seen).toHaveLength(1);
  });

  it("renders the localized turn-end title and body", async () => {
    notifications.deliver(frame({ kind: "turn_end", project: "proj" }));
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    const { title, body } = sendNotification.mock.calls[0]![0];
    expect(title).toBe(`proj — ${m.notification_turn_end_title_suffix()}`);
    expect(body).toBe(m.notification_turn_end_body());
    // English catalogue, pinned in beforeEach.
    expect(title).toBe("proj — reply finished");
    expect(body).toBe("The agent finished this turn and is waiting for you.");
  });

  it("renders the localized new-reply title and the excerpt body", async () => {
    notifications.deliver(frame({ kind: "new_reply", project: "proj", excerpt: "Partial output" }));
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    const { title, body } = sendNotification.mock.calls[0]![0];
    expect(title).toBe(`proj — ${m.notification_new_reply_title_suffix()}`);
    expect(title).toBe("proj — new reply");
    expect(body).toBe("Partial output");
  });

  it("prefers the session's own name over the project in the title", async () => {
    notifications.deliver(
      frame({ kind: "turn_end", project: "proj", display_name: "Fix the login bug" }),
    );
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    const { title } = sendNotification.mock.calls[0]![0];
    expect(title).toBe(`Fix the login bug — ${m.notification_turn_end_title_suffix()}`);
    expect(title).toBe("Fix the login bug — reply finished");
  });

  it("falls back to the project when the session carries no name", async () => {
    notifications.deliver(frame({ kind: "turn_end", project: "proj", display_name: "" }));
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    const { title } = sendNotification.mock.calls[0]![0];
    expect(title).toBe(`proj — ${m.notification_turn_end_title_suffix()}`);
  });

  it("localizes the toast text to the active UI locale", async () => {
    runtime.setLocale("zh-CN", { reload: false });
    notifications.deliver(frame({ kind: "turn_end", project: "proj" }));
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    const { title, body } = sendNotification.mock.calls[0]![0];
    expect(title).toBe(`proj — ${m.notification_turn_end_title_suffix()}`);
    expect(title).not.toContain("reply finished");
    expect(body).toBe(m.notification_turn_end_body());
    expect(body).not.toBe("The agent finished this turn and is waiting for you.");
  });

  it("suppresses toasts for the session being viewed", async () => {
    const f = frame();
    notifications.setActiveSession(f.session_id);
    notifications.deliver(f);
    notifications.setActiveSession(null);
    const next = frame();
    notifications.deliver(next);
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    // Only the second (unviewed) frame reached the bridge.
    expect(sendNotification.mock.calls[0]![0].title).toBe(
      `proj — ${m.notification_turn_end_title_suffix()}`,
    );
  });

  it("navigates on focus within the click window", async () => {
    const navigate = vi.fn();
    const f = frame();
    notifications.deliver(f);
    // Settle the async native delivery so it cannot leak into a
    // later test's assertions.
    await vi.waitFor(() => expect(sendNotification).toHaveBeenCalledTimes(1));
    notifications.handleFocus(navigate);
    expect(navigate).toHaveBeenCalledWith(f.session_id);
    // The pending click target is consumed: a second focus is inert.
    notifications.handleFocus(navigate);
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("does not navigate long after the toast fired", () => {
    const navigate = vi.fn();
    notifications.deliver(frame());
    const realNow = Date.now;
    vi.spyOn(Date, "now").mockReturnValue(realNow() + 60_000 + 1000);
    notifications.handleFocus(navigate);
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("showNativeNotification without a bridge", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("is a no-op outside the desktop shell", async () => {
    stubWindow(false);
    const { showNativeNotification } = await import("./notifications.svelte.js");
    // No bridge, no crash: delivery is silently skipped.
    expect(() => showNativeNotification(frame())).not.toThrow();
  });
});
