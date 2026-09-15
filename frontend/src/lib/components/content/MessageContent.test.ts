// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount, type ComponentProps } from "svelte";
import type { Message, Session } from "../../api/types.js";
import type { SessionTiming } from "../../api/types/timing.js";
import { setLocale } from "../../i18n/index.js";
import MessageContent from "./MessageContent.svelte";

const timingState = vi.hoisted(() => ({ timing: null as SessionTiming | null }));
vi.mock("../../stores/sessionTiming.svelte.js", () => ({ sessionTiming: timingState }));

const copyMock = vi.hoisted(() => vi.fn().mockResolvedValue(true));
const mermaidMock = vi.hoisted(() => vi.fn(() => ({ renderNow: vi.fn(), disconnect: vi.fn() })));
const forkMock = vi.hoisted(() => vi.fn());
const state = vi.hoisted(() => ({
  sessions: [] as Session[],
  activeSession: null as Session | null,
  readOnly: false,
  remote: false,
  searching: false,
}));
vi.mock("../../stores/messages.svelte.js", () => ({ messages: { sessionId: "", mainModel: "" } }));
const uiState = vi.hoisted(() => {
  const hidden = new Set<string>();
  return {
    hidden,
    isBlockVisible: (type: string) => !hidden.has(type),
    hideBlock: (type: string) => hidden.add(type),
    showAllBlocks: () => hidden.clear(),
  };
});

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: uiState,
}));

vi.mock("../../stores/pins.svelte.js", () => ({
  pins: { isPinned: () => false, togglePin: vi.fn().mockResolvedValue(undefined) },
}));
vi.mock("../../stores/sessions.svelte.js", () => ({ sessions: state }));
vi.mock("../../stores/sync.svelte.js", () => ({ sync: state }));
vi.mock("../../stores/inSessionSearch.svelte.js", () => ({
  inSessionSearch: {
    get isActive() {
      return state.searching;
    },
    get debouncedQuery() {
      return state.searching ? "SearchTarget" : "";
    },
    navigationRevision: 0,
    isCurrentBlock: () => false,
    countForBlock: () => 0,
    currentOccurrence: () => -1,
  },
}));
vi.mock("../../api/runtime.js", () => ({ isRemoteConnection: () => state.remote }));
vi.mock("../../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/index")>()),
  SessionsService: { postApiV1SessionsByIdResume: forkMock },
}));
vi.mock("../../utils/clipboard.js", () => ({ copyToClipboard: copyMock }));
vi.mock("@kenn-io/kit-ui/utils/markdown-mermaid", () => ({
  mermaidCodeFence: (code: string, lang: string) => {
    if (lang !== "mermaid") return undefined;
    const pre = document.createElement("pre");
    pre.className = "mermaid";
    pre.textContent = code;
    return pre.outerHTML;
  },
  initMarkdownMermaidRendering: mermaidMock,
}));
const components: ReturnType<typeof mount>[] = [];
let nextId = 220000;
function message(overrides: Partial<Message> = {}): Message {
  const content = overrides.content ?? "Token summary";
  return {
    id: nextId++,
    session_id: "session-1",
    ordinal: 0,
    role: "assistant",
    content,
    timestamp: "2026-02-20T12:30:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: content.length,
    model: "claude-sonnet",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}
function session(overrides: Partial<Session> = {}): Session {
  return {
    id: "session-1",
    agent: "claude",
    project: "proj-a",
    machine: "test",
    first_message: "hello",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 3,
    user_message_count: 2,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2026-02-20T12:30:00Z",
    ...overrides,
  } as Session;
}
async function render(
  source = message(),
  props: Partial<ComponentProps<typeof MessageContent>> = {},
) {
  components.push(
    mount(MessageContent, { target: document.body, props: { message: source, ...props } }),
  );
  await tick();
}
async function click(selector: string) {
  const button = document.querySelector<HTMLButtonElement>(selector);
  expect(button).not.toBeNull();
  button!.click();
  await Promise.resolve();
  await tick();
}
const text = (selector: string) => document.querySelector(selector)?.textContent?.trim() ?? "";
beforeEach(() => {
  forkMock.mockReset();
  setLocale("en");
});
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  document.body.replaceChildren();
  timingState.timing = null;
  setLocale("en");
  vi.clearAllMocks();
  state.sessions = [];
  state.activeSession = null;
  state.readOnly = false;
  state.remote = false;
  state.searching = false;
  uiState.showAllBlocks();
});

describe("MessageContent", () => {
  it("omits duration for legacy calls without stored timing", async () => {
    await render(message({ content: "[Bash]\npwd", has_tool_use: true }));
    expect(document.querySelector(".tool-duration")).toBeNull();
  });

  it.each([
    { duration: 2000, running: false, label: "2.0s" },
    { duration: 0, running: false, label: "0ms" },
    { duration: null, running: false, label: "unknown" },
    { duration: null, running: true, label: "running" },
    { duration: null, running: true, turnDurationMs: 5000, label: "unknown" },
  ])(
    "uses the call evidence for $label, running=$running",
    async ({ duration, running, turnDurationMs, label }) => {
      timingState.timing = {
        session_id: "session-1",
        total_duration_ms: 6000,
        tool_duration_ms: duration ?? 0,
        turn_count: 1,
        tool_call_count: 1,
        subagent_count: 0,
        slowest_call: null,
        by_category: [],
        activity: [],
        activity_totals: {
          thinking_ms: 0,
          generation_ms: 0,
          tool_ms: duration ?? 0,
          unattributed_ms: 6000 - (duration ?? 0),
        },
        running,
        turns: [
          {
            message_id: 1,
            ordinal: 0,
            started_at: "2026-02-20T12:30:00Z",
            duration_ms: turnDurationMs ?? (running ? null : 5000),
            primary_category: "Bash",
            calls: [
              {
                tool_use_id: "call-1",
                tool_name: "Bash",
                category: "Bash",
                duration_ms: duration,
                is_parallel: false,
                input_preview: "pwd",
              },
            ],
          },
        ],
      };
      await render(
        message({
          id: 1,
          content: "",
          has_tool_use: true,
          tool_calls: [
            { tool_use_id: "call-1", tool_name: "Bash", input_json: '{"command":"pwd"}' },
          ],
        }),
      );

      const actual = document.querySelector(".tool-duration")?.textContent?.trim();
      if (label === "running") expect(actual).toMatch(/^running /);
      else if (label === "unknown") expect(actual).toBeUndefined();
      else expect(actual).toBe(label);
    },
  );

  it.each([
    [
      "inline teammate",
      '<teammate-message teammate_id="t">reply</teammate-message>',
      {},
      false,
      "Teammate",
      "T",
    ],
    ["ordinary user", "Please summarize this.", {}, false, "User", "U"],
    [
      "teammate ancestry",
      "ordinary",
      { first_message: "<teammate-message>hello</teammate-message>" },
      false,
      "Teammate",
      "T",
    ],
    [
      "subagent ancestry",
      '<teammate-message teammate_id="t">reply</teammate-message>',
      { relationship_type: "subagent" },
      false,
      "Agent",
      "S",
    ],
    ["embedded subagent", "ordinary", {}, true, "Agent", "S"],
    [
      "quoted XML",
      '```xml\n<teammate-message teammate_id="t">reply</teammate-message>\n```',
      {},
      false,
      "User",
      "U",
    ],
  ] as const)(
    "keeps %s role and icon",
    async (_name, content, overrides, isSubagentContext, label, icon) => {
      state.sessions = [session(overrides)];
      await render(message({ role: "user", content }), { isSubagentContext });
      expect(text(".role-label")).toBe(label);
      expect(text(".role-icon")).toBe(icon);
    },
  );
  it("keeps differently classified rows separate in one document", async () => {
    state.sessions = [session(), session({ id: "child", relationship_type: "subagent" })];
    await render(
      message({
        role: "user",
        content: '<teammate-message teammate_id="t">reply</teammate-message>',
      }),
    );
    await render(message({ role: "user", content: "normal" }));
    await render(message({ role: "user", session_id: "child", content: "child" }));
    expect(
      Array.from(document.querySelectorAll(".role-label"), (node) => node.textContent?.trim()),
    ).toEqual(["Teammate", "User", "Agent"]);
  });
  it("localizes controls without translating user content", async () => {
    setLocale("zh-CN");
    await render(message({ role: "user", content: "Do not translate this prompt." }));
    expect(text(".role-label")).toBe("用户");
    expect(document.querySelector(".role-icon")?.getAttribute("style")).toContain(
      "var(--accent-blue-foreground)",
    );
    expect(document.querySelector('button[aria-label="复制消息"]')?.getAttribute("title")).toBe(
      "复制消息",
    );
    expect(document.querySelector(".pin-btn")?.getAttribute("title")).toBe("固定消息");
    expect(document.body.textContent).toContain("Do not translate this prompt.");
  });
  it("localizes assistant and thinking labels", async () => {
    setLocale("zh-CN");
    await render(
      message({
        content: "[Thinking]\nInternal reasoning.\n[/Thinking]\n\nVisible response.",
        has_thinking: true,
      }),
    );
    expect(text(".role-label")).toBe("助手");
    expect(text(".thinking-label")).toBe("思考");
    expect(document.body.textContent).toContain("Visible response.");
  });
  it("reports compact token totals", async () => {
    await render(
      message({
        context_tokens: 2400,
        output_tokens: 180,
        has_context_tokens: true,
        has_output_tokens: true,
      }),
    );
    expect(text(".message-tokens").replace(/\s+/g, " ")).toBe("2.4k ctx / 180 out");
  });
  it("uses the assistant accent foreground", async () => {
    await render();
    expect(document.querySelector(".role-icon")?.getAttribute("style")).toContain(
      "var(--accent-purple-foreground)",
    );
  });
  it("shows the missing context placeholder", async () => {
    await render(
      message({ output_tokens: 180, has_context_tokens: false, has_output_tokens: true }),
    );
    expect(text(".message-tokens").replace(/\s+/g, " ")).toBe("— ctx / 180 out");
  });
  it("copies exact fenced code, retaining the controlled icon-only button", async () => {
    const code = "const answer = 42;\n";
    await render(message({ content: `Here is code:\n\n\`\`\`ts\n${code}\`\`\`` }));
    await click('button[aria-label="Copy code block"]');
    expect(copyMock).toHaveBeenCalledWith(code);
    const button = document.querySelector('button[aria-label="Copied code block"]');
    expect(button?.querySelector("svg")).not.toBeNull();
    expect(button?.textContent?.trim()).toBe("");
  });
  it("forwards the header copy and updates its labels", async () => {
    await render();
    await click('button[aria-label="Copy message"]');
    expect(copyMock).toHaveBeenCalledTimes(1);
    expect(copyMock.mock.calls[0]?.[0]).toContain("Token summary");
    expect(
      document.querySelector('button[aria-label="Copied message"]')?.getAttribute("title"),
    ).toBe("Copied!");
  });
  it.each([false, true])(
    "forks from the selected ordinal in local read-only=%s",
    async (readOnly) => {
      state.readOnly = readOnly;
      state.sessions = [session()];
      const command = "claude < '/tmp/session-1-ordinal-1.txt'";
      forkMock.mockResolvedValueOnce({ launched: false, command, cwd: "/tmp/project" });
      await render(message({ ordinal: 1 }));
      await click(".fork-btn");
      expect(forkMock).toHaveBeenCalledWith(
        { id: "session-1" },
        {
          ...(readOnly ? { command_only: true } : {}),
          from_ordinal: 1,
          fork_session: true,
        },
      );
      await vi.waitFor(() => expect(copyMock).toHaveBeenCalledWith(command));
      expect(text(".fork-feedback")).not.toBe("");
    },
  );
  it("hides forking in remote read-only mode", async () => {
    state.readOnly = true;
    state.remote = true;
    state.sessions = [session()];
    await render();
    expect(document.querySelector(".fork-btn")).toBeNull();
  });
  it.each([session({ id: "child", agent: "codex" }), null])(
    "does not borrow parent fork support for embedded metadata %s",
    async (child) => {
      state.activeSession = session();
      await render(message({ session_id: "child" }), { session: child, isSubagentContext: true });
      expect(document.querySelector(".fork-btn")).toBeNull();
    },
  );
  it("routes mermaid source to the diagram renderer normally", async () => {
    await render(message({ content: "Mermaid diagram:\n\n```mermaid\ngraph TD\nA-->B\n```" }));
    await tick();
    expect(text(".mermaid-block pre.mermaid")).toBe("graph TD\nA-->B");
    expect(mermaidMock).toHaveBeenCalledTimes(1);
  });
  it("exposes mermaid source as searchable code during find", async () => {
    state.searching = true;
    await render(message({ content: "```mermaid\ngraph TD\nA-->SearchTarget\n```" }), {
      searchOrdinal: 0,
    });
    expect(mermaidMock).not.toHaveBeenCalled();
    expect(text(".code-content")).toContain("A-->SearchTarget");
    expect(text(".code-lang")).toBe("mermaid");
    expect(document.querySelector("mark")).toBeNull();
  });
});

describe("MessageContent filtered code fences", () => {
  function renderFilteredMessage(content: string, props: Record<string, unknown> = {}) {
    return mount(MessageContent, {
      target: document.body,
      props: {
        message: message({
          id: 9400,
          content,
          content_length: content.length,
        }),
        ...props,
      },
    });
  }

  function normalizedText(node: Element | null | undefined): string {
    return node?.textContent?.replace(/\s+/g, " ").trim() ?? "";
  }

  it("renders an inline collapsed placeholder and preserves prose order", async () => {
    uiState.hideBlock("code");
    const content = [
      "Before the fence.",
      "",
      "```latex",
      "\\subsection{Deployment Considerations}",
      "\\label{subsec:deployment}",
      "```",
      "",
      "After the fence.",
    ].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    const block = document.querySelector<HTMLElement>(".code-fence-block");
    const toggle = block?.querySelector<HTMLButtonElement>(".code-fence-toggle");
    expect(block).not.toBeNull();
    expect(toggle).not.toBeNull();
    expect(toggle!.getAttribute("aria-expanded")).toBe("false");
    expect(normalizedText(toggle)).toBe("Code block collapsed · latex · Expand");
    expect(block!.querySelector(".code-content")).toBeNull();

    const sequence = Array.from(
      document.querySelectorAll<HTMLElement>(
        ".message-body > .text-content, .message-body > .code-fence-block",
      ),
    ).map((node) =>
      node.classList.contains("code-fence-block") ? "code" : (node.textContent?.trim() ?? ""),
    );
    expect(sequence).toEqual(["Before the fence.", "code", "After the fence."]);

    unmount(component);
  });

  it("omits the language from the placeholder when the fence has none", async () => {
    uiState.hideBlock("code");
    const content = ["```", "plain text", "```"].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    const toggle = document.querySelector<HTMLButtonElement>(".code-fence-toggle");
    expect(normalizedText(toggle)).toBe("Code block collapsed · Expand");
    expect(normalizedText(toggle)).not.toContain("undefined");

    unmount(component);
  });

  it("expands and collapses only the clicked fence", async () => {
    uiState.hideBlock("code");
    const content = [
      "First:",
      "",
      "```latex",
      "\\alpha",
      "```",
      "",
      "Middle",
      "",
      "```python",
      "print('x')",
      "```",
      "",
      "Last",
    ].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    let blocks = document.querySelectorAll<HTMLElement>(".code-fence-block");
    expect(blocks).toHaveLength(2);

    blocks[0]!.querySelector<HTMLButtonElement>(".code-fence-toggle")!.click();
    await tick();

    blocks = document.querySelectorAll<HTMLElement>(".code-fence-block");
    expect(blocks[0]!.querySelector(".code-content")?.textContent).toContain("\\alpha");
    expect(normalizedText(blocks[0]!.querySelector(".code-fence-toggle"))).toContain("Collapse");
    expect(blocks[0]!.querySelector(".code-fence-toggle")?.getAttribute("aria-expanded")).toBe(
      "true",
    );
    expect(blocks[1]!.querySelector(".code-content")).toBeNull();

    blocks[1]!.querySelector<HTMLButtonElement>(".code-fence-toggle")!.click();
    await tick();

    blocks = document.querySelectorAll<HTMLElement>(".code-fence-block");
    expect(blocks[1]!.querySelector(".code-content")?.textContent).toContain("print('x')");
    expect(blocks[0]!.querySelector(".code-content")?.textContent).toContain("\\alpha");

    blocks[0]!.querySelector<HTMLButtonElement>(".code-fence-toggle")!.click();
    await tick();

    blocks = document.querySelectorAll<HTMLElement>(".code-fence-block");
    expect(blocks[0]!.querySelector(".code-content")).toBeNull();
    expect(blocks[1]!.querySelector(".code-content")?.textContent).toContain("print('x')");

    unmount(component);
  });

  it("localizes the collapsed placeholder in Simplified Chinese", async () => {
    setLocale("zh-CN");
    uiState.hideBlock("code");
    const content = ["```latex", "\\subsection{Deployment Considerations}", "```"].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    const toggle = document.querySelector<HTMLButtonElement>(".code-fence-toggle");
    expect(normalizedText(toggle)).toBe("代码块已折叠 · latex · 展开");

    unmount(component);
  });

  it("keeps filtered code excluded from search while allowing manual expansion", async () => {
    state.searching = true;
    uiState.hideBlock("code");
    const content = [
      "Before the fence.",
      "",
      "```latex",
      "\\subsection{Deployment Considerations}",
      "```",
      "",
      "After the fence.",
    ].join("\n");

    const component = renderFilteredMessage(content, {
      searchOrdinal: 0,
    });
    await tick();

    expect(document.querySelector(".code-content")).toBeNull();
    document.querySelector<HTMLButtonElement>(".code-fence-toggle")!.click();
    await tick();
    expect(document.querySelector(".code-fence-block [data-search-block]")).toBeNull();
    expect(document.querySelector(".code-content")?.textContent).toContain(
      "\\subsection{Deployment Considerations}",
    );

    unmount(component);
  });

  it("routes an expanded filtered mermaid fence through MermaidBlock", async () => {
    uiState.hideBlock("code");
    const content = ["Mermaid diagram:", "", "```mermaid", "graph TD", "A-->B", "```"].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    const toggle = document.querySelector<HTMLButtonElement>(".code-fence-toggle");
    expect(normalizedText(toggle)).toBe("Code block collapsed · mermaid · Expand");

    toggle!.click();
    await tick();
    await tick();

    expect(mermaidMock).toHaveBeenCalledTimes(1);
    expect(document.querySelector(".mermaid-block pre.mermaid")?.textContent).toContain("graph TD");
    expect(document.querySelector(".code-content")).toBeNull();

    unmount(component);
  });

  it("copies the raw fence source after expanding it from the placeholder", async () => {
    uiState.hideBlock("code");
    const code = "const answer = 42;\n";
    const content = ["```ts", code.trimEnd(), "```"].join("\n");

    const component = renderFilteredMessage(content);
    await tick();

    document.querySelector<HTMLButtonElement>(".code-fence-toggle")!.click();
    await tick();

    const copyButton = document.querySelector<HTMLButtonElement>(
      'button.kit-copy-btn[aria-label="Copy code block"]',
    );
    expect(copyButton).not.toBeNull();
    copyButton!.click();
    await Promise.resolve();
    await tick();

    expect(copyMock).toHaveBeenCalledWith(code);

    unmount(component);
  });
});
