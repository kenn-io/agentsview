// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vite-plus/test";
import { mount, unmount, tick } from "svelte";
import { ApiError } from "../../api/runtime.js";
import { registerShortcuts } from "../../utils/keyboard.js";

const {
  mockUi,
  mockSessions,
  mockSearchStore,
  mockRouter,
  mockCopyToClipboard,
  mockEmbeddingsService,
} = vi.hoisted(
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
      activeSessionId: null as string | null,
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
      deselectSession: vi.fn(),
    },
    mockSearchStore: {
      results: [] as Array<unknown>,
      isSearching: false,
      error: null as {
        detail: string | null;
        kind: "generic" | "timeout" | "semantic-unavailable";
      } | null,
      mode: "fulltext" as "fulltext" | "semantic" | "hybrid",
      sort: "relevance" as "relevance" | "recency",
      search: vi.fn(),
      clear: vi.fn(),
      resetSort: vi.fn(),
      retry: vi.fn(),
      setMode: vi.fn(),
      setSort: vi.fn(),
    },
    mockRouter: {
      navigateToSession: vi.fn(),
    },
    mockCopyToClipboard: vi.fn(),
    mockEmbeddingsService: {
      getApiV1EmbeddingsStatus: vi.fn(),
      postApiV1EmbeddingsBuild: vi.fn(),
    },
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

// SemanticSetupHelp (mounted for the semantic-unavailable error kind) probes
// the embeddings status on mount; keep the probe pending so palette tests
// exercise the wiring without embeddings API behavior (covered in
// SemanticSetupHelp.test.ts).
vi.mock("../../api/generated/index.js", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../../api/generated/index.js")>();
  return {
    ...actual,
    EmbeddingsService: mockEmbeddingsService,
  };
});

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

function makeSearchResult(
  overrides: Record<string, unknown> = {},
) {
  return {
    session_id: "codex:search123",
    project: "proj-a",
    agent: "codex",
    ordinal: 7,
    timestamp: "2026-01-01T00:00:00Z",
    snippet: "matching content",
    rank: 0,
    snippetFormat: "highlighted-html",
    ...overrides,
  };
}

async function enterSearchQuery(value = "match") {
  const input = document.querySelector<HTMLInputElement>(".palette-input")!;
  input.value = value;
  input.dispatchEvent(new InputEvent("input", { bubbles: true }));
  await tick();
}

describe("CommandPalette", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    document.body.replaceChildren();
    // jsdom does not implement scrollIntoView
    Element.prototype.scrollIntoView = vi.fn();
    mockSearchStore.results = [];
    mockSearchStore.isSearching = false;
    mockSearchStore.error = null;
    mockSearchStore.mode = "fulltext";
    mockSearchStore.sort = "relevance";
    mockEmbeddingsService.getApiV1EmbeddingsStatus.mockReset();
    mockEmbeddingsService.getApiV1EmbeddingsStatus.mockImplementation(
      () => new Promise(() => {}),
    );
    mockEmbeddingsService.postApiV1EmbeddingsBuild.mockReset();
    mockSessions.activeSessionId = null;
    mockSessions.deselectSession.mockImplementation(() => {
      mockSessions.activeSessionId = null;
    });
    mockUi.activeModal = "commandPalette";
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
      makeSearchResult({
        session_id: "codex:abc123def456",
        project: "test-proj",
        ordinal: 0,
        snippet: "some matching text",
      }),
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    await enterSearchQuery("abc");

    const badge = await tickUntil(".item-id");
    expect(badge.textContent?.trim()).toBe("abc123de");

    badge.click();
    await tick();

    expect(mockCopyToClipboard).toHaveBeenCalledWith("codex:abc123def456");

    unmount(component);
  });

  it("omits relative-time segment when timestamp is empty", async () => {
    mockSearchStore.results = [
      makeSearchResult({
        session_id: "codex:emptytime123",
        project: "my-proj",
        ordinal: 0,
        timestamp: "",
        snippet: "some text",
      }),
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    await enterSearchQuery("abc");

    const meta = await tickUntil(".item-meta");
    // Should show project but no " · <time>" segment.
    expect(meta.textContent?.trim()).toBe("my-proj");

    unmount(component);
  });

  it("name-only result (ordinal === -1) hydrates session and clears selection without scrolling", async () => {
    mockSearchStore.results = [
      makeSearchResult({
        session_id: "claude:nameonly123",
        project: "proj-a",
        agent: "claude",
        name: "nameonly match",
        ordinal: -1,
        snippet: "",
      }),
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    await enterSearchQuery("nameonly");

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
      makeSearchResult(),
    ];

    const component = mount(CommandPalette, { target: document.body });
    await tick();

    await enterSearchQuery();

    const item = await tickUntil(".palette-item");
    item.click();
    await tick();

    expect(mockSessions.selectSession).not.toHaveBeenCalled();
    expect(mockSessions.navigateToSession).toHaveBeenCalledWith("codex:search123");
    expect(mockUi.scrollToOrdinal).toHaveBeenCalledWith(7, "codex:search123");
    expect(mockRouter.navigateToSession).toHaveBeenCalledWith("codex:search123");

    unmount(component);
  });

  it("always renders localized search modes below the input", async () => {
    const component = mount(CommandPalette, { target: document.body });
    await tick();

    const inputWrap = document.querySelector(".palette-input-wrap")!;
    const controls = document.querySelector<HTMLElement>(".palette-controls")!;
    const group = controls.querySelector<HTMLElement>(
      '[role="radiogroup"][aria-label="Search mode"]',
    );
    const radios = Array.from(
      controls.querySelectorAll<HTMLElement>('[role="radio"]'),
    );

    expect(inputWrap.nextElementSibling).toBe(controls);
    expect(group).not.toBeNull();
    expect(radios.map((radio) => radio.textContent?.trim())).toEqual([
      "Full text",
      "Semantic",
      "Hybrid",
    ]);
    expect(radios[0]?.getAttribute("aria-checked")).toBe("true");
    expect(controls.querySelector(".palette-sort")).toBeNull();

    unmount(component);
  });

  it("mode controls handle activation and arrows without activating results", async () => {
    mockSearchStore.error = {
      detail: "temporarily unavailable",
      kind: "generic",
    };
    mockSearchStore.results = [makeSearchResult()];
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const controls = document.querySelector<HTMLElement>(".palette-controls")!;
    const radios = Array.from(
      controls.querySelectorAll<HTMLButtonElement>('[role="radio"]'),
    );
    radios[1]?.click();
    radios[0]?.dispatchEvent(
      new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }),
    );
    for (const key of ["Enter", " "]) {
      radios[0]?.dispatchEvent(
        new KeyboardEvent("keydown", { key, bubbles: true }),
      );
    }
    await tick();

    expect(mockSearchStore.setMode).toHaveBeenNthCalledWith(1, "semantic");
    expect(mockSearchStore.setMode).toHaveBeenNthCalledWith(2, "semantic");
    expect(mockSearchStore.retry).not.toHaveBeenCalled();
    expect(mockSessions.navigateToSession).not.toHaveBeenCalled();
    expect(mockRouter.navigateToSession).not.toHaveBeenCalled();

    radios[0]?.dispatchEvent(
      new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
    );
    await tick();
    expect(mockUi.activeModal).toBeNull();

    unmount(component);
  });

  it.each([
    ["semantic", "click"],
    ["semantic", "Enter"],
    ["semantic", " "],
    ["hybrid", "click"],
    ["hybrid", "Enter"],
    ["hybrid", " "],
  ] as const)(
    "retries an errored active %s mode once on %s without navigating",
    async (mode, activation) => {
      mockSearchStore.mode = mode;
      mockSearchStore.error = {
        detail: "temporarily unavailable",
        kind: "generic",
      };
      mockSearchStore.results = [makeSearchResult()];
      const component = mount(CommandPalette, { target: document.body });
      await enterSearchQuery();

      const activeMode = document.querySelector<HTMLButtonElement>(
        '.palette-controls [role="radio"][aria-checked="true"]',
      )!;
      if (activation === "click") {
        activeMode.click();
      } else {
        activeMode.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: activation,
            bubbles: true,
            cancelable: true,
          }),
        );
      }
      await tick();

      expect(mockSearchStore.retry).toHaveBeenCalledOnce();
      expect(mockSearchStore.setMode).not.toHaveBeenCalled();
      expect(mockSessions.navigateToSession).not.toHaveBeenCalled();
      expect(mockRouter.navigateToSession).not.toHaveBeenCalled();

      unmount(component);
    },
  );

  it("shows sort only for full-text query results and activates it through the rendered button", async () => {
    mockSearchStore.results = [makeSearchResult()];
    const fullText = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const recency = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".palette-sort button"),
    ).find((button) => button.textContent?.trim() === "Recency");
    expect(recency).toBeDefined();
    expect(recency?.closest(".palette-controls")).not.toBeNull();
    recency?.click();
    expect(mockSearchStore.setSort).toHaveBeenCalledWith("recency");
    unmount(fullText);

    mockSearchStore.mode = "semantic";
    const semantic = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();
    expect(document.querySelector(".palette-sort")).toBeNull();
    unmount(semantic);
  });

  it("renders loading before error, empty state, and results", async () => {
    mockSearchStore.isSearching = true;
    mockSearchStore.error = { detail: "backend detail", kind: "generic" };
    mockSearchStore.results = [makeSearchResult()];
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    expect(document.querySelector(".palette-empty")?.textContent).toContain("Searching");
    expect(document.querySelector(".palette-error")).toBeNull();
    expect(document.querySelector(".palette-item")).toBeNull();

    unmount(component);
  });

  it("renders the localized error heading and exact backend detail before results", async () => {
    mockSearchStore.error = {
      detail: "Run agentsview embeddings build --full-rebuild",
      kind: "generic",
    };
    mockSearchStore.results = [makeSearchResult()];
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const error = document.querySelector(".palette-error");
    expect(error?.querySelector("strong")?.textContent).toBe("Search unavailable");
    expect(error?.querySelector("span")?.textContent).toBe(
      "Run agentsview embeddings build --full-rebuild",
    );
    expect(document.querySelector(".palette-item")).toBeNull();

    unmount(component);
  });

  it("renders localized fallback copy for an error without string detail", async () => {
    mockSearchStore.error = { detail: null, kind: "generic" };
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const error = document.querySelector(".palette-error");
    expect(error?.querySelector("strong")?.textContent).toBe("Search unavailable");
    expect(error?.querySelector("span")?.textContent).toBe(
      "Search failed. Please try again.",
    );

    unmount(component);
  });

  it("explains a semantic timeout and offers an explicit retry", async () => {
    mockSearchStore.mode = "semantic";
    mockSearchStore.error = {
      detail: "request timed out",
      kind: "timeout",
    };
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const error = document.querySelector(".palette-error");
    expect(error?.querySelector("strong")?.textContent).toBe(
      "Search took too long",
    );
    expect(error?.querySelector("span")?.textContent).toBe(
      "Try again. The first Semantic or Hybrid search can be slower while the embedding model warms up.",
    );
    expect(error?.textContent).not.toContain("request timed out");

    const retry = Array.from(
      error?.querySelectorAll<HTMLButtonElement>("button") ?? [],
    ).find((button) => button.textContent?.trim() === "Retry");
    expect(retry).toBeDefined();
    retry?.click();
    expect(mockSearchStore.retry).toHaveBeenCalledOnce();
    expect(mockRouter.navigateToSession).not.toHaveBeenCalled();

    unmount(component);
  });

  it("renders the semantic setup panel instead of the raw error for semantic-unavailable", async () => {
    mockSearchStore.mode = "semantic";
    mockSearchStore.error = {
      detail:
        "semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'",
      kind: "semantic-unavailable",
    };
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    expect(document.querySelector(".semantic-setup")).not.toBeNull();
    expect(document.querySelector(".palette-error")).toBeNull();

    unmount(component);
  });

  it.each([
    [
      "Build",
      () => mockEmbeddingsService.getApiV1EmbeddingsStatus.mockResolvedValue({
        running: false,
        done: 0,
        total: 0,
        eta_milliseconds: 0,
      }),
      "button",
      "Build embeddings",
    ],
    [
      "Retry",
      () => mockEmbeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
        new Error("status probe failed"),
      ),
      "button",
      "Retry",
    ],
    [
      "Copy",
      () => mockEmbeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
        new ApiError(501, "embeddings manager not available"),
      ),
      "button.kit-copy-btn",
      "",
    ],
  ] as const)(
    "does not cancel Enter on the semantic setup %s control",
    async (_name, arrangeStatus, selector, label) => {
      arrangeStatus();
      mockSearchStore.mode = "semantic";
      mockSearchStore.error = {
        detail:
          "semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'",
        kind: "semantic-unavailable",
      };
      const component = mount(CommandPalette, { target: document.body });
      await enterSearchQuery();

      const controls = await tickUntil(".semantic-setup");
      const control = Array.from(
        controls.querySelectorAll<HTMLButtonElement>(selector),
      ).find((button) => !label || button.textContent?.includes(label));
      expect(control).toBeDefined();
      control!.focus();
      expect(document.activeElement).toBe(control);

      const enter = new KeyboardEvent("keydown", {
        key: "Enter",
        bubbles: true,
        cancelable: true,
      });
      const allowed = control!.dispatchEvent(enter);

      expect(allowed).toBe(true);
      expect(enter.defaultPrevented).toBe(false);

      control!.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Escape",
          bubbles: true,
          cancelable: true,
        }),
      );
      expect(mockUi.activeModal).toBeNull();

      unmount(component);
    },
  );

  it("stops setup Escape before global shortcuts deselect the active session", async () => {
    mockEmbeddingsService.getApiV1EmbeddingsStatus.mockRejectedValue(
      new ApiError(501, "embeddings manager not available"),
    );
    mockSearchStore.mode = "semantic";
    mockSearchStore.error = {
      detail:
        "semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'",
      kind: "semantic-unavailable",
    };
    mockSessions.activeSessionId = "session-1";
    const cleanupShortcuts = registerShortcuts({
      navigateMessage: vi.fn(),
      navigateUserPrompt: vi.fn(),
    });
    const component = mount(CommandPalette, { target: document.body });

    try {
      await enterSearchQuery();
      const copy = await tickUntil(".semantic-setup button.kit-copy-btn");
      copy.focus();
      copy.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Escape",
          bubbles: true,
          cancelable: true,
        }),
      );

      expect(mockUi.activeModal).toBeNull();
      expect(mockSessions.activeSessionId).toBe("session-1");
      expect(mockSessions.deselectSession).not.toHaveBeenCalled();
    } finally {
      cleanupShortcuts();
      unmount(component);
    }
  });

  it("renders empty and result states when no higher-priority state applies", async () => {
    const empty = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();
    expect(document.querySelector(".palette-empty")?.textContent).toContain("No results");
    unmount(empty);

    mockSearchStore.results = [makeSearchResult({ snippet: "visible result" })];
    const results = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();
    expect(document.querySelector(".item-snippet")?.textContent).toBe("visible result");
    expect(document.querySelector(".palette-empty")).toBeNull();
    unmount(results);
  });

  it("renders semantic snippets as literal text without creating HTML elements", async () => {
    mockSearchStore.mode = "semantic";
    mockSearchStore.results = [
      makeSearchResult({
        snippet: "<img src=x onerror=alert(1)>",
        snippetFormat: "plain-text",
      }),
    ];
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const snippet = document.querySelector(".item-snippet");
    expect(snippet?.textContent).toBe("<img src=x onerror=alert(1)>");
    expect(snippet?.querySelector("img")).toBeNull();

    unmount(component);
  });

  it("preserves sanitized full-text mark highlighting", async () => {
    mockSearchStore.results = [
      makeSearchResult({
        snippet: "before <mark>needle</mark> after",
        snippetFormat: "highlighted-html",
      }),
    ];
    const component = mount(CommandPalette, { target: document.body });
    await enterSearchQuery();

    const mark = document.querySelector(".item-snippet mark");
    expect(mark?.textContent).toBe("needle");

    unmount(component);
  });
});
