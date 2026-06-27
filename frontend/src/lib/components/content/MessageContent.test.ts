// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Message } from "../../api/types.js";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import MessageContent from "./MessageContent.svelte";

const copyToClipboardMock = vi.hoisted(() =>
  vi.fn().mockResolvedValue(true),
);

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
  sessions: {
    sessions: [],
    activeSession: null,
  },
}));

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
      "button.copy-btn",
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
      'button.copy-btn[aria-label="Copy code block"]',
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
});
