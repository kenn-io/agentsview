import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import { ui } from "../stores/ui.svelte.js";
import { sessions } from "../stores/sessions.svelte.js";
import { starred } from "../stores/starred.svelte.js";
import { router } from "../stores/router.svelte.js";
import { registerShortcuts } from "./keyboard.js";

function fireKey(
  key: string,
  opts: Partial<KeyboardEventInit> = {},
) {
  const event = new KeyboardEvent("keydown", {
    key,
    bubbles: true,
    ...opts,
  });
  document.dispatchEvent(event);
}

describe("registerShortcuts", () => {
  let cleanup: () => void;
  let navigateMessage: (delta: number) => void;

  beforeEach(() => {
    ui.activeModal = null;
    ui.selectedOrdinal = null;
    sessions.activeSessionId = null;
    sessions.sessions = [];
    starred.filterOnly = false;
    for (const id of [...starred.ids]) {
      starred.unstar(id);
    }
    navigateMessage = vi.fn();
    cleanup = registerShortcuts({ navigateMessage });
  });

  afterEach(() => {
    cleanup();
  });

  describe("Cmd+K modal toggle", () => {
    it("should open command palette on Cmd+K", () => {
      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBe("commandPalette");
    });

    it("should close command palette on second Cmd+K", () => {
      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBe("commandPalette");

      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBeNull();
    });

    it("should replace other modal with command palette", () => {
      ui.activeModal = "shortcuts";
      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBe("commandPalette");
    });

    it("should work with Ctrl+K", () => {
      fireKey("k", { ctrlKey: true });
      expect(ui.activeModal).toBe("commandPalette");
    });
  });

  describe("Escape handling", () => {
    it("should close active modal on Escape", () => {
      ui.activeModal = "commandPalette";
      fireKey("Escape");
      expect(ui.activeModal).toBeNull();
    });

    it("should close shortcuts modal on Escape", () => {
      ui.activeModal = "shortcuts";
      fireKey("Escape");
      expect(ui.activeModal).toBeNull();
    });

    it("should close publish modal on Escape", () => {
      ui.activeModal = "publish";
      fireKey("Escape");
      expect(ui.activeModal).toBeNull();
    });

    it("should deselect session when no modal is open", () => {
      sessions.activeSessionId = "s1";
      fireKey("Escape");
      expect(sessions.activeSessionId).toBeNull();
    });

    it("should prioritize closing modal over deselecting session", () => {
      ui.activeModal = "commandPalette";
      sessions.activeSessionId = "s1";

      fireKey("Escape");

      expect(ui.activeModal).toBeNull();
      expect(sessions.activeSessionId).toBe("s1");
    });
  });

  describe("modal blocks other shortcuts", () => {
    it("should block navigation when modal is open", () => {
      ui.activeModal = "commandPalette";
      fireKey("j");
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("should allow navigation when no modal is open", () => {
      fireKey("j");
      expect(navigateMessage).toHaveBeenCalledWith(1);
    });
  });

  describe("? opens shortcuts modal", () => {
    it("should open shortcuts modal", () => {
      fireKey("?");
      expect(ui.activeModal).toBe("shortcuts");
    });
  });

  describe("modifier keys bypass single-key shortcuts", () => {
    it("should NOT trigger shortcut on Ctrl+C", () => {
      // Ctrl+C is native copy — must not be intercepted
      const event = new KeyboardEvent("keydown", {
        key: "c",
        ctrlKey: true,
        bubbles: true,
        cancelable: true,
      });
      const prevented = !document.dispatchEvent(event);
      // If preventDefault was called, the event would be cancelled.
      // Since our handler returns early, default should NOT be prevented.
      expect(prevented).toBe(false);
    });

    it("should NOT trigger shortcut on Cmd+C (metaKey)", () => {
      const event = new KeyboardEvent("keydown", {
        key: "c",
        metaKey: true,
        bubbles: true,
        cancelable: true,
      });
      const prevented = !document.dispatchEvent(event);
      expect(prevented).toBe(false);
    });

    it("should NOT trigger navigation on Ctrl+J", () => {
      fireKey("j", { ctrlKey: true });
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("should NOT trigger navigation on Cmd+J", () => {
      fireKey("j", { metaKey: true });
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("should still navigate on plain J key", () => {
      fireKey("j");
      expect(navigateMessage).toHaveBeenCalledWith(1);
    });

    it("should still open ? shortcut (Shift is allowed)", () => {
      fireKey("?", { shiftKey: true });
      expect(ui.activeModal).toBe("shortcuts");
    });

    it("should still allow Cmd+K (modifier shortcut)", () => {
      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBe("commandPalette");
    });
  });

  describe("s shortcut (star/unstar)", () => {
    it("should toggle starred when activeSessionId exists", () => {
      sessions.activeSessionId = "session-1";
      expect(starred.isStarred("session-1")).toBe(false);

      fireKey("s");
      expect(starred.isStarred("session-1")).toBe(true);

      fireKey("s");
      expect(starred.isStarred("session-1")).toBe(false);
    });

    it("should not toggle starred when no activeSessionId", () => {
      sessions.activeSessionId = null;
      fireKey("s");
      expect(starred.count).toBe(0);
    });
  });

  describe("[ ] with starred-only filter", () => {
    function makeSession(id: string) {
      return {
        id,
        project: "proj",
        machine: "local",
        agent: "claude",
        first_message: null,
        started_at: null,
        ended_at: null,
        message_count: 1,
        user_message_count: 1,
        total_output_tokens: 0,
        peak_context_tokens: 0,
        is_automated: false,
        created_at: "2024-01-01T00:00:00Z",
      };
    }

    it("should navigate forward skipping unstarred when filterOnly is enabled", () => {
      sessions.sessions = [
        makeSession("s1"),
        makeSession("s2"),
        makeSession("s3"),
      ];
      sessions.activeSessionId = "s1";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = true;

      fireKey("]");

      // Should skip s2 (unstarred) and land on s3
      expect(sessions.activeSessionId).toBe("s3");
    });

    it("should navigate forward without filter when filterOnly is disabled", () => {
      sessions.sessions = [
        makeSession("s1"),
        makeSession("s2"),
        makeSession("s3"),
      ];
      sessions.activeSessionId = "s1";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = false;

      fireKey("]");

      // Should go to s2 (no filter applied)
      expect(sessions.activeSessionId).toBe("s2");
    });

    it("should navigate backward skipping unstarred when filterOnly is enabled", () => {
      sessions.sessions = [
        makeSession("s1"),
        makeSession("s2"),
        makeSession("s3"),
      ];
      sessions.activeSessionId = "s3";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = true;

      fireKey("[");

      // Should skip s2 (unstarred) and land on s1
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should be a no-op when filtered list is empty", () => {
      sessions.sessions = [
        makeSession("s1"),
        makeSession("s2"),
      ];
      sessions.activeSessionId = "s1";
      // No sessions are starred, so filtered list will be empty
      starred.filterOnly = true;

      fireKey("]");

      // Should remain unchanged since filtered list is empty
      expect(sessions.activeSessionId).toBe("s1");
    });
  });

  describe("b shortcut (toggle sidebar)", () => {
    it("should toggle sidebar on sessions route", () => {
      router.navigate("sessions");
      ui.sidebarOpen = true;
      fireKey("b");
      expect(ui.sidebarOpen).toBe(false);

      fireKey("b");
      expect(ui.sidebarOpen).toBe(true);
    });

    it("should navigate to sessions on non-session routes when mobile", () => {
      router.navigate("insights");
      ui.isMobileViewport = true;
      ui.sidebarOpen = false;
      fireKey("b");
      expect(router.route).toBe("sessions");
      expect(ui.sidebarOpen).toBe(true);
      ui.isMobileViewport = false;
    });

    it("should toggle sidebar on non-session routes when desktop", () => {
      router.navigate("insights");
      ui.isMobileViewport = false;
      ui.sidebarOpen = true;
      fireKey("b");
      expect(ui.sidebarOpen).toBe(false);
      expect(router.route).toBe("insights");
    });

    it("should not toggle sidebar when modal is open", () => {
      router.navigate("sessions");
      ui.sidebarOpen = true;
      ui.activeModal = "shortcuts";
      fireKey("b");
      expect(ui.sidebarOpen).toBe(true);
    });

    it("should not toggle sidebar when input is focused", () => {
      router.navigate("sessions");
      ui.sidebarOpen = true;
      const input = document.createElement("input");
      document.body.appendChild(input);
      input.focus();
      try {
        fireKey("b");
        expect(ui.sidebarOpen).toBe(true);
      } finally {
        document.body.removeChild(input);
      }
    });
  });

  describe("cleanup removes listener", () => {
    it("should stop handling keys after cleanup", () => {
      cleanup();
      fireKey("k", { metaKey: true });
      expect(ui.activeModal).toBeNull();
    });
  });
});
