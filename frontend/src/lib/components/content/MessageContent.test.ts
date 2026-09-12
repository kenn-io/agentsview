// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount, type ComponentProps } from "svelte";
import type { Message, Session } from "../../api/types.js";
import { setLocale } from "../../i18n/index.js";
import MessageContent from "./MessageContent.svelte";

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
vi.mock("../../stores/ui.svelte.js", () => ({ ui: { isBlockVisible: () => true } }));
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
  setLocale("en");
  vi.clearAllMocks();
  state.sessions = [];
  state.activeSession = null;
  state.readOnly = false;
  state.remote = false;
  state.searching = false;
});

describe("MessageContent", () => {
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
