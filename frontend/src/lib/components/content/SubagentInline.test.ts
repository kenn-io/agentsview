// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types.js";
// @ts-ignore
import SubagentInline from "./SubagentInline.svelte";

const {
  getMessages,
  getSession,
  childSessions,
} = vi.hoisted(() => ({
  getMessages: vi.fn(),
  getSession: vi.fn(),
  childSessions: new Map<string, Session>(),
}));

vi.mock("../../api/client.js", () => ({
  getMessages,
  getSession,
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    childSessions,
    pendingNavTarget: null,
    navigateToSession: vi.fn(),
  },
}));

vi.mock("../../stores/router.svelte.js", () => ({
  router: {
    route: "sessions",
    navigate: vi.fn(() => true),
    buildSessionHref: vi.fn((id: string) => `#/sessions/${id}`),
  },
}));

function makeSession(
  overrides: Partial<Session> = {},
): Session {
  return {
    id: "subagent-session-id",
    project: "proj-a",
    machine: "mac",
    agent: "claude",
    first_message: "subagent",
    started_at: "2026-02-20T12:30:00Z",
    ended_at: "2026-02-20T12:31:00Z",
    message_count: 1,
    user_message_count: 0,
    total_output_tokens: 180,
    peak_context_tokens: 0,
    has_total_output_tokens: true,
    has_peak_context_tokens: false,
    is_automated: false,
    created_at: "2026-02-20T12:30:00Z",
    ...overrides,
  };
}

afterEach(() => {
  childSessions.clear();
  getMessages.mockReset();
  getSession.mockReset();
  document.body.innerHTML = "";
});

describe("SubagentInline", () => {
  it("prefers fetched session metadata for the token summary when available", async () => {
    childSessions.set(
      "subagent-session-id",
      makeSession(),
    );
    getMessages.mockResolvedValue({
      messages: [],
      count: 0,
    });
    getSession.mockResolvedValue(
      makeSession({
        peak_context_tokens: 2400,
        has_peak_context_tokens: true,
      }),
    );

    const component = mount(SubagentInline, {
      target: document.body,
      props: { sessionId: "subagent-session-id" },
    });

    await tick();
    expect(
      document.querySelector(".toggle-tokens")?.textContent,
    ).toContain("— ctx / 180 out");

    document.querySelector<HTMLButtonElement>(".subagent-toggle")?.click();
    await vi.waitFor(() => {
      expect(
        document.querySelector(".toggle-tokens")?.textContent,
      ).toContain("2.4k ctx / 180 out");
    });

    unmount(component);
  });
});
