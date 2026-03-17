// @vitest-environment jsdom
// ABOUTME: Unit tests for MessageContent's off-main-model badge behavior.
// ABOUTME: Covers correct session resolution for top-level and subagent messages.
import { describe, it, expect, vi } from "vitest";
import { mount, unmount, tick } from "svelte";
import type { Message, Session } from "../../api/types.js";

// Mock all stores and child components that MessageContent depends on.
vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    sessions: [
      {
        id: "parent-session",
        project: "proj",
        machine: "mac",
        agent: "copilot",
        first_message: "hello",
        started_at: "2026-01-01T00:00:00Z",
        ended_at: "2026-01-01T00:01:00Z",
        message_count: 2,
        user_message_count: 1,
        total_output_tokens: 0,
        peak_context_tokens: 0,
        main_model: "claude-sonnet-4.6",
        created_at: "2026-01-01T00:00:00Z",
      },
    ],
    activeSession: null,
    childSessions: new Map(),
  },
}));

vi.mock("../../stores/ui.svelte.js", () => ({
  ui: {
    isBlockVisible: () => true,
    findInSession: { query: "" },
  },
}));

vi.mock("../../stores/pins.svelte.js", () => ({
  pins: { isPinned: () => false, togglePin: async () => {} },
}));

vi.mock("./ThinkingBlock.svelte", () => ({ default: {} }));
vi.mock("./ToolBlock.svelte", () => ({ default: {} }));
vi.mock("./CodeBlock.svelte", () => ({ default: {} }));
vi.mock("./SkillBlock.svelte", () => ({ default: {} }));

// @ts-ignore
import MessageContent from "./MessageContent.svelte";

function makeMessage(overrides: Partial<Message> = {}): Message {
  return {
    id: 1,
    session_id: "parent-session",
    ordinal: 0,
    role: "assistant",
    content: "Hello from assistant",
    has_thinking: false,
    has_tool_use: false,
    content_length: 20,
    timestamp: "2026-01-01T00:00:30Z",
    model: "",
    ...overrides,
  };
}

function makeSession(id: string, mainModel: string): Session {
  return {
    id,
    project: "proj",
    machine: "mac",
    agent: "copilot",
    first_message: "hello",
    started_at: "2026-01-01T00:00:00Z",
    ended_at: "2026-01-01T00:01:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    main_model: mainModel,
    created_at: "2026-01-01T00:00:00Z",
  };
}

describe("MessageContent off-main-model badge", () => {
  it("shows no model badge when message.model matches the session main_model", async () => {
    const message = makeMessage({ model: "claude-sonnet-4.6" });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message },
    });
    await tick();

    expect(document.querySelector(".message-model")).toBeNull();
    unmount(component);
  });

  it("shows model badge when message.model differs from session main_model", async () => {
    const message = makeMessage({ model: "claude-haiku-4.5" });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message },
    });
    await tick();

    const badge = document.querySelector(".message-model");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("haiku-4.5");
    unmount(component);
  });

  it("shows no model badge when message has no model set", async () => {
    const message = makeMessage({ model: "" });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message },
    });
    await tick();

    expect(document.querySelector(".message-model")).toBeNull();
    unmount(component);
  });

  it("shows no model badge for user messages even when model is set", async () => {
    const message = makeMessage({ role: "user", model: "claude-haiku-4.5" });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message },
    });
    await tick();

    expect(document.querySelector(".message-model")).toBeNull();
    unmount(component);
  });

  it("uses explicit owningSession prop over sessions.sessions lookup", async () => {
    // The message belongs to a child session (haiku as main model) but its
    // session_id is "parent-session" in the store (sonnet as main model).
    // Passing the child session explicitly should compare against haiku.
    const childSession = makeSession("child-session", "claude-haiku-4.5");
    const message = makeMessage({
      session_id: "child-session",
      model: "claude-haiku-4.5", // same as child main_model → no badge
    });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message, owningSession: childSession },
    });
    await tick();

    // model matches child session's main_model → no badge
    expect(document.querySelector(".message-model")).toBeNull();
    unmount(component);
  });

  it("shows badge for subagent message using a different model than its child session", async () => {
    // Child session's main model is haiku, but this message used sonnet.
    const childSession = makeSession("child-session", "claude-haiku-4.5");
    const message = makeMessage({
      session_id: "child-session",
      model: "claude-sonnet-4.6",
    });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message, owningSession: childSession },
    });
    await tick();

    const badge = document.querySelector(".message-model");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("sonnet-4.6");
    unmount(component);
  });

  it("without owningSession prop, falls back to sessions.sessions lookup by session_id", async () => {
    // parent-session has main_model=claude-sonnet-4.6 in the mock store.
    // Message uses haiku → badge should appear.
    const message = makeMessage({
      session_id: "parent-session",
      model: "claude-haiku-4.5",
    });
    const component = mount(MessageContent, {
      target: document.body,
      props: { message },
    });
    await tick();

    const badge = document.querySelector(".message-model");
    expect(badge).toBeTruthy();
    expect(badge?.textContent).toContain("haiku-4.5");
    unmount(component);
  });
});
