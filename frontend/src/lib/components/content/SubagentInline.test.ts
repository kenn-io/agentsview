// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { Session } from "../../api/types.js";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import SubagentInline from "./SubagentInline.svelte";

const {
  getMessages,
  getSession,
  childSessions,
  callGenerated,
} = vi.hoisted(() => ({
  getMessages: vi.fn(),
  getSession: vi.fn(),
  childSessions: new Map<string, Session>(),
  callGenerated: vi.fn(
    (request: () => Promise<unknown>, _signal?: AbortSignal) => request(),
  ),
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated,
  isAbortError: vi.fn(() => false),
}));

vi.mock("../../api/generated/index", () => ({
  SessionsService: {
    getApiV1SessionsIdMessages: vi.fn(({ id, limit }) =>
      getMessages(id, { limit })
    ),
    getApiV1SessionsId: vi.fn(({ id }) => getSession(id)),
  },
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
  setLocale("en");
  childSessions.clear();
  getMessages.mockReset();
  getSession.mockReset();
  callGenerated.mockReset();
  callGenerated.mockImplementation(
    (request: () => Promise<unknown>, _signal?: AbortSignal) => request(),
  );
  document.body.innerHTML = "";
});

describe("SubagentInline", () => {
  it("aborts the nested read when collapsed", async () => {
    const signals: AbortSignal[] = [];
    callGenerated.mockImplementation((request, signal) => {
      signals.push(signal as AbortSignal);
      return request();
    });
    getMessages.mockImplementationOnce(() => new Promise(() => {}));
    getSession.mockImplementationOnce(() => new Promise(() => {}));
    const component = mount(SubagentInline, {
      target: document.body,
      props: { sessionId: "subagent-session-id" },
    });
    await tick();
    const toggle = document.querySelector<HTMLButtonElement>(
      ".subagent-toggle",
    )!;

    toggle.click();
    await tick();
    toggle.click();
    await tick();

    expect(signals).toHaveLength(2);
    expect(signals.every((signal) => signal.aborted)).toBe(true);
    unmount(component);
  });

  it("renders subagent controls in Simplified Chinese", async () => {
    setLocale("zh-CN");
    childSessions.set(
      "subagent-session-id",
      makeSession({ message_count: 2 }),
    );
    getMessages.mockResolvedValue({
      messages: [],
      count: 0,
    });
    getSession.mockResolvedValue(makeSession({ message_count: 2 }));

    const component = mount(SubagentInline, {
      target: document.body,
      props: { sessionId: "subagent-session-id" },
    });

    await tick();
    expect(document.body.textContent).toContain("Subagent 会话");
    expect(document.body.textContent).toContain("2 条消息");
    const openLink = document.querySelector<HTMLAnchorElement>(
      ".open-session-link",
    );
    expect(openLink?.textContent).toContain("打开会话");
    expect(openLink?.getAttribute("title")).toBe("作为完整会话打开");

    document.querySelector<HTMLButtonElement>(".subagent-toggle")?.click();
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("无消息");
    });

    unmount(component);
  });

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
