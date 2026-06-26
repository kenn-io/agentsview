// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vite-plus/test";
import { mount, unmount, tick } from "svelte";

const { mockUi, mockSessions, mockSearchStore, mockRouter, mockCopyToClipboard } = vi.hoisted(
  () => ({
    mockUi: {
      activeModal: "commandPalette" as
        | "commandPalette"
        | null,
      scrollToOrdinal: vi.fn(),
      clearSelection: vi.fn(),
      clearScrollState: vi.fn(),
    },
    mockSessions: {
      sessions: [] as Array<{
        id: string;
        project: string;
        machine: string;
        agent: string;
        first_message: string | null;
        started_at: string | null;
        ended_at: string | null;
        message_count: number;
        user_message_count: number;
        created_at: string;
      }>,
      filters: { project: "" },
      selectSession: vi.fn(),
      navigateToSession: vi.fn(),
    },
    mockSearchStore: {
      results: [] as Array<unknown>,
      isSearching: false,
      sort: "relevance" as "relevance" | "recency",
      search: vi.fn(),
      clear: vi.fn(),
      resetSort: vi.fn(),
      setSort: vi.fn(),
    },
    mockRouter: {
      navigateToSession: vi.fn(),
    },
    mockCopyToClipboard: vi.fn(),
  }),
);

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: mockUi,
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: mockSessions,
}));

vi.mock("../../stores/search.svelte.js", () => ({
  searchStore: mockSearchStore,
}));

vi.mock("../../stores/router.svelte.js", () => ({
  router: mockRouter,
}));

vi.mock("../../stores/messages.svelte.js", () => ({
  messages: {},
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: mockCopyToClipboard,
}));

// @ts-ignore
import CommandPalette from "./CommandPalette.svelte";

/**
 * Polls via tick() until the selector matches or the iteration limit is hit.
 * Svelte 5's microtask scheduler requires explicit tick() calls to flush DOM
 * updates in jsdom — setTimeout-based waitFor() retries don't drive it.
 */
async function tickUntil(
  selector: string,
  maxTicks = 20,
): Promise<HTMLElement> {
  for (let i = 0; i < maxTicks; i++) {
    await tick();
    const el = document.querySelector<HTMLElement>(selector);
    if (el) return el;
  }
  throw new Error(
    `"${selector}" not found after ${maxTicks} tick() calls`,
  );
}

function makeSession(id: string, agent: string) {
  return {
    id,
    project: "proj-a",
    machine: "mac",
    agent,
    first_message: "hello",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    created_at: "2026-02-20T12:30:00Z",
  };
}

describe("CommandPalette", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // jsdom does not implement scrollIntoView
    Element.prototype.scrollIntoView = vi.fn();
    mockSearchStore.results = [];
    mockSearchStore.isSearching = false;
    mockSessions.filters.project = "";
    mockSessions.sessions = [
      makeSession("s1", "cursor"),
      makeSession("s2", "unknown"),
    ];
  });

  it("uses agentColor for recent-session dots including fallback", async () => {
    const component = mount(CommandPalette, {
      target: document.body,
    });

    await tick();

    const dots = Array.from(
      document.querySelectorAll<HTMLElement>(".item-dot"),
    );
    expect(dots).toHaveLength(2);
    expect(dots[0]?.getAttribute("style")).toContain(
      "var(--accent-black)",
    );
    expect(dots[1]?.getAttribute("style")).toContain(
      "var(--accent-blue)",
    );

    unmount(component);
  });

  it("calls clear() and resetSort() on unmount via onDestroy", async () => {
    const component = mount(CommandPalette, {
      target: document.body,
    });

    await tick();

    unmount(component);

    expect(mockSearchStore.clear).toHaveBeenCalledOnce();
    expect(mockSearchStore.resetSort).toHaveBeenCalledOnce();
  });

  it("copies canonical session_id to clipboard, not stripped display ID", async () => {
    // Prefixed ID: stripIdPrefix("codex:abc123def456", "codex") → "abc123def456"
    // Display shows first 8 chars: "abc123de"
    // Copy must use the full canonical "codex:abc123def456"
    mockSearchStore.results = [
      {
        session_id: "codex:abc123def456",
        project: "test-proj",
        agent: "codex",
        ordinal: 0,
        session_ended_at: "2026-01-01T00:00:00Z",
        snippet: "some matching text",
        rank: 0,
      },
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    // Type 3+ chars so showSearchResults becomes true.
    const input = document.querySelector<HTMLInputElement>(".palette-input")!;
    input.value = "abc";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));

    const badge = await tickUntil(".item-id");
    expect(badge.textContent?.trim()).toBe("abc123de");

    badge.click();
    await tick();

    expect(mockCopyToClipboard).toHaveBeenCalledWith("codex:abc123def456");

    unmount(component);
  });

  it("omits relative-time segment when session_ended_at is empty", async () => {
    mockSearchStore.results = [
      {
        session_id: "codex:emptytime123",
        project: "my-proj",
        agent: "codex",
        ordinal: 0,
        session_ended_at: "",
        snippet: "some text",
        rank: 0,
      },
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    const input = document.querySelector<HTMLInputElement>(".palette-input")!;
    input.value = "abc";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));

    const meta = await tickUntil(".item-meta");
    // Should show project but no " · <time>" segment.
    expect(meta.textContent?.trim()).toBe("my-proj");

    unmount(component);
  });

  it("name-only result (ordinal === -1) hydrates session and clears selection without scrolling", async () => {
    mockSearchStore.results = [
      {
        session_id: "claude:nameonly123",
        project: "proj-a",
        agent: "claude",
        name: "nameonly match",
        ordinal: -1,
        session_ended_at: "2026-01-01T00:00:00Z",
        snippet: "",
        rank: 0,
      },
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    const input = document.querySelector<HTMLInputElement>(".palette-input")!;
    input.value = "nameonly";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));

    const item = await tickUntil(".palette-item");
    item.click();
    await tick();

    expect(mockSessions.selectSession).not.toHaveBeenCalled();
    expect(mockSessions.navigateToSession).toHaveBeenCalledWith("claude:nameonly123");
    expect(mockRouter.navigateToSession).toHaveBeenCalledWith("claude:nameonly123");
    expect(mockUi.scrollToOrdinal).not.toHaveBeenCalled();
    expect(mockUi.clearScrollState).toHaveBeenCalled();

    unmount(component);
  });

  it("does not reselect typed text after each keystroke (#795)", async () => {
    const component = mount(CommandPalette, { target: document.body });
    await tick();

    const input = document.querySelector<HTMLInputElement>(".palette-input")!;

    function typeChar(char: string) {
      const start = input.selectionStart ?? input.value.length;
      const end = input.selectionEnd ?? input.value.length;
      input.value =
        input.value.slice(0, start) + char + input.value.slice(end);
      input.setSelectionRange(start + 1, start + 1);
      input.dispatchEvent(
        new InputEvent("input", { bubbles: true, inputType: "insertText", data: char }),
      );
    }

    typeChar("a");
    await tick();

    expect(input.value).toBe("a");
    expect(input.selectionStart).toBe(1);
    expect(input.selectionEnd).toBe(1);

    typeChar("b");
    await tick();

    expect(input.value).toBe("ab");
    expect(input.selectionStart).toBe(2);
    expect(input.selectionEnd).toBe(2);

    unmount(component);
  });

  it("search result click navigates to the session route", async () => {
    mockSearchStore.results = [
      {
        session_id: "codex:search123",
        project: "proj-a",
        agent: "codex",
        ordinal: 7,
        session_ended_at: "2026-01-01T00:00:00Z",
        snippet: "matching content",
        rank: 0,
      },
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    const input = document.querySelector<HTMLInputElement>(".palette-input")!;
    input.value = "match";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));

    const item = await tickUntil(".palette-item");
    item.click();
    await tick();

    expect(mockSessions.selectSession).not.toHaveBeenCalled();
    expect(mockSessions.navigateToSession).toHaveBeenCalledWith("codex:search123");
    expect(mockUi.scrollToOrdinal).toHaveBeenCalledWith(7, "codex:search123");
    expect(mockRouter.navigateToSession).toHaveBeenCalledWith("codex:search123");

    unmount(component);
  });
});
