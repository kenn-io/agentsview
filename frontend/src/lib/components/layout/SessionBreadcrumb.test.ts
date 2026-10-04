// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { mount as mountComponent, unmount as unmountComponent, tick } from "svelte";
import { createClassComponent as createLegacyComponent } from "svelte/legacy";
// @ts-ignore
import SessionBreadcrumb from "./SessionBreadcrumb.svelte";
import type { Session } from "../../api/types.js";
import {
  OpenersService,
  SessionsService,
  type DbSessionTiming,
  type SessionToolSequencesResponse,
} from "../../api/generated/index";
import { ApiError } from "../../api/runtime.js";
import { messages } from "../../stores/messages.svelte.js";
import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { setLocale } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { testMoney } from "../../test/money.js";
import type { Money } from "../../money.js";
import { copyToClipboard } from "../../utils/clipboard.js";

const { generateForSession, getTiming } = vi.hoisted(() => ({
  generateForSession: vi.fn(),
  getTiming: vi.fn(),
}));

vi.mock("../../api/generated/sessions/sessions.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/sessions/sessions.js")>()),
  getApiV1SessionsByIdTiming: getTiming,
}));

vi.mock("../../stores/insights.svelte.js", () => ({
  insights: {
    generateForSession,
  },
}));

vi.mock("../../api/client.js", () => ({
  listOpeners: vi.fn().mockResolvedValue({ openers: [] }),
  getSessionDirectory: vi.fn().mockResolvedValue({ path: "" }),
  resumeSession: vi.fn(),
  openSession: vi.fn(),
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
}));

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    OpenersService: {
      getApiV1Openers: vi.fn(),
    },
    SessionsService: {
      getApiV1SessionsById: vi.fn(),
      getApiV1SessionsByIdMessages: vi.fn(),
      getApiV1SessionsByIdDirectory: vi.fn(),
      getApiV1SessionsByIdUsage: vi.fn(),
      getApiV1SessionsByIdToolSequences: vi.fn(),
      postApiV1SessionsByIdResume: vi.fn(),
      postApiV1SessionsByIdOpen: vi.fn(),
    },
  };
});

const openersService = OpenersService as unknown as {
  getApiV1Openers: ReturnType<typeof vi.fn>;
};

const sessionsService = SessionsService as unknown as {
  getApiV1SessionsById: ReturnType<typeof vi.fn>;
  getApiV1SessionsByIdMessages: ReturnType<typeof vi.fn>;
  getApiV1SessionsByIdDirectory: ReturnType<typeof vi.fn>;
  getApiV1SessionsByIdUsage: ReturnType<typeof vi.fn>;
  getApiV1SessionsByIdToolSequences: ReturnType<typeof vi.fn>;
  postApiV1SessionsByIdResume: ReturnType<typeof vi.fn>;
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
    compaction_count: 0,
    consecutive_failure_max: 0,
    edit_churn_count: 0,
    ended_with_role: "",
    final_failure_streak: 0,
    has_peak_context_tokens: false,
    has_total_output_tokens: false,
    mid_task_compaction_count: 0,
    outcome: "",
    outcome_confidence: "",
    secret_leak_count: 0,
    tool_failure_signal_count: 0,
    tool_retry_count: 0,
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
  cost: Money;
  has_cost: boolean;
  rollup_cost?: Money;
  has_rollup_cost?: boolean;
  rollup_subagent_count?: number;
  models: string[];
  unpriced_models: string[];
  breakdown_count: number;
  breakdown: SessionUsageBreakdownEntry[];
  server_running: boolean;
}

interface SessionUsageBreakdownEntry {
  ordinal: number;
  message_ordinal?: number;
  source: string;
  label: string;
  timestamp: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens: number;
  cache_read_input_tokens: number;
  cost: Money;
  has_cost: boolean;
}

function makeUsage(overrides: Partial<SessionUsage> = {}): SessionUsage {
  return {
    session_id: "run:123456789abcdef",
    agent: "claude",
    project: "proj-a",
    total_output_tokens: 0,
    peak_context_tokens: 0,
    has_token_data: false,
    cost: testMoney(0),
    has_cost: false,
    models: [],
    unpriced_models: [],
    breakdown_count: 0,
    breakdown: [],
    server_running: true,
    ...overrides,
  };
}

// Call rows render only inside an expanded sequence, so expand it before reading their text.
async function expandedSequenceText(): Promise<string> {
  const row = document.querySelector<HTMLButtonElement>(".tool-sequences-panel .sequence-row");
  if (row?.getAttribute("aria-expanded") === "false") {
    row.click();
    await tick();
  }
  return document.body.textContent ?? "";
}

function makeToolSequences(
  ordinal = 3,
  sessionId = "run:123456789abcdef",
  transcriptRevision = "revision-1",
): SessionToolSequencesResponse {
  return {
    session_id: sessionId,
    transcript_revision: transcriptRevision,
    total_tool_calls: 1,
    total_sequences: 1,
    omitted_sequences: 0,
    total_sequence_calls: 1,
    omitted_calls: 0,
    sequences: [
      {
        ending: "recovered",
        identical: false,
        near_identical: false,
        tool_changed: false,
        total_calls: 1,
        omitted_calls: 0,
        calls: [
          {
            ordinal,
            call_index: 0,
            tool_use_id: "tool-id",
            tool_name: "Grep",
            outcome: "empty",
            repeat: "none",
            tool_changed: false,
            input_preview: "{}",
            input_bytes: 2,
            input_omitted_bytes: 0,
            result_preview: "No matches found",
            result_bytes: 15,
            result_omitted_bytes: 0,
            result_content_unknown: false,
          },
        ],
      },
    ],
  };
}

// The timing view's snapshot, measuring the one Grep call at message 3.
function makeTiming(sessionId: string, durationMs: number | null): DbSessionTiming {
  return {
    session_id: sessionId,
    running: true,
    turns: [
      {
        message_id: 3,
        ordinal: 3,
        started_at: "2026-02-20T12:30:30Z",
        duration_ms: durationMs,
        primary_category: "Grep",
        calls: [
          {
            tool_use_id: "tool-id",
            tool_name: "Grep",
            category: "Grep",
            duration_ms: durationMs,
            input_preview: "",
            is_parallel: false,
          },
        ],
      },
    ],
  } as unknown as DbSessionTiming;
}

async function loadTiming(sessionId: string, durationMs: number | null) {
  getTiming.mockResolvedValueOnce(makeTiming(sessionId, durationMs));
  await sessionTiming.load(sessionId);
}

// Stand in for the message list having loaded rows from one transcript revision.
function showMessages(sessionId: string, revision: string) {
  messages.sessionId = sessionId;
  messages.loadedRevision = revision;
}

function jumpLink(ordinal: number): HTMLAnchorElement | null {
  return document.querySelector<HTMLAnchorElement>(
    `.tool-sequences-panel a[aria-label="Message ${ordinal}: open the Grep call in the transcript"]`,
  );
}

function panelBusy(): string | null | undefined {
  return document.querySelector(".tool-sequences-panel")?.getAttribute("aria-busy");
}

async function openUsageBreakdown(): Promise<void> {
  const details = document.querySelector<HTMLDetailsElement>(".usage-breakdown");
  expect(details).not.toBeNull();
  details!.open = true;
  details!.dispatchEvent(new Event("toggle"));
  await tick();
  // Opening triggers the lazy breakdown fetch; let it settle.
  await Promise.resolve();
  await tick();
}

function makeAssistantMessage(model: string, reasoning_effort?: string) {
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
    reasoning_effort,
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
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function flushPromises() {
  await Promise.resolve();
  await tick();
}

beforeEach(() => {
  generateForSession.mockReset();
  vi.mocked(copyToClipboard).mockReset().mockResolvedValue(true);
  openersService.getApiV1Openers.mockReset().mockResolvedValue({ openers: [] });
  sessionsService.getApiV1SessionsByIdDirectory.mockReset().mockResolvedValue({ path: "" });
  sessionsService.getApiV1SessionsByIdUsage.mockReset().mockResolvedValue(makeUsage());
  sessionsService.getApiV1SessionsByIdToolSequences
    .mockReset()
    .mockResolvedValue(makeToolSequences());
  messages.clear();
  sessionTiming.reset();
  getTiming.mockReset();
  sessionsService.postApiV1SessionsByIdResume.mockReset();
  sessions.activeSessionId = null;
  sessions.activeSessionUsageVersion = 0;
  sessions.childSessions = new Map();
  ui.sidebarOpen = true;
  ui.isMobileViewport = false;
  ui.signalPanelOpen = false;
});

// A component a test leaves mounted keeps reacting to the shared stores, so each test tears down what it left.
const live = new Map<object, () => void>();

const mount: typeof mountComponent = (component, options) => {
  const instance = mountComponent(component, options);
  live.set(instance, () => void unmountComponent(instance));
  return instance;
};

const unmount: typeof unmountComponent = (instance, options) => {
  live.delete(instance);
  return unmountComponent(instance, options);
};

const createClassComponent = ((options: Parameters<typeof createLegacyComponent>[0]) => {
  const instance = createLegacyComponent(options);
  const destroy = instance.$destroy.bind(instance);
  instance.$destroy = () => {
    live.delete(instance);
    destroy();
  };
  live.set(instance, destroy);
  return instance;
}) as typeof createLegacyComponent;

afterEach(() => {
  for (const teardown of [...live.values()]) teardown();
  live.clear();
  setLocale("en");
  document.body.innerHTML = "";
  ui.sidebarOpen = true;
  ui.isMobileViewport = false;
});

describe("SessionBreadcrumb", () => {
  it("places the desktop expand control to the left of the relocated filter", async () => {
    ui.sidebarOpen = false;

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });
    await tick();

    const controls = document.querySelector<HTMLElement>(".sidebar-controls");
    const expandButton = controls?.querySelector<HTMLButtonElement>(
      'button[aria-label="Open sidebar"]',
    );
    const filterButton = controls?.querySelector<HTMLButtonElement>(".filter-btn");

    expect(controls).not.toBeNull();
    expect(expandButton).not.toBeNull();
    expect(filterButton).not.toBeNull();
    expect(expandButton?.nextElementSibling).toBe(filterButton);
    expect(expandButton?.title).toBe("Toggle sidebar (b)");

    expandButton!.click();
    await tick();

    expect(ui.sidebarOpen).toBe(true);
    await unmount(component);
  });

  it("does not duplicate collapsed sidebar controls below the mobile title bar", async () => {
    ui.sidebarOpen = false;
    ui.isMobileViewport = true;

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });
    await tick();

    expect(document.querySelector(".sidebar-controls")).toBeNull();
    expect(document.querySelector('button[aria-label="Open sidebar"]')).toBeNull();

    await unmount(component);
  });

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
    sessionsService.getApiV1SessionsByIdDirectory.mockResolvedValue({
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

    const backButton = document.querySelector<HTMLButtonElement>(".breadcrumb-link");
    expect(backButton?.textContent?.trim()).toBe("会话");
    expect(backButton?.getAttribute("title")).toBe("返回会话列表");

    const linkButton = document.querySelector<HTMLButtonElement>(".link-btn");
    expect(linkButton?.getAttribute("aria-label")).toBe("复制会话链接");
    expect(linkButton?.getAttribute("title")).toBe("复制会话链接");

    const findButton = document.querySelector<HTMLButtonElement>(".find-btn");
    expect(findButton?.getAttribute("aria-label")).toBe("在会话中查找");
    expect(findButton?.getAttribute("title")).toBe("在会话中查找 (/)");

    const resumeButton = document.querySelector<HTMLButtonElement>(".resume-btn");
    expect(resumeButton?.textContent?.replace(/\s+/g, " ").trim()).toBe("继续");
    resumeButton?.click();
    await tick();

    expect(document.body.textContent).toContain("默认终端");
    expect(document.body.textContent).toContain("复制命令");
    expect(document.body.textContent).toContain("复制目录路径");
    expect(document.body.textContent).toContain("打开方式");
    expect(document.body.textContent).toContain("VS Code");

    const actionsButton = document.querySelector<HTMLButtonElement>(".actions-btn");
    expect(actionsButton?.getAttribute("aria-label")).toBe("会话操作");
    actionsButton?.click();
    await tick();

    expect(document.body.textContent).toContain("重命名");
    expect(document.body.textContent).toContain("删除");

    unmount(component);
  });

  it("renders the recorded effort beside the model", async () => {
    sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(makeUsage());
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("model-test", "high")];

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await vi.waitFor(() => {
      expect(document.querySelector(".model-badge")?.textContent?.trim()).toBe("model-test high");
    });
    unmount(component);
  });

  it("shows clipboard failures as errors when copying the directory path", async () => {
    vi.mocked(copyToClipboard).mockResolvedValue(false);
    sessionsService.getApiV1SessionsByIdDirectory.mockResolvedValue({
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
      expect(sessionsService.getApiV1SessionsByIdDirectory).toHaveBeenCalled();
    });
    await flushPromises();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    await vi.waitFor(() => {
      expect(document.querySelector('[data-testid="claude-code-link"]')).toBeTruthy();
    });

    const copyPathButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Copy directory path"));
    expect(copyPathButton).toBeTruthy();
    copyPathButton!.click();

    await vi.waitFor(() => {
      const feedback = document.querySelector<HTMLButtonElement>(".resume-btn");
      expect(feedback?.textContent?.trim()).toBe("Failed");
      expect(feedback?.classList.contains("has-feedback-error")).toBe(true);
      expect(feedback?.classList.contains("has-feedback-success")).toBe(false);
      expect(feedback?.querySelector(".lucide-triangle-alert")).toBeTruthy();
      expect(feedback?.querySelector(".lucide-check")).toBeNull();
    });

    unmount(component);
  });

  it("keeps whole-session resume request bodies unchanged", async () => {
    sessionsService.postApiV1SessionsByIdResume.mockResolvedValue({
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

    const resumeItem = document.querySelector<HTMLButtonElement>(".open-menu-item");
    expect(resumeItem).toBeTruthy();
    resumeItem!.click();
    await Promise.resolve();
    await tick();

    expect(sessionsService.postApiV1SessionsByIdResume).toHaveBeenCalledWith(
      { id: "run:123456789abcdef" },
      {},
    );

    unmount(component);
  });

  it("keeps the backend default resume command authoritative when a local model exists", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    messages.historyComplete = true;
    sessionsService.postApiV1SessionsByIdResume.mockResolvedValue({
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

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const defaultTerminal = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Default terminal"));
    defaultTerminal?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume run:123456789abcdef");
    });

    unmount(component);
    messages.clear();
  });

  it("pins the active model when the backend resume request fails", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    messages.historyComplete = true;
    sessionsService.postApiV1SessionsByIdResume.mockRejectedValue(new Error("backend unavailable"));

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const defaultTerminal = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Default terminal"));
    defaultTerminal?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith(
        "claude --resume 'run:123456789abcdef' --model 'claude sonnet'",
      );
    });

    unmount(component);
    messages.clear();
  });

  it("does not pin a partial-history model when older messages remain unloaded", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    messages.historyComplete = false;
    messages.hasOlder = true;
    sessionsService.postApiV1SessionsByIdResume.mockRejectedValue(new Error("backend unavailable"));

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", { message_count: 3001 }),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const defaultTerminal = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Default terminal"));
    defaultTerminal?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume 'run:123456789abcdef'");
    });
    expect(document.querySelector(".model-badge")?.textContent).toBe("claude sonnet");

    unmount(component);
    messages.clear();
  });

  it("does not pin a reloading stable model in the resume fallback", async () => {
    vi.mocked(copyToClipboard).mockClear();
    const session = makeSession("claude", { message_count: 1 });
    vi.mocked(SessionsService.getApiV1SessionsById, { partial: true }).mockResolvedValueOnce({
      id: session.id,
      message_count: session.message_count,
    });
    vi.mocked(SessionsService.getApiV1SessionsByIdMessages).mockResolvedValueOnce({
      messages: [makeAssistantMessage("claude sonnet")],
      count: 1,
    });
    await messages.loadSession(session.id);
    messages.loading = true;
    sessionsService.postApiV1SessionsByIdResume.mockRejectedValue(new Error("backend unavailable"));

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session,
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const defaultTerminal = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Default terminal"));
    defaultTerminal?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume 'run:123456789abcdef'");
    });
    expect(document.querySelector(".model-badge")?.textContent).toBe("claude sonnet");

    unmount(component);
    messages.clear();
  });

  it("pins the active model when handleResumeIn falls back locally", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    messages.historyComplete = true;
    openersService.getApiV1Openers.mockResolvedValue({
      openers: [
        {
          id: "test-terminal",
          name: "Test Terminal",
          kind: "terminal",
          bin: "wt.exe",
        },
      ],
    });
    sessionsService.postApiV1SessionsByIdResume.mockRejectedValue(new Error("backend unavailable"));

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await vi.waitFor(() => {
      expect(document.querySelector(".resume-btn")).toBeTruthy();
    });
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await vi.waitFor(() => {
      const opener = Array.from(
        document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
      ).find((button) => button.textContent?.includes("Test Terminal"));
      expect(opener).toBeTruthy();
    });
    const opener = Array.from(document.querySelectorAll<HTMLButtonElement>(".open-menu-item")).find(
      (button) => button.textContent?.includes("Test Terminal"),
    );
    opener!.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith(
        "claude --resume 'run:123456789abcdef' --model 'claude sonnet'",
      );
    });

    unmount(component);
    messages.clear();
  });

  it("keeps backend opener commands authoritative when a local model exists", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    openersService.getApiV1Openers.mockResolvedValue({
      openers: [
        {
          id: "test-terminal",
          name: "Test Terminal",
          kind: "terminal",
          bin: "wt.exe",
        },
      ],
    });
    sessionsService.postApiV1SessionsByIdResume.mockResolvedValue({
      launched: false,
      command: "claude --resume run:123456789abcdef",
      cwd: "/tmp/project",
    });

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await vi.waitFor(() => {
      expect(openersService.getApiV1Openers).toHaveBeenCalled();
    });
    await flushPromises();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const opener = Array.from(document.querySelectorAll<HTMLButtonElement>(".open-menu-item")).find(
      (button) => button.textContent?.includes("Test Terminal"),
    );
    opener?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume run:123456789abcdef");
    });

    unmount(component);
    messages.clear();
  });

  it("pins the active model when handleCopyResumeCommand falls back locally", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    messages.historyComplete = true;
    sessionsService.postApiV1SessionsByIdResume.mockRejectedValue(new Error("backend unavailable"));

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const copyCommand = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Copy command"));
    copyCommand?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith(
        "claude --resume 'run:123456789abcdef' --model 'claude sonnet'",
      );
    });

    unmount(component);
    messages.clear();
  });

  it("keeps backend command-only responses authoritative when a local model exists", async () => {
    vi.mocked(copyToClipboard).mockClear();
    messages.sessionId = "run:123456789abcdef";
    messages.messages = [makeAssistantMessage("claude sonnet")];
    sessionsService.postApiV1SessionsByIdResume.mockResolvedValue({
      launched: false,
      command: "claude --resume run:123456789abcdef",
      cwd: "/tmp/project",
    });

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();
    const copyCommand = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".open-menu-item"),
    ).find((button) => button.textContent?.includes("Copy command"));
    copyCommand?.click();
    await vi.waitFor(() => {
      expect(copyToClipboard).toHaveBeenCalledWith("claude --resume run:123456789abcdef");
    });

    unmount(component);
    messages.clear();
  });

  it("offers a Codex Desktop deep link for a local terminal-created session", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("codex", {
          id: "codex:terminal-session-123",
        }),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();

    const link = document.querySelector<HTMLAnchorElement>('[data-testid="codex-desktop-link"]');
    expect(link).toBeTruthy();
    expect(link?.getAttribute("href")).toBe("codex://threads/terminal-session-123");
    expect(link?.textContent).toContain("Codex Desktop");

    const menuLabels = Array.from(document.querySelectorAll(".open-menu-name")).map((node) =>
      node.textContent?.trim(),
    );
    const codexMenuIndex = menuLabels.findIndex((label) => label?.includes("Codex Desktop"));
    expect(codexMenuIndex).toBeLessThan(menuLabels.indexOf("Copy command"));

    await unmount(component);
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
    expect(badge?.getAttribute("style")).toContain("var(--accent-rose)");
    expect(badge?.getAttribute("style")).toContain("var(--accent-rose-foreground)");

    unmount(component);
  });

  it("offers a Claude Code deep link using the session directory", async () => {
    sessionsService.getApiV1SessionsByIdDirectory.mockResolvedValue({
      path: "/tmp/claude project",
    });

    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude"),
        onBack: () => {},
      },
    });

    await tick();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();

    await vi.waitFor(() => {
      expect(
        document
          .querySelector<HTMLAnchorElement>('[data-testid="claude-code-link"]')
          ?.getAttribute("href"),
      ).toBe("claude://code/new?folder=%2Ftmp%2Fclaude%20project");
    });

    await unmount(component);
  });

  it("keeps the directory read across same-session metadata refreshes", async () => {
    const directory = deferred<{ path: string }>();
    sessionsService.getApiV1SessionsByIdDirectory.mockReturnValue(directory.promise);

    const component = createClassComponent({
      component: SessionBreadcrumb,
      target: document.body,
      props: {
        session: makeSession("claude", { message_count: 2 }),
        onBack: () => {},
      },
    });
    await flushPromises();

    component.$set({
      session: makeSession("claude", { message_count: 3 }),
    });
    await flushPromises();
    component.$set({
      session: makeSession("claude", { message_count: 4 }),
    });
    await flushPromises();

    expect(sessionsService.getApiV1SessionsByIdDirectory).toHaveBeenCalledOnce();

    directory.resolve({ path: "/tmp/refreshed-session" });
    await flushPromises();
    document.querySelector<HTMLButtonElement>(".resume-btn")?.click();
    await tick();

    await vi.waitFor(() => {
      expect(
        document
          .querySelector<HTMLAnchorElement>('[data-testid="claude-code-link"]')
          ?.getAttribute("href"),
      ).toBe("claude://code/new?folder=%2Ftmp%2Frefreshed-session");
    });

    component.$destroy();
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
    expect(badge?.getAttribute("style")).toContain("var(--accent-blue)");
    expect(badge?.getAttribute("style")).toContain("var(--accent-blue-foreground)");

    unmount(component);
  });

  it("renders Claude session identity overrides in the badges", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", {
          agent_label: "triage",
          entrypoint: "sdk-cli",
        }),
        onBack: () => {},
      },
    });

    await tick();
    const badges = Array.from(document.querySelectorAll(".agent-badge"));
    expect(badges[0]?.textContent?.trim()).toBe("triage");
    expect(document.querySelector(".entrypoint-badge")?.textContent?.trim()).toBe("sdk-cli");

    unmount(component);
  });

  it("suppresses the default cli entrypoint badge", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", { entrypoint: "cli" }),
        onBack: () => {},
      },
    });

    await tick();
    expect(document.querySelector(".entrypoint-badge")).toBeNull();

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
      linkBtn!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
      await vi.advanceTimersByTimeAsync(0);
      await tick();
      expect(linkBtn!.classList.contains("link-btn--copied")).toBe(true);

      // Advance 1s, then copy again
      await vi.advanceTimersByTimeAsync(1000);
      linkBtn!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
      await vi.advanceTimersByTimeAsync(0);
      await tick();

      // 600ms after second click — first timer's 1.5s
      // would have expired, but it was cleared
      await vi.advanceTimersByTimeAsync(600);
      await tick();
      expect(linkBtn!.classList.contains("link-btn--copied")).toBe(true);

      // After full 1.5s from second click, state clears
      await vi.advanceTimersByTimeAsync(900);
      await tick();
      expect(linkBtn!.classList.contains("link-btn--copied")).toBe(false);

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
    expect(tokenBadge?.textContent?.replace(/\s+/g, " ").trim()).toBe("2.4k ctx / 180 out");

    unmount(component);
  });

  it("starts single-session agent analysis from the top bar", async () => {
    const navigateSpy = vi.spyOn(router, "navigate");
    const session = makeSession("claude");
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session,
        onBack: () => {},
      },
    });

    await tick();
    const button = document.querySelector<HTMLButtonElement>(".insight-btn");
    expect(button).toBeTruthy();

    button!.click();

    expect(generateForSession).toHaveBeenCalledWith(session);
    expect(navigateSpy).toHaveBeenCalledWith("recall", {
      tab: "generated",
    });

    navigateSpy.mockRestore();
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
    expect(tokenBadge?.textContent?.replace(/\s+/g, " ").trim()).toBe("— ctx / 180 out");

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

    const mobileTokenBadge = document.querySelector(".token-badge--mobile");
    expect(mobileTokenBadge?.textContent?.replace(/\s+/g, " ").trim()).toBe("2.4k ctx / 180 out");

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
      expect(badge?.getAttribute("title")).toBe("3 lines in the source file could not be parsed");
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
      expect(badge?.getAttribute("title")).toBe("1 line in the source file could not be parsed");
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
      expect(badge?.textContent?.trim().toLowerCase()).toContain("unverified schema");
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

  it.each([false, true])(
    "copies remote commands with fallback=%s and excludes local actions",
    async (fallback) => {
      const resume = vi.mocked(SessionsService.postApiV1SessionsByIdResume);
      const command = "cd '/home/user/project' && claude --resume abc-123";
      if (fallback) resume.mockRejectedValueOnce(new Error("offline"));
      else resume.mockResolvedValueOnce({ launched: false, command, cwd: "/home/user/project" });
      openersService.getApiV1Openers.mockResolvedValue({
        openers: [
          { id: "kitty", name: "Kitty", kind: "terminal", bin: "kitty" },
          { id: "code", name: "VS Code", kind: "editor", bin: "code" },
          { id: "finder", name: "Finder", kind: "files", bin: "open" },
          { id: "claude-desktop", name: "Claude Desktop", kind: "action", bin: "open" },
        ],
      });
      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            id: "devbox1~claude:abc-123",
            machine: "devbox1",
            file_path: "/remote/session.jsonl",
          }),
          onBack: () => {},
        },
      });

      try {
        await flushPromises();
        const trigger = document.querySelector<HTMLButtonElement>(".resume-btn");
        expect(trigger).not.toBeNull();
        trigger!.click();
        await tick();
        expect(
          Array.from(document.querySelectorAll(".open-menu-name"), (el) => el.textContent),
        ).toEqual(["Copy command"]);
        expect(document.querySelector(".open-menu-divider")).toBeNull();
        document.dispatchEvent(new KeyboardEvent("keydown", { key: "1" }));
        await tick();
        expect(resume).not.toHaveBeenCalled();
        document.querySelector<HTMLButtonElement>(".open-menu-item")!.click();
        await vi.waitFor(() =>
          expect(copyToClipboard).toHaveBeenCalledWith(
            fallback ? "claude --resume abc-123" : command,
          ),
        );
        expect(resume).toHaveBeenCalledExactlyOnceWith(
          { id: "devbox1~claude:abc-123" },
          { command_only: true },
        );
        expect(SessionsService.postApiV1SessionsByIdOpen).not.toHaveBeenCalled();
        expect(trigger!.textContent).toContain("Command copied!");
      } finally {
        await unmount(component);
      }
    },
  );

  it("hides the remote menu for unsupported agents", async () => {
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: { session: makeSession("unknown", { id: "devbox1~unsupported" }), onBack: () => {} },
    });
    await flushPromises();
    expect(document.querySelector(".resume-btn")).toBeNull();
    await unmount(component);
  });

  it("copies remote Kiro commands", async () => {
    const resume = vi.mocked(SessionsService.postApiV1SessionsByIdResume);
    resume.mockResolvedValueOnce({
      launched: false,
      command: "cd '/home/user/project' && kiro-cli chat --resume-id session-1",
      cwd: "/home/user/project",
    });
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("kiro", {
          id: "devbox1~kiro:session-1",
          machine: "devbox1",
        }),
        onBack: () => {},
      },
    });
    try {
      await flushPromises();
      document.querySelector<HTMLButtonElement>(".resume-btn")!.click();
      await tick();
      document.querySelector<HTMLButtonElement>(".open-menu-item")!.click();
      await vi.waitFor(() =>
        expect(copyToClipboard).toHaveBeenCalledWith(
          "cd '/home/user/project' && kiro-cli chat --resume-id session-1",
        ),
      );
      expect(resume).toHaveBeenCalledExactlyOnceWith(
        {
          id: "devbox1~kiro:session-1",
        },
        { command_only: true },
      );
    } finally {
      await unmount(component);
    }
  });

  it("keeps local launch and file actions with remote-looking machine metadata", async () => {
    openersService.getApiV1Openers.mockResolvedValue({
      openers: [
        { id: "kitty", name: "Kitty", kind: "terminal", bin: "kitty" },
        { id: "code", name: "VS Code", kind: "editor", bin: "code" },
        { id: "finder", name: "Finder", kind: "files", bin: "open" },
        { id: "claude-desktop", name: "Claude Desktop", kind: "action", bin: "open" },
      ],
    });
    vi.mocked(SessionsService.postApiV1SessionsByIdResume).mockResolvedValueOnce({
      launched: true,
      command: "claude --resume abc-123",
    });
    const component = mount(SessionBreadcrumb, {
      target: document.body,
      props: {
        session: makeSession("claude", { id: "claude:abc-123", machine: "devbox1~remote" }),
        onBack: () => {},
      },
    });
    try {
      await flushPromises();
      document.querySelector<HTMLButtonElement>(".resume-btn")!.click();
      await tick();
      expect(
        Array.from(document.querySelectorAll(".open-menu-name"), (el) => el.textContent),
      ).toEqual([
        "Kitty",
        "Default terminal",
        "Open in Claude Code",
        "Copy command",
        "Copy directory path",
        "VS Code",
        "Finder",
        "Claude Desktop",
      ]);
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "1" }));
      await vi.waitFor(() =>
        expect(SessionsService.postApiV1SessionsByIdResume).toHaveBeenCalledExactlyOnceWith(
          {
            id: "claude:abc-123",
          },
          { opener_id: "kitty" },
        ),
      );
    } finally {
      await unmount(component);
    }
  });

  describe("cost badge", () => {
    afterEach(() => {
      messages.clear();
      messages.sessionId = null;
    });

    it("renders the session cost when usage reports a priced cost", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({ has_cost: true, cost: testMoney(1.234) }),
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

    it("renders a total badge for a complete subagent rollup", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({
          has_cost: true,
          cost: testMoney(1),
          has_rollup_cost: true,
          rollup_cost: testMoney(3),
          rollup_subagent_count: 2,
        }),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: { session: makeSession("claude"), onBack: () => {} },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")?.textContent).toContain("$3.00");
      });
      expect(document.querySelector(".cost-badge")?.getAttribute("title")).toBe(
        "Total cost including 2 subagents",
      );
      expect(document.querySelector(".cost-badge")?.textContent).toContain("Total");
      expect(sessionsService.getApiV1SessionsByIdUsage.mock.lastCall?.slice(0, 2)).toEqual([
        { id: "run:123456789abcdef" },
        { rollup: true },
      ]);
      unmount(component);
    });

    it("keeps the root cost when the rollup is incomplete", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({
          has_cost: true,
          cost: testMoney(1),
          has_rollup_cost: false,
          rollup_subagent_count: 1,
        }),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: { session: makeSession("claude"), onBack: () => {} },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")?.textContent?.trim()).toBe("$1.00");
      });
      expect(document.querySelector(".cost-badge")?.textContent).not.toContain("Total");
      unmount(component);
    });

    it("renders the cost badge between the token badges and the model badge", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({ has_cost: true, cost: testMoney(4.12) }),
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
      const costIdx = children.findIndex((el) => el.classList.contains("cost-badge"));
      const modelIdx = children.findIndex((el) => el.classList.contains("model-badge"));

      expect(desktopTokenIdx).toBeGreaterThanOrEqual(0);
      expect(mobileTokenIdx).toBeGreaterThan(desktopTokenIdx);
      expect(costIdx).toBeGreaterThan(mobileTokenIdx);
      expect(modelIdx).toBeGreaterThan(costIdx);

      unmount(component);
    });

    it("renders no cost badge when the session has no priced cost", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({ has_cost: false, cost: testMoney(0) }),
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
        expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalled();
      });
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      unmount(component);
    });

    it("renders no cost badge when the usage request fails", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockRejectedValue(new Error("boom"));

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await flushPromises();
      await vi.waitFor(() => {
        expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalled();
      });
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      unmount(component);
    });

    it("renders the session usage breakdown lazily when the menu opens", async () => {
      const rows: SessionUsageBreakdownEntry[] = [
        {
          ordinal: 1,
          message_ordinal: 0,
          source: "message",
          label: "Prompt 1",
          timestamp: "2026-02-20T12:30:00Z",
          model: "claude-opus-4-6",
          input_tokens: 1000,
          output_tokens: 500,
          cache_creation_input_tokens: 200,
          cache_read_input_tokens: 300,
          cost: testMoney(0.017),
          has_cost: true,
        },
        {
          ordinal: 2,
          source: "session",
          label: "session",
          timestamp: "2026-02-20T12:31:00Z",
          model: "gpt-5.4",
          input_tokens: 150,
          output_tokens: 20,
          cache_creation_input_tokens: 0,
          cache_read_input_tokens: 0,
          cost: testMoney(0.005),
          has_cost: true,
        },
      ];
      sessionsService.getApiV1SessionsByIdUsage.mockImplementation(
        (_path: { id: string }, { breakdown }: { breakdown?: boolean }) =>
          Promise.resolve(
            makeUsage({
              has_cost: true,
              cost: testMoney(0.022),
              breakdown_count: 2,
              breakdown: breakdown ? rows : [],
            }),
          ),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude", {
            peak_context_tokens: 1500,
            total_output_tokens: 520,
            has_peak_context_tokens: true,
            has_total_output_tokens: true,
          }),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".usage-breakdown-trigger")?.textContent?.trim()).toBe(
          "2 steps",
        );
      });
      expect(document.querySelectorAll(".usage-breakdown-row")).toHaveLength(0);
      expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalledTimes(1);
      expect(sessionsService.getApiV1SessionsByIdUsage.mock.calls[0]?.slice(0, 2)).toEqual([
        { id: "run:123456789abcdef" },
        { rollup: true },
      ]);

      await openUsageBreakdown();
      expect(sessionsService.getApiV1SessionsByIdUsage.mock.lastCall?.slice(0, 2)).toEqual([
        { id: "run:123456789abcdef" },
        { breakdown: true },
      ]);
      const renderedRows = Array.from(document.querySelectorAll(".usage-breakdown-row"));
      expect(renderedRows).toHaveLength(2);
      const first = renderedRows[0]!;
      const second = renderedRows[1]!;
      expect(first.textContent).toContain("Prompt 1");
      expect(first.textContent).toContain("claude-opus-4-6");
      expect(first.textContent).toContain("1,500 ctx");
      expect(first.textContent).toContain("500 out");
      expect(second.textContent).toContain("session");
      expect(second.textContent).toContain("gpt-5.4");

      unmount(component);
    });

    it("renders no usage breakdown when the usage response counts no rows", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({ breakdown_count: 0 }),
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
        expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalled();
      });
      expect(document.querySelector(".usage-breakdown")).toBeNull();

      unmount(component);
    });

    it("renders every breakdown row in the scrollable menu", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockImplementation(
        (_path: { id: string }, { breakdown }: { breakdown?: boolean }) =>
          Promise.resolve(
            makeUsage({
              breakdown_count: 8,
              breakdown: breakdown
                ? Array.from({ length: 8 }, (_, i) => ({
                    ordinal: i + 1,
                    source: "message",
                    label: `Prompt ${i + 1}`,
                    timestamp: "2026-02-20T12:30:00Z",
                    model: "claude-opus-4-6",
                    input_tokens: 100 + i,
                    output_tokens: 10 + i,
                    cache_creation_input_tokens: 0,
                    cache_read_input_tokens: 0,
                    cost: testMoney(0),
                    has_cost: false,
                  }))
                : [],
            }),
          ),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".usage-breakdown-trigger")?.textContent?.trim()).toBe(
          "8 steps",
        );
      });
      await openUsageBreakdown();
      expect(document.querySelectorAll(".usage-breakdown-row")).toHaveLength(8);

      unmount(component);
    });

    it("shows a loading placeholder until breakdown rows arrive", async () => {
      const rowsFetch = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsByIdUsage.mockImplementation(
        (_path: { id: string }, { breakdown }: { breakdown?: boolean }) =>
          breakdown ? rowsFetch.promise : Promise.resolve(makeUsage({ breakdown_count: 1 })),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".usage-breakdown-trigger")?.textContent?.trim()).toBe(
          "1 step",
        );
      });
      await openUsageBreakdown();
      expect(document.querySelector(".usage-breakdown-status")?.textContent?.trim()).toBe(
        "Loading usage...",
      );
      expect(document.querySelectorAll(".usage-breakdown-row")).toHaveLength(0);

      rowsFetch.resolve(
        makeUsage({
          breakdown_count: 1,
          breakdown: [
            {
              ordinal: 1,
              source: "message",
              label: "Prompt 1",
              timestamp: "2026-02-20T12:30:00Z",
              model: "claude-opus-4-6",
              input_tokens: 100,
              output_tokens: 10,
              cache_creation_input_tokens: 0,
              cache_read_input_tokens: 0,
              cost: testMoney(0),
              has_cost: false,
            },
          ],
        }),
      );
      await flushPromises();
      expect(document.querySelector(".usage-breakdown-status")).toBeNull();
      expect(document.querySelectorAll(".usage-breakdown-row")).toHaveLength(1);

      unmount(component);
    });

    it("shows a failure placeholder when the breakdown fetch fails", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockImplementation(
        (_path: { id: string }, { breakdown }: { breakdown?: boolean }) =>
          breakdown
            ? Promise.reject(new Error("boom"))
            : Promise.resolve(makeUsage({ breakdown_count: 3 })),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".usage-breakdown-trigger")?.textContent?.trim()).toBe(
          "3 steps",
        );
      });
      await openUsageBreakdown();
      expect(document.querySelector(".usage-breakdown-status")?.textContent?.trim()).toBe("Failed");
      expect(document.querySelectorAll(".usage-breakdown-row")).toHaveLength(0);

      unmount(component);
    });

    it("ignores a stale usage response after switching sessions", async () => {
      const first = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsByIdUsage.mockImplementation(
        ({ id }: { id: string }, { breakdown }: { breakdown?: boolean }) => {
          if (id === "run:aaa") return first.promise;
          return Promise.resolve(
            makeUsage({
              session_id: "run:bbb",
              has_cost: true,
              cost: testMoney(2),
              breakdown_count: 1,
              breakdown: breakdown
                ? [
                    {
                      ordinal: 1,
                      source: "message",
                      label: "Prompt 1",
                      timestamp: "2026-02-20T12:31:00Z",
                      model: "gpt-5.4",
                      input_tokens: 10,
                      output_tokens: 2,
                      cache_creation_input_tokens: 0,
                      cache_read_input_tokens: 0,
                      cost: testMoney(2),
                      has_cost: true,
                    },
                  ]
                : [],
            }),
          );
        },
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
      await openUsageBreakdown();
      expect(document.querySelector(".usage-breakdown-row")?.textContent).toContain("gpt-5.4");

      // The first session's response arrives late and must not
      // overwrite the newer session's cost or step count.
      first.resolve(
        makeUsage({
          session_id: "run:aaa",
          has_cost: true,
          cost: testMoney(9.99),
          breakdown_count: 42,
        }),
      );
      await flushPromises();
      expect(document.querySelector(".cost-badge")?.textContent?.trim()).toBe("$2.00");
      expect(document.querySelector(".usage-breakdown-trigger")?.textContent?.trim()).toBe(
        "1 step",
      );
      expect(document.querySelector(".usage-breakdown-row")?.textContent).toContain("gpt-5.4");

      component.$destroy();
    });

    it("refetches when a resync changes context tokens without output movement", async () => {
      sessionsService.getApiV1SessionsByIdUsage
        .mockResolvedValueOnce(makeUsage({ has_cost: true, cost: testMoney(1) }))
        .mockResolvedValueOnce(makeUsage({ has_cost: true, cost: testMoney(1.75) }));

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
      expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalledTimes(2);

      component.$destroy();
    });

    it("refetches on return navigation and rejects the other session's late response", async () => {
      const bRequest = deferred<SessionUsage>();
      const aRefetch = deferred<SessionUsage>();
      sessionsService.getApiV1SessionsByIdUsage
        .mockResolvedValueOnce(
          makeUsage({
            session_id: "run:aaa",
            has_cost: true,
            cost: testMoney(1.5),
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
      expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalledTimes(3);

      // B's late response must not be shown on A.
      bRequest.resolve(
        makeUsage({
          session_id: "run:bbb",
          has_cost: true,
          cost: testMoney(9.99),
        }),
      );
      await flushPromises();
      expect(document.querySelector(".cost-badge")).toBeNull();

      // A's refetch lands and restores A's cost.
      aRefetch.resolve(
        makeUsage({
          session_id: "run:aaa",
          has_cost: true,
          cost: testMoney(1.5),
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
      sessionsService.getApiV1SessionsByIdUsage
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
      expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalledTimes(2);

      second.resolve(makeUsage({ has_cost: true, cost: testMoney(3.5) }));
      await vi.waitFor(() => {
        const badge = document.querySelector(".cost-badge");
        expect(badge?.textContent?.trim()).toBe("$3.50");
      });

      first.resolve(makeUsage({ has_cost: true, cost: testMoney(1) }));
      await flushPromises();
      expect(document.querySelector(".cost-badge")?.textContent?.trim()).toBe("$3.50");

      component.$destroy();
    });

    it("refetches rollup cost when active session usage freshness changes", async () => {
      sessionsService.getApiV1SessionsByIdUsage
        .mockResolvedValueOnce(
          makeUsage({
            has_cost: true,
            cost: testMoney(1),
            has_rollup_cost: true,
            rollup_cost: testMoney(3),
            rollup_subagent_count: 1,
          }),
        )
        .mockResolvedValueOnce(
          makeUsage({
            has_cost: true,
            cost: testMoney(1),
            has_rollup_cost: true,
            rollup_cost: testMoney(5),
            rollup_subagent_count: 1,
          }),
        );

      sessions.activeSessionId = "run:123456789abcdef";

      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: {
          session: makeSession("claude"),
          onBack: () => {},
        },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")?.textContent).toContain("$3.00");
      });

      sessions.activeSessionUsageVersion = 1;
      await flushPromises();

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")?.textContent).toContain("$5.00");
      });
      expect(sessionsService.getApiV1SessionsByIdUsage).toHaveBeenCalledTimes(2);

      component.$destroy();
    });

    it("uses singular total-cost copy for one subagent", async () => {
      sessionsService.getApiV1SessionsByIdUsage.mockResolvedValue(
        makeUsage({
          has_cost: true,
          cost: testMoney(1),
          has_rollup_cost: true,
          rollup_cost: testMoney(3),
          rollup_subagent_count: 1,
        }),
      );

      const component = mount(SessionBreadcrumb, {
        target: document.body,
        props: { session: makeSession("claude"), onBack: () => {} },
      });

      await vi.waitFor(() => {
        expect(document.querySelector(".cost-badge")?.getAttribute("title")).toBe(
          "Total cost including 1 subagent",
        );
      });

      unmount(component);
    });

    it("loads tool sequences only while visible and skips usage-only refreshes", async () => {
      const session = makeSession("claude", { termination_status: "clean" });
      showMessages(session.id, "revision-1");
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await flushPromises();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).not.toHaveBeenCalled();

      ui.signalPanelOpen = true;
      await vi.waitFor(async () => {
        expect(await expandedSequenceText()).toContain("Message 3");
        expect(panelBusy()).toBe("false");
      });
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);

      // Other sessions changing, or this session's metadata moving ahead, says nothing new about the loaded transcript.
      sessions.activeSessionUsageVersion += 1;
      component.$set({ session: { ...session, transcript_revision: "revision-2" } });
      await flushPromises();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);
      component.$destroy();
    });

    it("takes durations from live timing and follows a running child with no parent change", async () => {
      const session = makeSession("claude", { termination_status: "tool_call_pending" });
      showMessages(session.id, "revision-1");
      await loadTiming(session.id, 1000);
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => expect(await expandedSequenceText()).toContain("1.0s"));

      // The delegated call's child keeps running: its end time advances while the parent transcript stays put.
      sessionTiming.applyEvent(makeTiming(session.id, 4000));
      await tick();
      expect(document.body.textContent).toContain("4.0s");
      expect(document.body.textContent).not.toContain("1.0s");
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);
      component.$destroy();
    });

    it("refetches when the loaded message revision or the end state changes", async () => {
      const session = makeSession("claude", { termination_status: "tool_call_pending" });
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"))
        .mockResolvedValueOnce(makeToolSequences(5, session.id, "revision-2"))
        .mockResolvedValueOnce(makeToolSequences(6, session.id, "revision-2"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });

      messages.loadedRevision = "revision-2";
      await vi.waitFor(() => expect(jumpLink(5)).not.toBeNull());
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(2);

      component.$set({ session: { ...session, termination_status: "clean" } });
      await vi.waitFor(() => expect(jumpLink(6)).not.toBeNull());
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(3);
      component.$destroy();
    });

    it("keeps an expanded sequence and its links while a refresh is in flight", async () => {
      const refresh = deferred<SessionToolSequencesResponse>();
      const session = makeSession("claude", { termination_status: "tool_call_pending" });
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"))
        .mockReturnValueOnce(refresh.promise);
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
        expect(panelBusy()).toBe("false");
      });

      component.$set({ session: { ...session, termination_status: "clean" } });
      await tick();
      expect(panelBusy()).toBe("true");
      expect(document.body.textContent).not.toContain("Loading tool sequences");
      expect(document.querySelector(".sequence-row")?.getAttribute("aria-expanded")).toBe("true");
      const scrollToOrdinal = vi.spyOn(ui, "scrollToOrdinal");
      jumpLink(3)!.click();
      expect(scrollToOrdinal).toHaveBeenCalledWith(3, session.id, "revision-1");
      scrollToOrdinal.mockRestore();

      refresh.resolve(makeToolSequences(4, session.id, "revision-1"));
      await vi.waitFor(() => expect(panelBusy()).toBe("false"));
      expect(document.querySelector(".sequence-row")?.getAttribute("aria-expanded")).toBe("true");
      expect(jumpLink(4)).not.toBeNull();
      component.$destroy();
    });

    it("withholds links from sequences newer than the loaded messages until they catch up", async () => {
      const session = makeSession("claude");
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockResolvedValueOnce(makeToolSequences(7, session.id, "revision-2"))
        .mockResolvedValueOnce(makeToolSequences(7, session.id, "revision-2"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => expect(await expandedSequenceText()).toContain("Message 7"));
      expect(document.querySelector(".tool-sequences-panel a.jump")).toBeNull();

      messages.loadedRevision = "revision-2";
      await vi.waitFor(() => expect(jumpLink(7)).not.toBeNull());
      component.$destroy();
    });

    it.each([
      ["delayed", false],
      ["failed", true],
    ])("never links into old messages while a message refresh after resync is %s", async (_, fails) => {
      vi.spyOn(console, "warn").mockImplementation(() => {});
      const session = makeSession("claude", { message_count: 4, termination_status: "tool_call_pending" });
      const rows = (revision: string) => ({
        messages: [0, 1, 2, 3].map((ordinal) => ({ ...makeAssistantMessage("claude sonnet"), ordinal })),
        count: 4,
        transcript_revision: revision,
      });
      sessionsService.getApiV1SessionsById.mockResolvedValue({ ...session, transcript_revision: "revision-1" });
      sessionsService.getApiV1SessionsByIdMessages.mockResolvedValueOnce(rows("revision-1"));
      await messages.loadSession(session.id);
      expect(messages.loadedRevision).toBe("revision-1");
      const replaced = makeToolSequences(2, session.id, "revision-2");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"))
        .mockResolvedValue(replaced);
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });

      // A resync replaces the transcript. Session metadata and the sequences reach the new revision first.
      const refresh = deferred<ReturnType<typeof rows>>();
      sessionsService.getApiV1SessionsById.mockResolvedValue({ ...session, transcript_revision: "revision-2" });
      sessionsService.getApiV1SessionsByIdMessages.mockReturnValueOnce(refresh.promise);
      const reload = messages.reload();
      component.$set({ session: { ...session, transcript_revision: "revision-2", termination_status: "clean" } });
      await vi.waitFor(async () => expect(await expandedSequenceText()).toContain("Message 2"));
      expect(document.querySelector(".tool-sequences-panel a.jump")).toBeNull();
      expect(messages.loadedRevision).toBe("revision-1");

      if (fails) refresh.reject(new Error("offline"));
      else refresh.resolve(rows("revision-2"));
      await reload;
      await flushPromises();
      if (fails) {
        expect(messages.loadedRevision).toBe("revision-1");
        expect(document.querySelector(".tool-sequences-panel a.jump")).toBeNull();
        expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(2);
      } else {
        await vi.waitFor(() => expect(jumpLink(2)).not.toBeNull());
        expect(messages.loadedRevision).toBe("revision-2");
        expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(3);
      }
      component.$destroy();
    });

    it("waits for the message list to land rows before reading sequences", async () => {
      const session = makeSession("claude");
      messages.sessionId = session.id;
      messages.loading = true;
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await flushPromises();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).not.toHaveBeenCalled();
      expect(document.body.textContent).toContain("Loading tool sequences");

      messages.loadedRevision = "revision-1";
      messages.loading = false;
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);
      component.$destroy();
    });

    it("ignores a superseded tool-sequence response for the same session", async () => {
      const revision2 = deferred<SessionToolSequencesResponse>();
      const session = makeSession("claude");
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"))
        .mockReturnValueOnce(revision2.promise)
        .mockResolvedValueOnce(makeToolSequences(9, session.id, "revision-3"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });
      messages.loadedRevision = "revision-2";
      await flushPromises();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(2);
      messages.loadedRevision = "revision-3";
      await vi.waitFor(() => expect(jumpLink(9)).not.toBeNull());

      revision2.resolve(makeToolSequences(5, session.id, "revision-2"));
      await flushPromises();
      expect(jumpLink(9)).not.toBeNull();
      expect(document.body.textContent).not.toContain("Message 5");
      component.$destroy();
    });

    it("does not let a closed-panel response replace the next read", async () => {
      const first = deferred<SessionToolSequencesResponse>();
      const session = makeSession("claude");
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockReturnValueOnce(first.promise)
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await flushPromises();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);

      ui.signalPanelOpen = false;
      await flushPromises();
      ui.signalPanelOpen = true;
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });
      first.resolve(makeToolSequences(8, session.id, "revision-1"));
      await flushPromises();
      expect(jumpLink(3)).not.toBeNull();
      expect(document.body.textContent).not.toContain("Message 8");
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(2);
      component.$destroy();
    });

    it("retries a failed tool-sequence read", async () => {
      const session = makeSession("claude");
      showMessages(session.id, "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockRejectedValueOnce(new Error("offline"))
        .mockResolvedValueOnce(makeToolSequences(3, session.id, "revision-1"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session, onBack: () => {} },
      });
      await vi.waitFor(() => {
        expect(document.querySelector('.tool-sequences-panel [role="alert"]')).not.toBeNull();
      });

      document
        .querySelector<HTMLButtonElement>('.tool-sequences-panel [role="alert"] button')!
        .click();
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });
      expect(document.querySelector('.tool-sequences-panel [role="alert"]')).toBeNull();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(2);
      component.$destroy();
    });

    it("says tool sequences are unavailable when the archive records no transcript revision", async () => {
      sessionsService.getApiV1SessionsByIdToolSequences.mockRejectedValueOnce(
        new ApiError(501, "this backend records no transcript revision", "revision_unavailable"),
      );
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session: makeSession("claude"), onBack: () => {} },
      });
      await vi.waitFor(() => {
        expect(document.querySelector(".tool-sequences-panel")?.textContent).toContain(
          "doesn't record transcript versions",
        );
      });
      expect(document.querySelector('.tool-sequences-panel [role="alert"]')).toBeNull();
      expect(sessionsService.getApiV1SessionsByIdToolSequences).toHaveBeenCalledTimes(1);
      component.$destroy();
    });

    it("ignores a late tool-sequence response after switching sessions", async () => {
      const first = deferred<SessionToolSequencesResponse>();
      showMessages("run:bbb", "revision-1");
      sessionsService.getApiV1SessionsByIdToolSequences
        .mockReturnValueOnce(first.promise)
        .mockResolvedValueOnce(makeToolSequences(3, "run:bbb", "revision-1"));
      ui.signalPanelOpen = true;
      const component = createClassComponent({
        component: SessionBreadcrumb,
        target: document.body,
        props: { session: makeSession("claude", { id: "run:aaa" }), onBack: () => {} },
      });
      await flushPromises();
      component.$set({ session: makeSession("claude", { id: "run:bbb" }) });
      await vi.waitFor(async () => {
        await expandedSequenceText();
        expect(jumpLink(3)).not.toBeNull();
      });

      first.resolve(makeToolSequences(8, "run:aaa", "revision-1"));
      await flushPromises();
      expect(jumpLink(3)).not.toBeNull();
      expect(document.body.textContent).not.toContain("Message 8");
      component.$destroy();
    });
  });
});
