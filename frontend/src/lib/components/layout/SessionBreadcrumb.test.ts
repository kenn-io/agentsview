// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vite-plus/test";
import { mount, unmount, tick } from "svelte";
import { createClassComponent } from "svelte/legacy";
// @ts-ignore
import SessionBreadcrumb from "./SessionBreadcrumb.svelte";
import type { Message, Session } from "../../api/types.js";
import {
  OpenersService,
  SessionsService,
} from "../../api/generated/index";
import { messages } from "../../stores/messages.svelte.js";
import { setLocale } from "../../i18n/index.js";

vi.mock("../../api/client.js", () => ({
  listOpeners: vi.fn().mockResolvedValue({ openers: [] }),
  getSessionDirectory: vi
    .fn()
    .mockResolvedValue({ path: "" }),
  resumeSession: vi.fn(),
  openSession: vi.fn(),
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
}));

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    OpenersService: {
      getApiV1Openers: vi.fn(),
    },
    SessionsService: {
      getApiV1SessionsIdDirectory: vi.fn(),
      getApiV1SessionsIdUsage: vi.fn(),
      postApiV1SessionsIdResume: vi.fn(),
      postApiV1SessionsIdOpen: vi.fn(),
    },
  };
});

const openersService = OpenersService as unknown as {
  getApiV1Openers: ReturnType<typeof vi.fn>;
};

const sessionsService = SessionsService as unknown as {
  getApiV1SessionsIdDirectory: ReturnType<typeof vi.fn>;
  getApiV1SessionsIdUsage: ReturnType<typeof vi.fn>;
  postApiV1SessionsIdResume: ReturnType<typeof vi.fn>;
};

type SessionWithTokenFlags = Session & {
  has_peak_context_tokens?: boolean;
  has_total_output_tokens?: boolean;
};

function makeSession(
  agent: string,
  overrides: Partial<SessionWithTokenFlags> = {},
): SessionWithTokenFlags {
  return {
    id: "run:123456789abcdef",
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
    is_automated: false,
    created_at: "2026-02-20T12:30:00Z",
    ...overrides,
  };
}

interface SessionUsage {
  session_id: string;
  agent: string;
  project: string;
  total_output_tokens: number;
  peak_context_tokens: number;
  has_token_data: boolean;
  cost_usd: number;
  has_cost: boolean;
  models: string[];
  unpriced_models: string[];
  server_running: boolean;
}

function makeUsage(
  overrides: Partial<SessionUsage> = {},
): SessionUsage {
  return {
    session_id: "run:123456789abcdef",
    agent: "claude",
    project: "proj-a",
    total_output_tokens: 0,
    peak_context_tokens: 0,
    has_token_data: false,
    cost_usd: 0,
    has_cost: false,
    models: [],
    unpriced_models: [],
    server_running: true,
    ...overrides,
  };
}

function makeAssistantMessage(model: string): Message {
  return {
    id: 1,
    session_id: "run:123456789abcdef",
    ordinal: 0,
    role: "assistant",
    content: "hi",
    timestamp: "2026-02-20T12:30:30Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 2,
    model,
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
  };
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

async function flushPromises() {
  await Promise.resolve();
  await tick();
}

beforeEach(() => {
  openersService.getApiV1Openers
    .mockReset()
    .mockResolvedValue({ openers: [] });
  sessionsService.getApiV1SessionsIdDirectory
    .mockReset()
    .mockResolvedValue({ path: "" });
  sessionsService.getApiV1SessionsIdUsage
    .mockReset()
    .mockResolvedValue(makeUsage());
  sessionsService.postApiV1SessionsIdResume.mockReset();
});

afterEach(() => {
  setLocale("en");
  document.body.innerHTML = "";
});

describe("SessionBreadcrumb", () => {
  it("renders session reading controls in Simplified Chinese", async () => {
    setLocale("zh-CN");
    openersService.getApiV1Openers.mockResolvedValue({
      openers: [
        {
          id: "vscode",
          name: "VS Code",
          kind: "editor",
          bin: "code",
        },
      ],
    });
    sessionsService.getApiV1SessionsIdDirectory.mockResolvedValue({
      path: "/tmp/project",
    });

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          file_path: "/tmp/project/session.jsonl",
        }),
        onBack: () => {},
      },
    });

    await vi.waitFor(() => {
      expect(document.querySelector(".resume-btn")).toBeTruthy();
    });
    await tick();

    const backButton = document.querySelector<HTMLButtonElement>(
      ".breadcrumb-link",
    );
    expect(backButton?.textContent?.trim()).toBe("会话");
    expect(backButton?.getAttribute("title")).toBe("返回会话列表");

    const linkButton = document.querySelector<HTMLButtonElement>(
      ".link-btn",
    );
    expect(linkButton?.getAttribute("aria-label")).toBe("复制会话链接");
    expect(linkButton?.getAttribute("title")).toBe("复制会话链接");

    const findButton = document.querySelector<HTMLButtonElement>(
      ".find-btn",
    );
    expect(findButton?.getAttribute("aria-label")).toBe("在会话中查找");
    expect(findButton?.getAttribute("title")).toBe("在会话中查找 (/)");

    const resumeButton = document.querySelector<HTMLButtonElement>(
      ".resume-btn",
    );
    expect(resumeButton?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "继续",
    );
    resumeButton?.click();
    await tick();

    expect(document.body.textContent).toContain("默认终端");
    expect(document.body.textContent).toContain("复制命令");
    expect(document.body.textContent).toContain("复制目录路径");
    expect(document.body.textContent).toContain("打开方式");
    expect(document.body.textContent).toContain("VS Code");

    const actionsButton = document.querySelector<HTMLButtonElement>(
      ".actions-btn",
    );
    expect(actionsButton?.getAttribute("aria-label")).toBe("会话操作");
    actionsButton?.click();
    await tick();

    expect(document.body.textContent).toContain("重命名");
    expect(document.body.textContent).toContain("删除");

    unmount(component);
  });

  it("keeps whole-session resume request bodies unchanged", async () => {
    sessionsService.postApiV1SessionsIdResume.mockResolvedValue({
      launched: false,
      command: "claude --resume run:123456789abcdef",
      cwd: "/tmp/project",
    });

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          file_path: "/tmp/project/session.jsonl",
        }),
        onBack: () => {},
      },
    });

    await vi.waitFor(() => {
      expect(document.querySelector(".resume-btn")).toBeTruthy();
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();

    const resumeItem = document.querySelector<HTMLButtonElement>(
      ".open-menu-item",
    );
    expect(resumeItem).toBeTruthy();
    resumeItem!.click();
    await Promise.resolve();
    await tick();

    expect(sessionsService.postApiV1SessionsIdResume).toHaveBeenCalledWith({
      id: "run:123456789abcdef",
      requestBody: {},
    });

    unmount(component);
  });

  it("renders gemini with rose badge color", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("gemini"),
        onBack: () => {},
      },
    });

    await tick();
    const badge = document.querySelector(".agent-badge");
    expect(badge).toBeTruthy();
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-rose)",
    );
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-rose-foreground)",
    );

    unmount(component);
  });

  it("falls back to blue for unknown agents", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("unknown"),
        onBack: () => {},
      },
    });

    await tick();
    const badge = document.querySelector(".agent-badge");
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-blue)",
    );
    expect(badge?.getAttribute("style")).toContain(
      "var(--accent-blue-foreground)",
    );

    unmount(component);
  });

  describe("copy-link timer", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it("restarts timer on rapid re-copy", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });
      await tick();

      const linkBtn = document.querySelector(".link-btn");
      expect(linkBtn).toBeTruthy();

      // First copy
      linkBtn!.dispatchEvent(
        new MouseEvent("click", { bubbles: true }),
      );
      await tick();
      await vi.advanceTimersByTimeAsync(0);
      await tick();
      expect(
        linkBtn!.classList.contains("link-btn--copied"),
      ).toBe(true);

      // Advance 1s, then copy again
      await vi.advanceTimersByTimeAsync(1000);
      linkBtn!.dispatchEvent(
        new MouseEvent("click", { bubbles: true }),
      );
      await tick();
      await vi.advanceTimersByTimeAsync(0);
      await tick();

      // 600ms after second click — first timer's 1.5s
      // would have expired, but it was cleared
      await vi.advanceTimersByTimeAsync(600);
      await tick();
      expect(
        linkBtn!.classList.contains("link-btn--copied"),
      ).toBe(true);

      // After full 1.5s from second click, state clears
      await vi.advanceTimersByTimeAsync(900);
      await tick();
      expect(
        linkBtn!.classList.contains("link-btn--copied"),
      ).toBe(false);

      unmount(component);
    });
  });

  it("renders compact token totals when both token metrics are reported", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          peak_context_tokens: 2400,
          total_output_tokens: 180,
          has_peak_context_tokens: true,
          has_total_output_tokens: true,
        }),
        onBack: () => {},
      },
    });

    await tick();
    const tokenBadge = document.querySelector(".token-badge");
    expect(tokenBadge?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "2.4k ctx / 180 out",
    );

    unmount(component);
  });

  it("renders an explicit missing token placeholder when context tokens are absent", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          peak_context_tokens: 0,
          total_output_tokens: 180,
          has_peak_context_tokens: false,
          has_total_output_tokens: true,
        }),
        onBack: () => {},
      },
    });

    await tick();
    const tokenBadge = document.querySelector(".token-badge");
    expect(tokenBadge?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "— ctx / 180 out",
    );

    unmount(component);
  });

  it("renders a dedicated mobile token badge", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          peak_context_tokens: 2400,
          total_output_tokens: 180,
          has_peak_context_tokens: true,
          has_total_output_tokens: true,
        }),
        onBack: () => {},
      },
    });

    await tick();

    const mobileTokenBadge = document.querySelector(
      ".token-badge--mobile",
    );
    expect(
      mobileTokenBadge?.textContent?.replace(/\s+/g, " ").trim(),
    ).toBe("2.4k ctx / 180 out");

    unmount(component);
  });

  describe("summary-mode badge", () => {
    it("shows the badge for summary-mode antigravity-cli", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity-cli", {
            transcript_fidelity: "summary",
          }),
          onBack: () => {},
        },
      });
      await tick();
      const badge = document.querySelector(".summary-badge");
      expect(badge).toBeTruthy();
      expect(badge?.textContent?.trim().toLowerCase()).toContain("summary mode");
      unmount(component);
    });

    it("hides the badge for full antigravity-cli", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity-cli", {
            transcript_fidelity: "full",
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".summary-badge")).toBeNull();
      unmount(component);
    });

    it("hides the badge for other agents even if summary", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude-code", {
            transcript_fidelity: "summary",
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".summary-badge")).toBeNull();
      unmount(component);
    });
  });

  describe("malformed-lines badge", () => {
    it("shows the badge with the line count when parser_malformed_lines is positive", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            parser_malformed_lines: 3,
          }),
          onBack: () => {},
        },
      });
      await tick();
      const badge = document.querySelector(".malformed-badge");
      expect(badge).toBeTruthy();
      expect(badge?.textContent?.trim()).toBe("3 malformed lines");
      expect(badge?.getAttribute("title")).toBe(
        "3 lines in the source file could not be parsed",
      );
      unmount(component);
    });

    it("uses singular wording for exactly one malformed line", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            parser_malformed_lines: 1,
          }),
          onBack: () => {},
        },
      });
      await tick();
      const badge = document.querySelector(".malformed-badge");
      expect(badge?.textContent?.trim()).toBe("1 malformed line");
      expect(badge?.getAttribute("title")).toBe(
        "1 line in the source file could not be parsed",
      );
      unmount(component);
    });

    it("hides the badge when parser_malformed_lines is zero", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            parser_malformed_lines: 0,
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".malformed-badge")).toBeNull();
      unmount(component);
    });

    it("hides the badge when parser_malformed_lines is absent", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".malformed-badge")).toBeNull();
      unmount(component);
    });
  });

  describe("decode-confidence badge", () => {
    it("shows the badge for low-confidence antigravity", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity", {
            decode_confidence: "low",
          }),
          onBack: () => {},
        },
      });
      await tick();
      const badge = document.querySelector(".decode-badge");
      expect(badge).toBeTruthy();
      expect(badge?.textContent?.trim().toLowerCase()).toContain(
        "unverified schema",
      );
      unmount(component);
    });

    it("shows the badge for low-confidence antigravity-cli", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity-cli", {
            decode_confidence: "low",
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".decode-badge")).toBeTruthy();
      unmount(component);
    });

    it("hides the badge for high confidence", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity", {
            decode_confidence: "high",
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".decode-badge")).toBeNull();
      unmount(component);
    });

    it("hides the badge when confidence is absent", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("antigravity"),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".decode-badge")).toBeNull();
      unmount(component);
    });

    it("hides the badge for non-antigravity agents", async () => {
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            decode_confidence: "low",
          }),
          onBack: () => {},
        },
      });
      await tick();
      expect(document.querySelector(".decode-badge")).toBeNull();
      unmount(component);
    });
  });

  it("hides local-only actions for remote sessions", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          id: "devbox1~abc-123",
          machine: "devbox1",
        }),
        onBack: () => {},
      },
    });

    await tick();

    // The dropdown trigger (.resume-btn) should not appear
    // for remote sessions (no resume, no copy-dir, no open-in).
    const resumeBtn = document.querySelector(".resume-btn");
    expect(resumeBtn).toBeNull();

    unmount(component);
  });

  describe("cost badge", () => {
    afterEach(() => {
      messages.clear();
      messages.sessionId = null;
    });

    it("renders the session cost when usage reports a priced cost", async () => {
      sessionsService.getApiV1SessionsIdUsage.mockResolvedValue(
        makeUsage({ has_cost: true, cost_usd: 1.234 }),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$1.23");
      });

      unmount(component);
    });

    it("renders the cost badge between the token badges and the model badge", async () => {
      sessionsService.getApiV1SessionsIdUsage.mockResolvedValue(
        makeUsage({ has_cost: true, cost_usd: 4.12 }),
      );
      messages.sessionId = "run:123456789abcdef";
      messages.messages = [makeAssistantMessage("claude-opus-4-8")];

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            peak_context_tokens: 2400,
            total_output_tokens: 180,
            has_peak_context_tokens: true,
            has_total_output_tokens: true,
          }),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")).toBeTruthy();
      });

      const meta = document.querySelector(".breadcrumb-meta");
      expect(meta).toBeTruthy();
      const children = Array.from(meta!.children);
      const desktopTokenIdx = children.findIndex((el) =>
        el.classList.contains("token-badge--desktop"),
      );
      const mobileTokenIdx = children.findIndex((el) =>
        el.classList.contains("token-badge--mobile"),
      );
      const costIdx = children.findIndex((el) =>
        el.classList.contains("cost-badge"),
      );
      const modelIdx = children.findIndex((el) =>
        el.classList.contains("model-badge"),
      );

      expect(desktopTokenIdx).toBeGreaterThanOrEqual(0);
      expect(mobileTokenIdx).toBeGreaterThan(desktopTokenIdx);
      expect(costIdx).toBeGreaterThan(mobileTokenIdx);
      expect(modelIdx).toBeGreaterThan(costIdx);

      unmount(component);
    });

    it("renders no cost badge when the session has no priced cost", async () => {
      sessionsService.getApiV1SessionsIdUsage.mockResolvedValue(
        makeUsage({ has_cost: false, cost_usd: 0 }),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await flushPromises();
      await vi.waitFor(() => {
        expect(
          sessionsService.getApiV1SessionsIdUsage,
        ).toHaveBeenCalled();
      });
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      unmount(component);
    });

    it("renders no cost badge when the usage request fails", async () => {
      sessionsService.getApiV1SessionsIdUsage.mockRejectedValue(
        new Error("boom"),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await flushPromises();
      await vi.waitFor(() => {
        expect(
          sessionsService.getApiV1SessionsIdUsage,
        ).toHaveBeenCalled();
      });
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      unmount(component);
    });

    it("ignores a stale usage response after switching sessions", async () => {
      const first = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsIdUsage
        .mockReturnValueOnce(first.promise)
        .mockResolvedValueOnce(
          makeUsage({
            session_id: "run:bbb",
            has_cost: true,
            cost_usd: 2,
          }),
        );

      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: {
          session: makeSession("claude", { id: "run:aaa" }),
          onBack: () => {},
        },
      });
      await flushPromises();

      component.$set({
        session: makeSession("claude", { id: "run:bbb" }),
      });
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$2.00");
      });

      // The first session's response arrives late and must not
      // overwrite the newer session's cost.
      first.resolve(
        makeUsage({
          session_id: "run:aaa",
          has_cost: true,
          cost_usd: 9.99,
        }),
      );
      await flushPromises();
      expect(
        document.querySelector(".cost-badge")?.textContent?.trim(),
      ).toBe("$2.00");

      component.$destroy();
    });

    it("refetches when a resync changes context tokens without output movement", async () => {
      sessionsService.getApiV1SessionsIdUsage
        .mockResolvedValueOnce(
          makeUsage({ has_cost: true, cost_usd: 1 }),
        )
        .mockResolvedValueOnce(
          makeUsage({ has_cost: true, cost_usd: 1.75 }),
        );

      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: {
          session: makeSession("claude", { peak_context_tokens: 1000 }),
          onBack: () => {},
        },
      });
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$1.00");
      });

      // A resync grows context tokens in place: same message count
      // and output tokens, only peak context moves.
      component.$set({
        session: makeSession("claude", { peak_context_tokens: 2000 }),
      });
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$1.75");
      });
      expect(
        sessionsService.getApiV1SessionsIdUsage,
      ).toHaveBeenCalledTimes(2);

      component.$destroy();
    });

    it("refetches on return navigation and rejects the other session's late response", async () => {
      const bRequest = deferred<SessionUsage>();
      const aRefetch = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsIdUsage
        .mockResolvedValueOnce(
          makeUsage({
            session_id: "run:aaa",
            has_cost: true,
            cost_usd: 1.5,
          }),
        )
        .mockReturnValueOnce(bRequest.promise)
        .mockReturnValueOnce(aRefetch.promise);

      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: {
          session: makeSession("claude", { id: "run:aaa" }),
          onBack: () => {},
        },
      });
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$1.50");
      });

      // Switch to B (request stays in flight), then back to A
      // before B resolves.
      component.$set({
        session: makeSession("claude", { id: "run:bbb" }),
      });
      await flushPromises();
      component.$set({
        session: makeSession("claude", { id: "run:aaa" }),
      });
      await flushPromises();
      expect(
        sessionsService.getApiV1SessionsIdUsage,
      ).toHaveBeenCalledTimes(3);

      // B's late response must not be shown on A.
      bRequest.resolve(
        makeUsage({
          session_id: "run:bbb",
          has_cost: true,
          cost_usd: 9.99,
        }),
      );
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      // A's refetch lands and restores A's cost.
      aRefetch.resolve(
        makeUsage({
          session_id: "run:aaa",
          has_cost: true,
          cost_usd: 1.5,
        }),
      );
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$1.50");
      });

      component.$destroy();
    });

    it("keeps the newer cost when same-session responses resolve out of order", async () => {
      const first = deferred<SessionUsage>();
      const second = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsIdUsage
        .mockReturnValueOnce(first.promise)
        .mockReturnValueOnce(second.promise);

      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: {
          session: makeSession("claude", { message_count: 2 }),
          onBack: () => {},
        },
      });
      await flushPromises();

      // A live-session update bumps message_count and triggers a
      // second fetch while the first is still in flight.
      component.$set({
        session: makeSession("claude", { message_count: 3 }),
      });
      await flushPromises();
      expect(
        sessionsService.getApiV1SessionsIdUsage,
      ).toHaveBeenCalledTimes(2);

      second.resolve(makeUsage({ has_cost: true, cost_usd: 3.5 }));
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$3.50");
      });

      first.resolve(makeUsage({ has_cost: true, cost_usd: 1 }));
      await flushPromises();
      expect(
        document.querySelector(".cost-badge")?.textContent?.trim(),
      ).toBe("$3.50");

      component.$destroy();
    });
  });

});
