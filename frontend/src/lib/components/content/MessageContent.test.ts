// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message, Session } from "../../api/types.js";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import MessageContent from "./MessageContent.svelte";

const copyToClipboardMock = vi.hoisted(() =>
  vi.fn().mockResolvedValue(true),
);

const forkSessionMock = vi.hoisted(() => vi.fn());
const sessionsState = vi.hoisted(() => ({
  sessions: [] as Session[],
  activeSession: null as Session | null,
}));
const syncState = vi.hoisted(() => ({
  readOnly: false,
}));
const runtimeState = vi.hoisted(() => ({
  isRemote: false,
}));

vi.mock("../../stores/messages.svelte.js", () => ({
  messages: {
    sessionId: "",
    mainModel: "",
  },
}));

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: {
    isBlockVisible: () => true,
  },
}));

vi.mock("../../stores/pins.svelte.js", () => ({
  pins: {
    isPinned: () => false,
    togglePin: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: sessionsState,
}));

vi.mock("../../stores/sync.svelte.js", () => ({
  sync: syncState,
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  isRemoteConnection: () => runtimeState.isRemote,
}));

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SessionsService: {
      postApiV1SessionsIdResume: forkSessionMock,
    },
  };
});

vi.mock("../../utils/highlight.js", async () => {
  const actual = await vi.importActual<
    typeof import("../../utils/highlight.js")
  >("../../utils/highlight.js");
  return {
    ...actual,
    applyHighlight: () => {},
  };
});

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: copyToClipboardMock,
}));

type MessageWithTokenFlags = Message & {
  has_context_tokens?: boolean;
  has_output_tokens?: boolean;
};

function makeMessage(
  overrides: Partial<MessageWithTokenFlags> = {},
): MessageWithTokenFlags {
  return {
    id: 1,
    session_id: "session-1",
    ordinal: 0,
    role: "assistant",
    content: "Token summary",
    timestamp: "2026-02-20T12:30:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 13,
    model: "claude-sonnet",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    ...overrides,
  };
}

afterEach(() => {
  setLocale("en");
  document.body.innerHTML = "";
  vi.clearAllMocks();
  sessionsState.sessions = [];
  sessionsState.activeSession = null;
  syncState.readOnly = false;
  runtimeState.isRemote = false;
});

beforeEach(() => {
  forkSessionMock.mockReset();
});

describe("MessageContent", () => {
  it("renders message controls in Simplified Chinese without translating content", async () => {
    setLocale("zh-CN");
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          role: "user",
          content: "Do not translate this prompt.",
        }),
      },
    });

    await tick();

    expect(document.querySelector(".role-label")?.textContent?.trim()).toBe(
      "用户",
    );
    expect(document.querySelector(".role-icon")?.getAttribute("style")).toContain(
      "var(--accent-blue-foreground)",
    );
    const copyButton = document.querySelector<HTMLButtonElement>(
      "button.kit-copy-btn",
    );
    expect(copyButton?.getAttribute("aria-label")).toBe("复制消息");
    expect(copyButton?.getAttribute("title")).toBe("复制消息");
    expect(
      document.querySelector<HTMLButtonElement>(".pin-btn")?.getAttribute(
        "title",
      ),
    ).toBe("固定消息");
    expect(document.body.textContent).toContain("Do not translate this prompt.");

    unmount(component);
  });

  it("localizes assistant role and thinking block labels", async () => {
    setLocale("zh-CN");
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          id: 2,
          role: "assistant",
          content: "[Thinking]\nInternal reasoning.\n[/Thinking]\n\nVisible response.",
          content_length: 61,
          has_thinking: true,
          thinking_text: "Internal reasoning.",
        }),
      },
    });

    await tick();

    expect(document.querySelector(".role-label")?.textContent?.trim()).toBe(
      "助手",
    );
    expect(document.querySelector(".thinking-label")?.textContent?.trim()).toBe(
      "思考",
    );
    expect(document.body.textContent).toContain("Visible response.");

    unmount(component);
  });

  it("renders compact token totals when both token metrics are reported", async () => {
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          context_tokens: 2400,
          output_tokens: 180,
          has_context_tokens: true,
          has_output_tokens: true,
        }),
      },
    });

    await tick();
    const tokenMeta = document.querySelector(".message-tokens");
    expect(tokenMeta?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "2.4k ctx / 180 out",
    );

    unmount(component);
  });

  it("uses the assistant accent foreground for assistant role icons", async () => {
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({ role: "assistant" }),
      },
    });

    await tick();

    expect(document.querySelector(".role-icon")?.getAttribute("style")).toContain(
      "var(--accent-purple-foreground)",
    );

    unmount(component);
  });

  it("renders an explicit missing token placeholder when context tokens are absent", async () => {
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          context_tokens: 0,
          output_tokens: 180,
          has_context_tokens: false,
          has_output_tokens: true,
        }),
      },
    });

    await tick();
    const tokenMeta = document.querySelector(".message-tokens");
    expect(tokenMeta?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "— ctx / 180 out",
    );

    unmount(component);
  });

  it("copies the exact raw content from a fenced code block", async () => {
    const code = "const answer = 42;\n";
    const content = `Here is code:\n\n\`\`\`ts\n${code}\`\`\``;
    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          content,
          content_length: content.length,
        }),
      },
    });

    await tick();
    const copyButton = document.querySelector<HTMLButtonElement>(
      'button.kit-copy-btn[aria-label="Copy code block"]',
    );
    expect(copyButton).not.toBeNull();
    expect(copyButton!.querySelector("svg")).not.toBeNull();
    expect(copyButton!.textContent?.trim()).toBe("");

    copyButton!.click();
    await Promise.resolve();
    await tick();

    expect(copyToClipboardMock).toHaveBeenCalledWith(code);
    expect(copyButton!.getAttribute("aria-label")).toBe(
      "Copied code block",
    );
    expect(copyButton!.querySelector("svg")).not.toBeNull();
    expect(copyButton!.textContent?.trim()).toBe("");

    unmount(component);
  });

  // Regression guard for the kit-ui CopyButton adoption: the header copy
  // button runs in controlled mode, so click forwarding into the app's
  // clipboard util and the parent-owned copied aria/title state must keep
  // working if kit-ui's API or class names change.
  it("forwards the header copy click and reflects the copied state", async () => {
    const component = mount(MessageContent, {
      target: document.body,
      props: { message: makeMessage() },
    });

    await tick();
    const copyButton = document.querySelector<HTMLButtonElement>(
      'button.kit-copy-btn[aria-label="Copy message"]',
    );
    expect(copyButton).not.toBeNull();
    expect(copyButton!.getAttribute("title")).toBe("Copy message");

    copyButton!.click();
    await Promise.resolve();
    await tick();

    expect(copyToClipboardMock).toHaveBeenCalledTimes(1);
    expect(copyToClipboardMock.mock.calls[0]?.[0]).toContain("Token summary");
    expect(copyButton!.getAttribute("aria-label")).toBe("Copied message");
    expect(copyButton!.getAttribute("title")).toBe("Copied!");

    unmount(component);
  });

  it("forks a Claude session from the selected message ordinal", async () => {
    sessionsState.sessions = [{
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
    } as Session];
    forkSessionMock.mockResolvedValueOnce({
      launched: false,
      command: "claude < '/tmp/agentsview/claude-message-points/session-1-ordinal-1.txt'",
      cwd: "/tmp/project",
    });

    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          session_id: "session-1",
          ordinal: 1,
          role: "assistant",
          content: "Branch here.",
        }),
      },
    });

    await tick();

    const forkButton = document.querySelector<HTMLButtonElement>(
      "button.fork-btn",
    );
    expect(forkButton).not.toBeNull();
    forkButton!.click();
    await Promise.resolve();
    await tick();

    expect(forkSessionMock).toHaveBeenCalledWith({
      id: "session-1",
      requestBody: {
        from_ordinal: 1,
        fork_session: true,
      },
    });
    expect(copyToClipboardMock).toHaveBeenCalledWith(
      "claude < '/tmp/agentsview/claude-message-points/session-1-ordinal-1.txt'",
    );
    await vi.waitFor(() => {
      expect(document.querySelector(".fork-feedback")).toBeTruthy();
    });
    const forkFeedback = document.querySelector(".fork-feedback");
    expect(forkFeedback).toBeTruthy();
    expect(forkFeedback?.textContent?.trim()).not.toBe("");

    unmount(component);
  });

  it("does not show the fork action for an embedded non-Claude child session", async () => {
    sessionsState.activeSession = {
      id: "parent-session",
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
    } as Session;

    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          session_id: "child-session",
          ordinal: 1,
          role: "assistant",
          content: "Embedded child message.",
        }),
        session: {
          id: "child-session",
          agent: "codex",
          project: "proj-b",
          machine: "test",
          first_message: "child",
          started_at: "2026-02-20T12:30:00Z",
          ended_at: "2026-02-20T12:31:00Z",
          message_count: 2,
          user_message_count: 1,
          total_output_tokens: 0,
          peak_context_tokens: 0,
          is_automated: false,
          created_at: "2026-02-20T12:30:00Z",
        } as Session,
        isSubagentContext: true,
      },
    });

    await tick();

    expect(document.querySelector("button.fork-btn")).toBeNull();

    unmount(component);
  });

  it("requests command-only message forks in local read-only mode", async () => {
    syncState.readOnly = true;
    sessionsState.sessions = [{
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
    } as Session];
    forkSessionMock.mockResolvedValueOnce({
      launched: false,
      command: "claude < '/tmp/agentsview/claude-message-points/session-1-ordinal-1.txt'",
      cwd: "/tmp/project",
    });

    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          session_id: "session-1",
          ordinal: 1,
          role: "assistant",
          content: "Branch here.",
        }),
      },
    });

    await tick();

    document.querySelector<HTMLButtonElement>("button.fork-btn")!.click();
    await Promise.resolve();
    await tick();

    expect(forkSessionMock).toHaveBeenCalledWith({
      id: "session-1",
      requestBody: {
        command_only: true,
        from_ordinal: 1,
        fork_session: true,
      },
    });

    unmount(component);
  });

  it("hides the fork action in remote read-only mode", async () => {
    syncState.readOnly = true;
    runtimeState.isRemote = true;
    sessionsState.sessions = [{
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
    } as Session];

    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          session_id: "session-1",
          ordinal: 1,
          role: "assistant",
          content: "Branch here.",
        }),
      },
    });

    await tick();

    expect(document.querySelector("button.fork-btn")).toBeNull();

    unmount(component);
  });

  it("does not fall back to the active session when embedded session metadata is missing", async () => {
    sessionsState.activeSession = {
      id: "parent-session",
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
    } as Session;

    const component = mount(MessageContent, {
      target: document.body,
      props: {
        message: makeMessage({
          session_id: "child-session",
          ordinal: 1,
          role: "assistant",
          content: "Embedded child message.",
        }),
        session: null,
        isSubagentContext: true,
      },
    });

    await tick();

    expect(document.querySelector("button.fork-btn")).toBeNull();

    unmount(component);
  });
});
