import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { ui } from "../stores/ui.svelte.js";
import { sessions } from "../stores/sessions.svelte.js";
import { starred } from "../stores/starred.svelte.js";
import { router } from "../stores/router.svelte.js";
import { messages } from "../stores/messages.svelte.js";
import { SessionsService } from "../api/generated/index";
import { copyToClipboard } from "../utils/clipboard.js";
import AppHeader from "../components/layout/AppHeader.svelte";
import SidebarToggleButton from "../components/layout/SidebarToggleButton.svelte";
import { registerShortcuts } from "./keyboard.js";
import { registerSessionList } from "./arrow-target.js";

vi.mock("../utils/clipboard.js", () => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
}));

function fireKey(key: string, opts: Partial<KeyboardEventInit> = {}) {
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
  let navigateUserPrompt: (delta: number) => void;
  let detachSessionList: (() => void) | undefined;

  beforeEach(() => {
    ui.activeModal = null;
    ui.selectedOrdinal = null;
    sessions.activeSessionId = null;
    sessions.sessions = [];
    starred.filterOnly = false;
    for (const id of starred.ids) {
      starred.unstar(id);
    }
    navigateMessage = vi.fn();
    navigateUserPrompt = vi.fn();
    cleanup = registerShortcuts({ navigateMessage, navigateUserPrompt });
    detachSessionList = undefined;
  });

  afterEach(() => {
    cleanup();
    detachSessionList?.();
    document.body.innerHTML = "";
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

  describe("Publish shortcut", () => {
    it("opens publish modal with the active session target", () => {
      sessions.activeSessionId = "sess-123";

      fireKey("p");

      expect(ui.activeModal).toBe("publish");
      expect(ui.publishSecret).toBe(false);
      expect(ui.publishTarget).toEqual({
        kind: "session",
        id: "sess-123",
      });
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

    it("should block prompt navigation when modal is open", () => {
      ui.activeModal = "commandPalette";
      fireKey("J", { shiftKey: true });
      expect(navigateUserPrompt).not.toHaveBeenCalled();
    });
  });

  describe("arrow target", () => {
    function mountSessionList(
      navigate = (delta: number) =>
        sessions.navigateSession(
          delta,
          starred.filterOnly ? (session) => starred.isStarred(session.id) : undefined,
        ),
    ) {
      const list = document.createElement("div");
      list.className = "session-list-scroll";
      document.body.appendChild(list);
      detachSessionList = registerSessionList(list, navigate);
      return list;
    }

    it("navigates sessions up and down in the registered list", () => {
      const list = mountSessionList();
      sessions.sessions = [{ id: "s1" } as any, { id: "s2" } as any];
      sessions.activeSessionId = "s1";
      const row = document.createElement("button");
      list.appendChild(row);
      row.focus();
      fireKey("ArrowDown");
      expect(sessions.activeSessionId).toBe("s2");
      fireKey("ArrowUp");
      expect(sessions.activeSessionId).toBe("s1");
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("switches panes after deliberate pointer interaction, not hover", () => {
      const navigateSessions = vi.fn();
      const list = mountSessionList(navigateSessions);
      const row = document.createElement("button");
      list.appendChild(row);
      row.focus();

      fireKey("ArrowDown");
      expect(navigateSessions).toHaveBeenCalledWith(1);

      const message = document.createElement("div");
      message.className = "message-list-scroll";
      document.body.appendChild(message);
      message.dispatchEvent(new Event("pointermove", { bubbles: true }));
      fireKey("ArrowDown");
      expect(navigateSessions).toHaveBeenCalledTimes(2);
      expect(navigateMessage).not.toHaveBeenCalled();

      message.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      expect(document.activeElement).toBe(row);

      fireKey("ArrowDown");
      expect(navigateSessions).toHaveBeenCalledTimes(2);
      expect(navigateMessage).toHaveBeenCalledWith(1);

      const unrelated = document.createElement("button");
      document.body.appendChild(unrelated);
      row.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      unrelated.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      fireKey("ArrowDown");
      expect(navigateSessions).toHaveBeenCalledTimes(3);
      expect(navigateMessage).toHaveBeenCalledTimes(1);
    });

    it("uses session navigation after a sidebar control interaction", () => {
      const navigateSessions = vi.fn();
      const sidebar = document.createElement("aside");
      sidebar.id = "session-sidebar";
      const filter = document.createElement("button");
      sidebar.appendChild(filter);
      document.body.appendChild(sidebar);
      const list = mountSessionList(navigateSessions);
      sidebar.appendChild(list);

      const messagePane = document.createElement("div");
      messagePane.className = "message-list-scroll";
      document.body.appendChild(messagePane);
      messagePane.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      filter.dispatchEvent(new Event("pointerdown", { bubbles: true }));

      fireKey("ArrowDown");

      expect(navigateSessions).toHaveBeenCalledWith(1);
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("keeps arrow navigation within the starred-only list", () => {
      const list = mountSessionList();
      sessions.sessions = [{ id: "s1" } as any, { id: "s2" } as any, { id: "s3" } as any];
      sessions.activeSessionId = "s1";
      starred.filterOnly = true;
      starred.ids = new Set(["s1", "s3"]);
      const row = document.createElement("button");
      list.appendChild(row);
      row.focus();

      fireKey("ArrowDown");
      expect(sessions.activeSessionId).toBe("s3");
      fireKey("ArrowUp");
      expect(sessions.activeSessionId).toBe("s1");
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it.each([
      ["direct/root", {}],
      ["subagent", { relationship_type: "subagent" }],
      ["forked child", { parent_session_id: "root", relationship_type: "fork" }],
      ["continuation", { parent_session_id: "root", relationship_type: "continuation" }],
      ["imported", { agent: "imported-agent" }],
      ["soft-deleted", { deleted_at: "2026-08-01T00:00:00Z" }],
      ["tombstoned", { tombstoned: true }],
    ])("preserves arrow routing for the %s Session lineage variant", (_name, variant) => {
      const list = mountSessionList();
      sessions.sessions = [{ id: "root" } as any, { id: "variant", ...variant } as any];
      sessions.activeSessionId = "root";
      const row = document.createElement("button");
      list.appendChild(row);
      row.focus();

      fireKey("ArrowDown");

      expect(sessions.activeSessionId).toBe("variant");
      expect(navigateMessage).not.toHaveBeenCalled();
    });

    it("keeps message fallback and native vetoes", () => {
      const list = mountSessionList();
      list.appendChild(document.createElement("button"));
      fireKey("ArrowDown");
      expect(navigateMessage).toHaveBeenCalledWith(1);

      const input = document.createElement("input");
      list.appendChild(input);
      input.focus();
      fireKey("ArrowDown");
      expect(navigateMessage).toHaveBeenCalledTimes(1);
      expect(sessions.activeSessionId).toBeNull();
    });

    it("does not route arrows from a dialog through the message fallback", () => {
      const list = mountSessionList();
      const dialog = document.createElement("div");
      dialog.setAttribute("role", "dialog");
      const button = document.createElement("button");
      dialog.appendChild(button);
      list.appendChild(dialog);
      button.focus();

      fireKey("ArrowDown");

      expect(navigateMessage).not.toHaveBeenCalled();
      expect(sessions.activeSessionId).toBeNull();
    });

    it("clears the list route when the component disconnects", () => {
      const list = mountSessionList();
      list.remove();
      fireKey("ArrowDown");
      expect(navigateMessage).toHaveBeenCalledWith(1);
    });

    it("routes arrows to messages after mobile auto-close hides the sidebar", () => {
      const navigateSessions = vi.fn();
      const sidebar = document.createElement("aside");
      document.body.appendChild(sidebar);
      const list = mountSessionList(navigateSessions);
      sidebar.appendChild(list);
      list.dispatchEvent(new Event("pointerdown", { bubbles: true }));

      sidebar.style.display = "none";
      fireKey("ArrowDown");

      expect(navigateSessions).not.toHaveBeenCalled();
      expect(navigateMessage).toHaveBeenCalledWith(1);
    });

    it("clears the list route when registration is unregistered", () => {
      const list = mountSessionList();
      detachSessionList?.();
      detachSessionList = undefined;
      list.appendChild(document.createElement("button"));
      fireKey("ArrowDown");
      expect(navigateMessage).toHaveBeenCalledWith(1);
      expect(sessions.activeSessionId).toBeNull();
    });
  });

  describe("prompt navigation", () => {
    it("dispatches Shift+J and Shift+K without changing adjacent navigation", () => {
      fireKey("J", { shiftKey: true });
      fireKey("K", { shiftKey: true });
      fireKey("j");
      fireKey("k");

      expect(navigateUserPrompt).toHaveBeenNthCalledWith(1, 1);
      expect(navigateUserPrompt).toHaveBeenNthCalledWith(2, -1);
      expect(navigateMessage).toHaveBeenNthCalledWith(1, 1);
      expect(navigateMessage).toHaveBeenNthCalledWith(2, -1);
    });

    it("suppresses prompt navigation while a focused input owns keys", () => {
      const input = document.createElement("input");
      document.body.appendChild(input);
      input.focus();
      try {
        fireKey("J", { shiftKey: true });
        expect(navigateUserPrompt).not.toHaveBeenCalled();
      } finally {
        document.body.removeChild(input);
      }
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
      sessions.sessions = [makeSession("s1"), makeSession("s2"), makeSession("s3")];
      sessions.activeSessionId = "s1";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = true;

      fireKey("]");

      // Should skip s2 (unstarred) and land on s3
      expect(sessions.activeSessionId).toBe("s3");
    });

    it("should navigate forward without filter when filterOnly is disabled", () => {
      sessions.sessions = [makeSession("s1"), makeSession("s2"), makeSession("s3")];
      sessions.activeSessionId = "s1";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = false;

      fireKey("]");

      // Should go to s2 (no filter applied)
      expect(sessions.activeSessionId).toBe("s2");
    });

    it("should navigate backward skipping unstarred when filterOnly is enabled", () => {
      sessions.sessions = [makeSession("s1"), makeSession("s2"), makeSession("s3")];
      sessions.activeSessionId = "s3";
      starred.star("s1");
      starred.star("s3");
      starred.filterOnly = true;

      fireKey("[");

      // Should skip s2 (unstarred) and land on s1
      expect(sessions.activeSessionId).toBe("s1");
    });

    it("should be a no-op when filtered list is empty", () => {
      sessions.sessions = [makeSession("s1"), makeSession("s2")];
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

    it("moves focus out of the sidebar when the shortcut collapses it", async () => {
      router.navigate("sessions");
      ui.isMobileViewport = false;
      ui.sidebarOpen = true;

      const sidebar = document.createElement("aside");
      sidebar.id = "session-sidebar";
      const focusedControl = document.createElement("button");
      focusedControl.textContent = "Focused sidebar control";
      sidebar.appendChild(focusedControl);
      document.body.appendChild(sidebar);
      const contentToggle = mount(SidebarToggleButton, {
        target: document.body,
        props: { placement: "content" },
      });

      try {
        focusedControl.focus();
        fireKey("b");
        await tick();

        const openButton = document.querySelector<HTMLButtonElement>(
          'button[aria-label="Open sidebar"]',
        );
        expect(ui.sidebarOpen).toBe(false);
        expect(openButton).not.toBeNull();
        await vi.waitFor(() => {
          expect(document.activeElement).toBe(openButton);
        });
      } finally {
        await unmount(contentToggle);
        sidebar.remove();
      }
    });

    it("moves mobile focus from the closing drawer to the hamburger", async () => {
      router.navigate("sessions");
      ui.isMobileViewport = true;
      ui.sidebarOpen = true;

      const sidebar = document.createElement("aside");
      sidebar.id = "session-sidebar";
      const focusedControl = document.createElement("button");
      focusedControl.textContent = "Focused mobile drawer control";
      sidebar.appendChild(focusedControl);
      document.body.appendChild(sidebar);
      const header = mount(AppHeader, { target: document.body });

      try {
        focusedControl.focus();
        fireKey("b");
        await tick();

        const hamburger = document.querySelector<HTMLButtonElement>(
          'button[aria-label="Toggle sidebar"]',
        );
        expect(ui.sidebarOpen).toBe(false);
        expect(hamburger).not.toBeNull();
        await vi.waitFor(() => {
          expect(document.activeElement).toBe(hamburger);
        });
      } finally {
        await unmount(header);
        sidebar.remove();
        ui.isMobileViewport = false;
      }
    });

    it("should navigate to sessions on non-session routes when mobile", () => {
      router.navigate("quality");
      ui.isMobileViewport = true;
      ui.sidebarOpen = false;
      fireKey("b");
      expect(router.route).toBe("sessions");
      expect(ui.sidebarOpen).toBe(true);
      ui.isMobileViewport = false;
    });

    it("should toggle sidebar on non-session routes when desktop", () => {
      router.navigate("quality");
      ui.isMobileViewport = false;
      ui.sidebarOpen = true;
      fireKey("b");
      expect(ui.sidebarOpen).toBe(false);
      expect(router.route).toBe("quality");
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

  it("pins the active session model in the resume fallback", async () => {
    const session = {
      id: "run:keyboard-session",
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
    sessions.sessions = [session];
    sessions.activeSessionId = session.id;
    messages.sessionId = session.id;
    messages.historyComplete = true;
    messages.messages = [
      {
        id: 1,
        session_id: session.id,
        ordinal: 0,
        role: "assistant",
        content: "answer",
        timestamp: "",
        has_thinking: false,
        thinking_text: "",
        has_tool_use: false,
        content_length: 6,
        model: "claude sonnet",
        token_usage: null,
        context_tokens: 0,
        output_tokens: 0,
        has_context_tokens: false,
        has_output_tokens: false,
        is_system: false,
      },
    ];
    vi.spyOn(SessionsService, "postApiV1SessionsByIdResume").mockRejectedValue(
      new Error("backend unavailable"),
    );

    fireKey("c");
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith(
        "claude --resume 'run:keyboard-session' --model 'claude sonnet'",
      );
    });
    messages.clear();
  });

  it("keeps successful backend resume commands authoritative", async () => {
    const session = {
      id: "run:keyboard-session",
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
    sessions.sessions = [session];
    sessions.activeSessionId = session.id;
    messages.sessionId = session.id;
    messages.messages = [
      {
        id: 1,
        session_id: session.id,
        ordinal: 0,
        role: "assistant",
        content: "answer",
        timestamp: "",
        has_thinking: false,
        thinking_text: "",
        has_tool_use: false,
        content_length: 6,
        model: "claude sonnet",
        token_usage: null,
        context_tokens: 0,
        output_tokens: 0,
        has_context_tokens: false,
        has_output_tokens: false,
        is_system: false,
      },
    ];
    vi.spyOn(SessionsService, "postApiV1SessionsByIdResume").mockResolvedValue({
      launched: false,
      command: "claude --resume run:keyboard-session",
      cwd: "/tmp/project",
    });

    fireKey("c");
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume run:keyboard-session");
    });
    messages.clear();
  });

  it("does not pin a partial-history model in the resume fallback", async () => {
    const session = {
      id: "run:keyboard-session",
      project: "proj",
      machine: "local",
      agent: "claude",
      first_message: null,
      started_at: null,
      ended_at: null,
      message_count: 3001,
      user_message_count: 1,
      total_output_tokens: 0,
      peak_context_tokens: 0,
      is_automated: false,
      created_at: "2024-01-01T00:00:00Z",
    };
    sessions.sessions = [session];
    sessions.activeSessionId = session.id;
    messages.sessionId = session.id;
    messages.messages = [
      {
        id: 1,
        session_id: session.id,
        ordinal: 0,
        role: "assistant",
        content: "answer",
        timestamp: "",
        has_thinking: false,
        thinking_text: "",
        has_tool_use: false,
        content_length: 6,
        model: "claude sonnet",
        token_usage: null,
        context_tokens: 0,
        output_tokens: 0,
        has_context_tokens: false,
        has_output_tokens: false,
        is_system: false,
      },
    ];
    messages.hasOlder = true;
    vi.spyOn(SessionsService, "postApiV1SessionsByIdResume").mockRejectedValue(
      new Error("backend unavailable"),
    );

    fireKey("c");
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume 'run:keyboard-session'");
    });
    messages.clear();
  });

  it("does not pin a reloading stable model in the resume fallback", async () => {
    const session = {
      id: "run:keyboard-session",
      project: "proj",
      machine: "local",
      agent: "claude",
      first_message: null,
      started_at: null,
      ended_at: null,
      message_count: 3001,
      user_message_count: 1,
      total_output_tokens: 0,
      peak_context_tokens: 0,
      is_automated: false,
      created_at: "2024-01-01T00:00:00Z",
    };
    sessions.sessions = [session];
    sessions.activeSessionId = session.id;
    messages.sessionId = session.id;
    messages.loading = true;
    (messages as any)._stableMainModel = "claude sonnet";
    vi.spyOn(SessionsService, "postApiV1SessionsByIdResume").mockRejectedValue(
      new Error("backend unavailable"),
    );

    fireKey("c");
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume 'run:keyboard-session'");
    });
    messages.clear();
  });
});
