import { describe, it, expect, vi, beforeEach } from "vite-plus/test";
import { messages } from "./messages.svelte.js";
import { readProgress } from "./read-progress.svelte.js";
import { parseContent } from "../utils/content-parser.js";
import type { Session } from "../api/types.js";
import type { DbMessage as Message } from "../api/generated/index.js";
import type { ServiceMessageList as MessagesResponse } from "../api/generated/index.js";
import { ApiError } from "../api/runtime.js";

const api = vi.hoisted(() => ({
  getMessages: vi.fn(),
  getSession: vi.fn(),
}));

const runtimeMocks = vi.hoisted(() => ({
  signals: [] as AbortSignal[],
}));

vi.mock("../api/runtime.js", () => ({
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      message: string,
    ) {
      super(message);
    }
  },
  isAbortError: (err: unknown) => {
    if (err instanceof DOMException && err.name === "AbortError") {
      return true;
    }
    if (err === null || typeof err !== "object") {
      return false;
    }
    const candidate = err as {
      isCancelled?: unknown;
      name?: unknown;
    };
    return candidate.isCancelled === true || candidate.name === "CancelError";
  },
}));

const sessionsStore = vi.hoisted(() => ({
  markActiveSessionMissing: vi.fn(),
}));

vi.mock("./sessions.svelte.js", () => ({
  sessions: sessionsStore,
}));

vi.mock("../api/generated/index", () => ({
  SessionsService: {
    getApiV1SessionsById: vi.fn(({ id }) => api.getSession(id)),
    getApiV1SessionsByIdMessages: vi.fn((path, params, options) =>
      api.getMessages(
        path.id,
        {
          from: params.from,
          limit: params.limit,
          direction: params.direction,
          ...(params.expected_revision ? { expected_revision: params.expected_revision } : {}),
        },
        options,
      ),
    ),
  },
}));

function createDeferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function generatedCancelError(): Error & { isCancelled: true } {
  const err = new Error("Request aborted") as Error & {
    isCancelled: true;
  };
  err.name = "CancelError";
  err.isCancelled = true;
  return err;
}

function makeSession(id: string, messageCount: number): Session {
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
    id,
    project: "project-alpha",
    machine: "test-machine",
    agent: "test-agent",
    first_message: null,
    started_at: null,
    ended_at: null,
    message_count: messageCount,
    user_message_count: messageCount,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: new Date(0).toISOString(),
  };
}

function makeMessage(ordinal: number): Message {
  return {
    id: ordinal + 1,
    session_id: "s1",
    ordinal,
    role: ordinal % 2 === 0 ? "user" : "assistant",
    content: `msg ${ordinal}`,
    timestamp: new Date(ordinal * 1000).toISOString(),
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 6,
    model: "",
    token_usage: null,
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
  };
}

function makeMessagesResponse(rows: Message[]): MessagesResponse {
  return {
    messages: rows,
    count: rows.length,
  };
}

async function setupSession(sessionId: string, messageCount: number, msgs: Message[] = []) {
  vi.mocked(api.getSession).mockResolvedValue(makeSession(sessionId, messageCount));
  vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse(msgs));
  await messages.loadSession(sessionId);
}

describe("MessagesStore", () => {
  beforeEach(() => {
    messages.clear();
    readProgress.reset();
    vi.clearAllMocks();
    runtimeMocks.signals.length = 0;
  });

  it("reports a failed metadata fetch to the sessions store", async () => {
    const err = new Error("session not found");
    vi.mocked(api.getSession).mockRejectedValue(err);
    vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse([]));

    await messages.loadSession("s1");

    expect(sessionsStore.markActiveSessionMissing).toHaveBeenCalledWith("s1", err);
  });

  it("does not report aborted metadata fetches", async () => {
    vi.mocked(api.getSession).mockRejectedValue(generatedCancelError());

    await messages.loadSession("s1");

    expect(sessionsStore.markActiveSessionMissing).not.toHaveBeenCalled();
  });

  it("does not report a stale metadata 404 after a newer load for the same session", async () => {
    const staleProbe = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValueOnce(staleProbe.promise);
    const staleLoad = messages.loadSession("s1");
    await Promise.resolve();
    messages.cancelInFlight();

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse([makeMessage(0)]));
    await messages.loadSession("s1");
    expect(messages.messages).toHaveLength(1);

    staleProbe.reject(new Error("session not found"));
    await staleLoad;

    expect(sessionsStore.markActiveSessionMissing).not.toHaveBeenCalled();
  });

  it("does not report a stale message-load failure after a newer load for the same session", async () => {
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    const stalePage = createDeferred<MessagesResponse>();
    vi.mocked(api.getMessages).mockReturnValueOnce(
      stalePage.promise as ReturnType<typeof api.getMessages>,
    );
    const staleLoad = messages.loadSession("s1");
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(1);
    });
    messages.cancelInFlight();

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse([makeMessage(0)]));
    await messages.loadSession("s1");
    expect(messages.messages).toHaveLength(1);

    stalePage.reject(new Error("session not found"));
    await staleLoad;

    expect(sessionsStore.markActiveSessionMissing).not.toHaveBeenCalled();
  });

  it("aborts in-flight reads without clearing cached messages", async () => {
    await setupSession("s1", 1, [makeMessage(0)]);
    const cached = messages.messages;
    const pending = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValueOnce(pending.promise);

    void messages.reload();
    await Promise.resolve();
    const signal = vi.mocked(api.getMessages).mock.lastCall?.[2]?.signal;
    messages.cancelInFlight();

    expect(signal?.aborted).toBe(true);
    expect(messages.messages).toBe(cached);

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([makeMessage(0)]));
    const resumed = messages.loadSession("s1");
    expect(messages.messages).toBe(cached);
    await resumed;
    expect(messages.messages).toHaveLength(1);
  });

  it("should clear reload state when loading a new session", async () => {
    await setupSession("s1", 10);
    expect(messages.sessionId).toBe("s1");

    // Trigger a reload that hangs
    const { promise: pendingReload, resolve: resolveReload } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(pendingReload);

    const p1 = messages.reload();

    // Switch to session s2
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", 5));
    await messages.loadSession("s2");

    expect(messages.sessionId).toBe("s2");

    // A new reload should create a fresh promise, not reuse p1
    const { promise: s2Reload, resolve: resolveS2 } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s2Reload);
    const p2 = messages.reload();
    expect(p2).not.toBe(p1);

    // Resolve dangling promises to clean up
    resolveReload(makeSession("s1", 10));
    resolveS2(makeSession("s2", 5));
    await Promise.all([p1, p2]);
  });

  it("should retain mainModel during reload", async () => {
    const msgs = Array.from({ length: 5 }, (_, i) => ({
      ...makeMessage(i),
      model: "claude-3-opus",
      reasoning_effort: i < 3 ? "high" : "medium",
    }));
    await setupSession("s1", 5, msgs);

    expect(messages.mainModel).toBe("claude-3-opus");
    expect(messages.mainModelInfo).toEqual({
      model: "claude-3-opus",
      reasoningEffort: "high",
    });

    // A smaller transcript triggers a full reload and its stable model state.
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 2));
    const { promise: hang, resolve: resolveHang } = createDeferred<MessagesResponse>();
    vi.mocked(api.getMessages).mockReturnValueOnce(hang);

    const p = messages.reload();
    await vi.waitFor(() => expect(messages.loading).toBe(true));

    expect(messages.mainModel).toBe("claude-3-opus");
    expect(messages.mainModelInfo).toEqual({
      model: "claude-3-opus",
      reasoningEffort: "high",
    });

    resolveHang(makeMessagesResponse(msgs.slice(0, 2)));
    await p;

    expect(messages.mainModel).toBe("claude-3-opus");
  });

  it("gates resume models through an in-flight, failed, and recovered reload", async () => {
    const modelMessage = {
      ...makeMessage(1),
      model: "claude sonnet",
    };
    await setupSession("s1", 2, [makeMessage(0), modelMessage]);
    expect(messages.resumeModelFor("s1")).toBe("claude sonnet");

    const pendingSession = createDeferred<Session>();
    const pendingMessages = createDeferred<MessagesResponse>();
    vi.mocked(api.getSession).mockReturnValueOnce(pendingSession.promise);
    vi.mocked(api.getMessages).mockReturnValueOnce(pendingMessages.promise);
    const inFlight = messages.reload();
    expect(messages.resumeModelFor("s1")).toBe("");

    pendingSession.resolve(makeSession("s1", 2));
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(2);
    });
    pendingMessages.resolve(makeMessagesResponse([makeMessage(0), modelMessage]));
    await inFlight;
    expect(messages.resumeModelFor("s1")).toBe("claude sonnet");

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockRejectedValueOnce(new Error("reload failed"));
    await messages.reload();
    expect(messages.resumeModelFor("s1")).toBe("");

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), modelMessage]),
    );
    await messages.reload();
    expect(messages.resumeModelFor("s1")).toBe("claude sonnet");
  });

  it("keeps partial history ineligible through page failure and pagination recovery", async () => {
    const modelMessage = {
      ...makeMessage(999),
      model: "claude sonnet",
    };
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3_001));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(
        Array.from({ length: 100 }, (_, i) => ({
          ...makeMessage(999 - i),
          model: i === 0 ? modelMessage.model : "",
        })),
      ),
    );
    await messages.loadSession("s1");
    expect(messages.resumeModelFor("s1")).toBe("");

    vi.mocked(api.getMessages).mockRejectedValueOnce(new Error("older page failed"));
    await messages.loadOlder();
    expect(messages.resumeModelFor("s1")).toBe("");

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(
        Array.from({ length: 900 }, (_, i) => ({
          ...makeMessage(899 - i),
          model: i === 0 ? modelMessage.model : "",
        })),
      ),
    );
    await messages.loadOlder();
    expect(messages.resumeModelFor("s1")).toBe("claude sonnet");
  });

  it("coalesces reloads and resets eligibility on session replacement and clear", async () => {
    const s1Message = { ...makeMessage(1), model: "claude sonnet" };
    await setupSession("s1", 2, [makeMessage(0), s1Message]);

    const pendingSession = createDeferred<Session>();
    const pendingMessages = createDeferred<MessagesResponse>();
    vi.mocked(api.getSession).mockReturnValueOnce(pendingSession.promise);
    vi.mocked(api.getMessages).mockReturnValueOnce(pendingMessages.promise);
    const first = messages.reload();
    expect(messages.reload()).toBe(first);
    expect(messages.resumeModelFor("s1")).toBe("");

    pendingSession.resolve(makeSession("s1", 2));
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(2);
    });
    pendingMessages.resolve(makeMessagesResponse([makeMessage(0), s1Message]));
    await first;
    expect(messages.resumeModelFor("s1")).toBe("claude sonnet");

    const replacementSession = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValueOnce(replacementSession.promise);
    const replacementReload = messages.reload();
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s2", 1));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([
        {
          ...makeMessage(0),
          role: "assistant",
          model: "o3-mini",
        },
      ]),
    );
    await messages.loadSession("s2");
    expect(messages.resumeModelFor("s1")).toBe("");
    expect(messages.resumeModelFor("s2")).toBe("o3-mini");
    replacementSession.resolve(makeSession("s1", 2));
    await replacementReload;

    messages.clear();
    expect(messages.resumeModelFor("s2")).toBe("");
  });

  it("ignores a deferred old-session page after the replacement session loads", async () => {
    const s1Page = createDeferred<MessagesResponse>();
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockReturnValueOnce(s1Page.promise);
    const s1Load = messages.loadSession("s1");
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(1);
    });

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s2", 1));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([
        {
          ...makeMessage(0),
          role: "assistant",
          model: "o3-mini",
        },
      ]),
    );
    await messages.loadSession("s2");
    expect(messages.resumeModelFor("s2")).toBe("o3-mini");

    s1Page.resolve(
      makeMessagesResponse([
        {
          ...makeMessage(0),
          role: "assistant",
          model: "claude sonnet",
        },
      ]),
    );
    await s1Load;

    expect(messages.sessionId).toBe("s2");
    expect(messages.mainModel).toBe("o3-mini");
    expect(messages.resumeModelFor("s2")).not.toBe("claude sonnet");
    expect(messages.resumeModelFor("s2")).toBe("o3-mini");
  });

  it("publishes a new transcript token only after refreshed messages arrive", async () => {
    vi.mocked(api.getSession).mockResolvedValue({
      ...makeSession("s1", 2),
      transcript_revision: "old",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );
    await messages.loadSession("s1");
    expect(messages.activeSessionToken).toBe("old");

    vi.mocked(api.getSession).mockResolvedValueOnce({
      ...makeSession("s1", 2),
      transcript_revision: "new",
    });
    const refreshed = createDeferred<MessagesResponse>();
    vi.mocked(api.getMessages).mockReturnValueOnce(refreshed.promise);

    const reload = messages.reload();
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(2);
    });
    expect(messages.activeSessionToken).toBe("old");

    refreshed.resolve(makeMessagesResponse([makeMessage(0), makeMessage(1)]));
    await reload;
    expect(messages.activeSessionToken).toBe("new");
  });

  it("publishes the earliest changed ordinal with a revised token", async () => {
    const original = [makeMessage(0), makeMessage(1), makeMessage(2)];
    vi.mocked(api.getSession).mockResolvedValue({
      ...makeSession("s1", original.length),
      transcript_revision: "old",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse(original));
    await messages.loadSession("s1");

    vi.mocked(api.getSession).mockResolvedValueOnce({
      ...makeSession("s1", original.length),
      transcript_revision: "new",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([
        { ...makeMessage(0), content: "edited earlier message" },
        makeMessage(1),
        makeMessage(2),
      ]),
    );

    await messages.reload();

    expect(messages.activeSessionToken).toBe("new");
    expect(messages.activeSessionUnreadOrdinal).toBe(0);
  });

  it("ignores replacement row IDs when locating changed transcript content", async () => {
    const original = [makeMessage(0), makeMessage(1)];
    vi.mocked(api.getSession).mockResolvedValue({
      ...makeSession("s1", original.length),
      transcript_revision: "old",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse(original));
    await messages.loadSession("s1");

    vi.mocked(api.getSession).mockResolvedValueOnce({
      ...makeSession("s1", original.length),
      transcript_revision: "new",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(
        original.map((message) => ({
          ...message,
          id: message.id + 100,
        })),
      ),
    );

    await messages.reload();

    expect(messages.activeSessionToken).toBe("new");
    expect(messages.activeSessionUnreadOrdinal).toBeNull();
  });

  it("keeps a revised token pending until progressively loaded history is visible", async () => {
    const count = 4_000;
    vi.mocked(api.getSession).mockResolvedValue({
      ...makeSession("s1", count),
      transcript_revision: "old",
    });
    const tail = Array.from({ length: 1_000 }, (_, i) => makeMessage(count - 1 - i));
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse(tail));
    await messages.loadSession("s1");
    expect(messages.hasOlder).toBe(true);
    expect(messages.activeSessionToken).toBe("old");

    vi.mocked(api.getSession).mockResolvedValueOnce({
      ...makeSession("s1", count),
      transcript_revision: "new",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([...tail].reverse()));
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([]));
    await messages.reload();
    expect(messages.activeSessionToken).toBe("old");

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(1), makeMessage(0)]),
    );
    await messages.loadOlder();
    expect(messages.hasOlder).toBe(false);
    expect(messages.activeSessionToken).toBe("new");
    expect(messages.activeSessionUnreadOrdinal).toBe(0);
  });

  it("publishes an already-current token for progressively loaded history", async () => {
    const count = 4_000;
    readProgress.baseline("s1", "current", count - 1);
    vi.mocked(api.getSession).mockResolvedValue({
      ...makeSession("s1", count),
      transcript_revision: "current",
    });
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(Array.from({ length: 1_000 }, (_, i) => makeMessage(count - 1 - i))),
    );

    await messages.loadSession("s1");

    expect(messages.hasOlder).toBe(true);
    expect(messages.activeSessionToken).toBe("current");
  });

  it("should not carry over mainModel to a different session", async () => {
    const s1Msgs = Array.from({ length: 3 }, (_, i) => ({
      ...makeMessage(i),
      model: "claude-3-opus",
    }));
    await setupSession("s1", 3, s1Msgs);
    expect(messages.mainModel).toBe("claude-3-opus");

    // Switch to s2 with a different model.
    const s2Msgs = Array.from({ length: 3 }, (_, i) => ({
      ...makeMessage(i),
      model: "claude-3-sonnet",
    }));
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", 3));
    vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse(s2Msgs));
    await messages.loadSession("s2");

    // Must show s2's model, not s1's.
    expect(messages.mainModel).toBe("claude-3-sonnet");
  });

  it("should not reuse reload promise from different session", async () => {
    await setupSession("s1", 10);

    // Start reload for s1
    const { promise: s1Promise, resolve: resolveS1 } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s1Promise);

    const p1 = messages.reload();

    // Switch to s2
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", 5));
    await messages.loadSession("s2");

    // Start reload for s2 — must be a new promise
    const { promise: s2Promise, resolve: resolveS2 } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s2Promise);

    const p2 = messages.reload();

    expect(p2).not.toBe(p1);

    resolveS1(makeSession("s1", 10));
    resolveS2(makeSession("s2", 5));
    await Promise.all([p1, p2]);
  });

  it("should coalesce reloads for the same session", async () => {
    await setupSession("s1", 10);

    // Start reload
    const { promise: s1Promise, resolve: resolveS1 } = createDeferred<Session>();
    vi.mocked(api.getSession)
      .mockReturnValueOnce(s1Promise)
      .mockResolvedValue(makeSession("s1", 10));

    const p1 = messages.reload();
    const p2 = messages.reload();

    // Coalesced: same promise returned
    expect(p1).toBe(p2);

    resolveS1(makeSession("s1", 10));
    await p1;
  });

  it("should no-op ensureOrdinalLoaded when full session is already loaded", async () => {
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 20));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(Array.from({ length: 20 }, (_, i) => makeMessage(i))),
    );

    await messages.loadSession("s1");

    expect(messages.messages.length).toBe(20);
    expect(messages.messages[0]).toBeDefined();
    expect(messages.messages[0]!.ordinal).toBe(0);
    expect(messages.hasOlder).toBe(false);

    await messages.ensureOrdinalLoaded(5);

    expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(1);
    expect(messages.messages.length).toBe(20);
    expect(messages.messages[0]).toBeDefined();
    expect(messages.messages[0]!.ordinal).toBe(0);
  });

  it("should not clear pending reload of a new session when old session reload finishes", async () => {
    // 1. Setup Session A
    await setupSession("s1", 10);

    // 2. Start Reload for Session A (P1) — hangs
    const { promise: p1Promise, resolve: resolveP1 } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(p1Promise);

    messages.reload();

    // 3. Switch to Session B
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", 5));
    await messages.loadSession("s2");

    // 4. Start Reload for Session B (P2) — hangs
    const { promise: p2Promise, resolve: resolveP2 } = createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(p2Promise);

    const p2 = messages.reload();

    // 5. Coalesced reload for Session B
    const p3 = messages.reload();
    expect(p3).toBe(p2); // Should reuse P2

    // 6. Resolve P1 (Session A).
    // This should NOT interfere with Session B's pending reload.
    const callsBeforeP1 = vi.mocked(api.getSession).mock.calls.length;
    resolveP1(makeSession("s1", 10));
    await new Promise((resolve) => setTimeout(resolve, 0));

    // P1 completing must not trigger an auto-reload for
    // Session B — getSession call count should be unchanged
    expect(vi.mocked(api.getSession).mock.calls.length).toBe(callsBeforeP1);

    // 7. Resolve P2 (Session B).
    // The pending reload should trigger automatically and
    // update state with the new count (6).
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", 6));
    vi.mocked(api.getMessages).mockResolvedValue(makeMessagesResponse([]));
    const callsBeforeP2 = vi.mocked(api.getSession).mock.calls.length;
    resolveP2(makeSession("s2", 5));

    // Wait for the automatic pending reload to fire and
    // call getSession again
    await vi.waitFor(() => {
      expect(vi.mocked(api.getSession).mock.calls.length).toBeGreaterThan(callsBeforeP2);
    });

    // The auto-reload fetched session with count=6,
    // confirming it actually ran and updated state
    expect(messages.messageCount).toBe(6);
  });

  it("should fallback to full reload if incremental fetch is out of sync", async () => {
    // 1. Initial State: Session 's1' with 2 messages
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession("s1");
    expect(messages.messageCount).toBe(2);

    // 2. Prepare for Reload
    // New state on server: count=4.
    // Incremental fetch returns only [2], missing [3].
    // This mismatch should trigger full reload.

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 4));

    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([makeMessage(2)]));

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(1), makeMessage(0), makeMessage(2), makeMessage(3)]),
    );

    await messages.reload();

    expect(messages.messageCount).toBe(4);
    expect(messages.messages.length).toBe(4);
    expect(messages.messages[3]!.ordinal).toBe(3);

    expect(vi.mocked(api.getMessages)).toHaveBeenLastCalledWith(
      "s1",
      expect.objectContaining({
        from: 0,
        limit: 1000,
        direction: "asc",
      }),
      expect.objectContaining({
        signal: expect.any(AbortSignal),
      }),
    );
  });

  it("should refresh the loaded window when reload count is unchanged", async () => {
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1), makeMessage(2)]),
    );

    await messages.loadSession("s1");
    expect(messages.messages[0]!.content).toBe("msg 0");

    const updated = {
      ...makeMessage(0),
      content: "msg 0 rewritten content",
      content_length: "msg 0 rewritten content".length,
    };
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 3));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([updated, makeMessage(1), makeMessage(2)]),
    );

    await messages.reload();

    expect(messages.messageCount).toBe(3);
    expect(messages.messages).toHaveLength(3);
    expect(messages.messages[0]!.content).toBe("msg 0 rewritten content");
    expect(vi.mocked(api.getMessages)).toHaveBeenLastCalledWith(
      "s1",
      expect.objectContaining({
        from: 0,
        limit: 1000,
        direction: "asc",
      }),
      expect.objectContaining({
        signal: expect.any(AbortSignal),
      }),
    );
  });

  it("should clear parser caches for same-length rewritten messages on same-count reload", async () => {
    const original = {
      ...makeMessage(0),
      content: "alpha1",
      content_length: 6,
    };
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([original]));

    await messages.loadSession("s1");
    expect(
      parseContent(original.content, original.has_tool_use, original.id, original.content_length),
    ).toEqual([{ type: "text", content: "alpha1" }]);

    const rewritten = {
      ...original,
      content: "bravo2",
    };
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 1));
    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([rewritten]));

    await messages.reload();

    expect(messages.messages[0]!.content).toBe("bravo2");
    expect(
      parseContent(
        messages.messages[0]!.content,
        messages.messages[0]!.has_tool_use,
        messages.messages[0]!.id,
        messages.messages[0]!.content_length,
      ),
    ).toEqual([{ type: "text", content: "bravo2" }]);
  });

  it("should refresh the loaded tail when appending new messages", async () => {
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession("s1");
    expect(messages.messages[1]!.content).toBe("msg 1");
    expect(
      parseContent(
        messages.messages[1]!.content,
        messages.messages[1]!.has_tool_use,
        messages.messages[1]!.id,
        messages.messages[1]!.content_length,
      ),
    ).toEqual([{ type: "text", content: "msg 1" }]);

    const updatedTail = {
      ...makeMessage(1),
      content: "tail!!",
      content_length: 6,
    };
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 3));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([updatedTail, makeMessage(2)]),
    );

    await messages.reload();

    expect(messages.messageCount).toBe(3);
    expect(messages.messages.map((m) => m.ordinal)).toEqual([0, 1, 2]);
    expect(messages.messages[1]!.content).toBe("tail!!");
    expect(
      parseContent(
        messages.messages[1]!.content,
        messages.messages[1]!.has_tool_use,
        messages.messages[1]!.id,
        messages.messages[1]!.content_length,
      ),
    ).toEqual([{ type: "text", content: "tail!!" }]);
    expect(vi.mocked(api.getMessages)).toHaveBeenLastCalledWith(
      "s1",
      expect.objectContaining({
        from: 0,
        limit: 1000,
        direction: "asc",
      }),
      expect.objectContaining({
        signal: expect.any(AbortSignal),
      }),
    );
  });

  it("should refresh earlier loaded messages when appending new messages", async () => {
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession("s1");
    expect(messages.messages[0]!.content).toBe("msg 0");

    const updatedEarlier = {
      ...makeMessage(0),
      content: "msg 0 rewritten",
      content_length: "msg 0 rewritten".length,
    };
    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 3));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([updatedEarlier, makeMessage(1), makeMessage(2)]),
    );

    await messages.reload();

    expect(messages.messageCount).toBe(3);
    expect(messages.messages.map((m) => m.ordinal)).toEqual([0, 1, 2]);
    expect(messages.messages[0]!.content).toBe("msg 0 rewritten");
    expect(vi.mocked(api.getMessages)).toHaveBeenLastCalledWith(
      "s1",
      expect.objectContaining({
        from: 0,
        limit: 1000,
        direction: "asc",
      }),
      expect.objectContaining({
        signal: expect.any(AbortSignal),
      }),
    );
  });

  it("should not update messageCount prematurely if incremental fetch fails and triggers full reload", async () => {
    // 1. Initial State: Session 's1' with 2 messages
    vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 2));
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession("s1");
    expect(messages.messageCount).toBe(2);

    vi.mocked(api.getSession).mockResolvedValueOnce(makeSession("s1", 4));

    vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse([makeMessage(2)]));

    // Full reload — delayed via deferred
    const { promise: fullReload, resolve: resolveFullReload } = createDeferred<MessagesResponse>();
    vi.mocked(api.getMessages).mockReturnValueOnce(
      fullReload as ReturnType<typeof api.getMessages>,
    );

    const reloadPromise = messages.reload();

    // Wait for the full reload call to be initiated
    await vi.waitFor(() => {
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(3);
    });

    // messageCount should still be 2 until full reload
    // completes
    expect(messages.messageCount).toBe(2);

    resolveFullReload(
      makeMessagesResponse([makeMessage(0), makeMessage(1), makeMessage(2), makeMessage(3)]),
    );

    await reloadPromise;

    expect(messages.messageCount).toBe(4);
  });

  describe("transcript revision", () => {
    function page(ordinals: number[], revision: string, content = "msg"): MessagesResponse {
      return {
        messages: ordinals.map((ordinal) => ({
          ...makeMessage(ordinal),
          content: `${content} ${ordinal}`,
        })),
        count: ordinals.length,
        transcript_revision: revision,
      };
    }
    const range = (from: number, to: number) =>
      Array.from({ length: to - from }, (_, i) => from + i);

    it("records the revision of the pages it accepted and binds later pages to it", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1500));
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page(range(0, 1000), "r1"))
        .mockResolvedValueOnce(page(range(1000, 1500), "r1"));

      await messages.loadSession("s1");

      expect(messages.messages).toHaveLength(1500);
      expect(messages.loadedRevision).toBe("r1");
      expect(vi.mocked(api.getMessages).mock.calls[0]![1]).not.toHaveProperty("expected_revision");
      expect(vi.mocked(api.getMessages).mock.calls[1]![1]).toMatchObject({
        from: 1000,
        expected_revision: "r1",
      });
    });

    it("starts over when a sync lands between pages, so every row shares one revision", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1500));
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page(range(0, 1000), "r1", "old"))
        .mockRejectedValueOnce(new ApiError(409, "transcript revision does not match"))
        .mockResolvedValueOnce(page(range(0, 1000), "r2", "new"))
        .mockResolvedValueOnce(page(range(1000, 1500), "r2", "new"));

      await messages.loadSession("s1");

      expect(messages.loadedRevision).toBe("r2");
      expect(messages.messages).toHaveLength(1500);
      expect(messages.messages.every((message) => message.content.startsWith("new"))).toBe(true);
      expect(vi.mocked(api.getMessages).mock.calls[3]![1]).toMatchObject({
        from: 1000,
        expected_revision: "r2",
      });
    });

    it("drops parsed content from an attempt a sync interrupted", async () => {
      const first = page(range(0, 1000), "r1");
      first.messages[0] = { ...first.messages[0]!, content: "alpha1", content_length: 6 };
      const rewritten = page(range(0, 1000), "r2");
      rewritten.messages[0] = { ...rewritten.messages[0]!, content: "bravo2", content_length: 6 };
      const gate = createDeferred<MessagesResponse>();
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1500));
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(first)
        .mockReturnValueOnce(gate.promise as ReturnType<typeof api.getMessages>)
        .mockImplementationOnce(async () => {
          // The old row is parsed again while the restarted page is in flight.
          parseContent(shown.content, shown.has_tool_use, shown.id, shown.content_length);
          return rewritten;
        })
        .mockResolvedValueOnce(page(range(1000, 1500), "r2"));

      let shown = first.messages[0]!;
      const load = messages.loadSession("s1");
      await vi.waitFor(() => expect(messages.messages).toHaveLength(1000));
      shown = messages.messages[0]!;
      expect(
        parseContent(shown.content, shown.has_tool_use, shown.id, shown.content_length),
      ).toEqual([{ type: "text", content: "alpha1" }]);
      gate.reject(new ApiError(409, "transcript revision does not match"));
      await load;

      const row = messages.messages[0]!;
      expect(messages.loadedRevision).toBe("r2");
      expect(parseContent(row.content, row.has_tool_use, row.id, row.content_length)).toEqual([
        { type: "text", content: "bravo2" },
      ]);
    });

    it("ignores a continuation page that a reload overtook", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1500));
      const second = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page(range(0, 1000), "r1", "old"))
        .mockReturnValueOnce(second.promise as ReturnType<typeof api.getMessages>);
      const load = messages.loadSession("s1");
      await vi.waitFor(() => expect(messages.messages).toHaveLength(1000));

      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page(range(0, 1000), "r2", "new"))
        .mockResolvedValueOnce(page(range(1000, 1500), "r2", "new"));
      await messages.reload();
      expect(messages.loadedRevision).toBe("r2");

      second.resolve(page(range(1000, 1500), "r1", "old"));
      await load;
      expect(messages.loadedRevision).toBe("r2");
      expect(messages.messages.every((message) => message.content.startsWith("new"))).toBe(true);
    });

    it("ignores a delayed first page that a reload overtook", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
      const first = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        first.promise as ReturnType<typeof api.getMessages>,
      );
      const load = messages.loadSession("s1");
      await vi.waitFor(() => expect(api.getMessages).toHaveBeenCalledTimes(1));

      // A sync-triggered reload lands the newer transcript before the initial page returns.
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r2", "new"));
      await messages.reload();
      expect(messages.loadedRevision).toBe("r2");

      first.resolve(page([0, 1, 2], "r1", "old"));
      await load;
      expect(messages.loadedRevision).toBe("r2");
      expect(messages.messages.every((message) => message.content.startsWith("new"))).toBe(true);
    });

    it("replaces the window rather than merging rows from a new revision", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r1", "old"));
      await messages.loadSession("s1");

      // A resync rewrote the transcript in place; the new revision has no row 2.
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1], "r2", "new"));
      await messages.reload();

      expect(messages.loadedRevision).toBe("r2");
      expect(messages.messages.map((message) => message.content)).toEqual(["new 0", "new 1"]);
    });

    it("keeps the loaded revision while a refresh is delayed and after it fails", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r1", "old"));
      await messages.loadSession("s1");
      vi.spyOn(console, "warn").mockImplementation(() => {});

      const delayed = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        delayed.promise as ReturnType<typeof api.getMessages>,
      );
      const reload = messages.reload();
      await Promise.resolve();
      expect(messages.loadedRevision).toBe("r1");

      delayed.reject(new Error("offline"));
      await reload;
      expect(messages.loadedRevision).toBe("r1");
      expect(messages.messages.map((message) => message.content)).toEqual([
        "old 0",
        "old 1",
        "old 2",
      ]);
    });

    it("drops an older page from a moved transcript and reloads instead", async () => {
      const count = 5_000;
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", count));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page(range(4000, 5000).reverse(), "r1"));
      await messages.loadSession("s1");
      expect(messages.hasOlder).toBe(true);

      vi.mocked(api.getMessages)
        .mockRejectedValueOnce(new ApiError(409, "transcript revision does not match"))
        .mockResolvedValueOnce(page(range(4000, 5000), "r2", "new"))
        .mockResolvedValueOnce(page([], "r2"));
      await messages.loadOlder();
      await vi.waitFor(() => expect(messages.loadedRevision).toBe("r2"));

      expect(vi.mocked(api.getMessages).mock.calls[1]![1]).toMatchObject({
        from: 3999,
        expected_revision: "r1",
      });
      expect(messages.messages[0]!.ordinal).toBe(4000);
      expect(messages.messages.every((message) => message.content.startsWith("new"))).toBe(true);
    });

    it("still reaches a jump target after the transcript moves under it", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 5_000));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page(range(4000, 5000).reverse(), "r1"));
      await messages.loadSession("s1");

      vi.mocked(api.getMessages)
        .mockRejectedValueOnce(new ApiError(409, "transcript revision does not match"))
        .mockResolvedValueOnce(page(range(4000, 5000), "r2"))
        .mockResolvedValueOnce(page([], "r2"))
        .mockResolvedValueOnce(page(range(3000, 4000).reverse(), "r2"));
      await messages.ensureOrdinalLoaded(3500);

      expect(messages.loadedRevision).toBe("r2");
      expect(messages.messages[0]!.ordinal).toBe(3000);
      expect(vi.mocked(api.getMessages).mock.calls.at(-1)![1]).toMatchObject({
        from: 3999,
        expected_revision: "r2",
      });
    });

    it("stops reloading after a few passes that a busy transcript keeps refusing", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 1500));
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page(range(0, 1000), "r1"))
        .mockResolvedValueOnce(page(range(1000, 1500), "r1"));
      await messages.loadSession("s1");
      vi.spyOn(console, "warn").mockImplementation(() => {});

      // Every refresh sees its first page, then a sync lands before the second.
      let call = 0;
      vi.mocked(api.getMessages).mockImplementation(async () => {
        call++;
        if (call % 2 === 1) return page(range(0, 1000), `r${call + 1}`);
        throw new ApiError(409, "transcript revision does not match");
      });
      const sessionReads = vi.mocked(api.getSession).mock.calls.length;
      await messages.reload();

      expect(vi.mocked(api.getSession).mock.calls.length - sessionReads).toBe(3);
      expect(messages.loadedRevision).toBe("r1");
      expect(messages.messages).toHaveLength(1500);
    });

    it("clears rows when an unchanged count hides a transcript emptied under the window", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r1"));
      await messages.loadSession("s1");

      // The session read still reports three rows, but the transcript empties before the page read.
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(page([], "r2"))
        .mockResolvedValueOnce(page([], "r2"));
      await messages.reload();

      expect(messages.messages).toEqual([]);
      expect(messages.loadedRevision).toBe("r2");

      // The rows come back while the count still matches the stale one.
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r3"));
      await messages.reload();
      expect(messages.messages).toHaveLength(3);
      expect(messages.loadedRevision).toBe("r3");
    });

    it("clears rows when a full reload finds the session emptied", async () => {
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 3));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([0, 1, 2], "r1"));
      await messages.loadSession("s1");

      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", 0));
      vi.mocked(api.getMessages).mockResolvedValueOnce(page([], "r2"));
      await messages.reload();

      expect(messages.messages).toEqual([]);
      expect(messages.loadedRevision).toBe("r2");
    });
  });

  describe("loadOlder abort handling", () => {
    async function setupProgressiveSession() {
      // Progressive loading triggers when count > 20_000.
      // The first desc page returns ordinals 900..999 (reversed
      // to 900..999 ascending). hasOlder is true because
      // oldest ordinal (900) > 0.
      const count = 25_000;
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s1", count));
      const descPage = Array.from({ length: 100 }, (_, i) => makeMessage(999 - i));
      vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse(descPage));

      await messages.loadSession("s1");
      expect(messages.hasOlder).toBe(true);
      expect(messages.messages[0]!.ordinal).toBe(900);
    }

    it("should not surface abort error as unhandled rejection from loadOlder", async () => {
      await setupProgressiveSession();

      // Make getMessages hang until aborted
      const { promise: hang, reject: rejectHang } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValue(hang as ReturnType<typeof api.getMessages>);

      const olderPromise = messages.loadOlder();
      expect(messages.loadingOlder).toBe(true);

      // Simulate session switch which aborts in-flight requests
      rejectHang(generatedCancelError());
      messages.clear();

      // Should resolve without throwing
      await expect(olderPromise).resolves.toBeUndefined();
    });

    it("should serialize concurrent loadOlder and ensureOrdinalLoaded", async () => {
      await setupProgressiveSession();

      // First loadOlder call — hangs
      const { promise: firstHang, resolve: resolveFirst } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        firstHang as ReturnType<typeof api.getMessages>,
      );

      const p1 = messages.loadOlder();

      // ensureOrdinalLoaded should wait for the in-flight
      // loadOlder before starting its own fetch. After
      // loadOlder resolves (800-899), ensureOrdinal needs
      // data further back (700-799).
      const ensureChunk = Array.from({ length: 100 }, (_, i) => makeMessage(799 - i));
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(makeMessagesResponse(ensureChunk))
        .mockResolvedValueOnce(makeMessagesResponse([]));

      const p2 = messages.ensureOrdinalLoaded(0);

      // p1 still pending — getMessages should only have been
      // called once so far (the loadOlder call)
      expect(vi.mocked(api.getMessages)).toHaveBeenCalledTimes(2); // 1 from loadSession + 1 from loadOlder

      // Resolve the first loadOlder
      const loadOlderChunk = Array.from({ length: 100 }, (_, i) => makeMessage(899 - i));
      resolveFirst(makeMessagesResponse(loadOlderChunk));

      await p1;
      await p2;

      // Both completed without errors; loadingOlder is reset
      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map((m) => m.ordinal);
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it("should not allow overlapping loadOlder calls", async () => {
      await setupProgressiveSession();
      const callsBefore = vi.mocked(api.getMessages).mock.calls.length;

      const { promise: hang, resolve: resolveHang } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(hang as ReturnType<typeof api.getMessages>);

      const p1 = messages.loadOlder();
      // Second call while first is in-flight should not start
      // another fetch
      const p2 = messages.loadOlder();

      // Only one additional getMessages call was made
      expect(vi.mocked(api.getMessages).mock.calls.length - callsBefore).toBe(1);

      const olderChunk = Array.from({ length: 100 }, (_, i) => makeMessage(899 - i));
      resolveHang(makeMessagesResponse(olderChunk));
      await Promise.all([p1, p2]);

      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map((m) => m.ordinal);
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it("should not let stale loadOlder promise clear a newer session loadOlderPromise", async () => {
      await setupProgressiveSession();

      // Start loadOlder for session A — hangs
      const { promise: s1Hang, resolve: resolveS1 } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(s1Hang as ReturnType<typeof api.getMessages>);

      const p1 = messages.loadOlder();

      // Switch to session B (progressive)
      const s2Count = 25_000;
      vi.mocked(api.getSession).mockResolvedValue(makeSession("s2", s2Count));
      const s2DescPage = Array.from({ length: 100 }, (_, i) => makeMessage(999 - i));
      vi.mocked(api.getMessages).mockResolvedValueOnce(makeMessagesResponse(s2DescPage));
      await messages.loadSession("s2");
      expect(messages.hasOlder).toBe(true);

      // Start loadOlder for session B — hangs
      const { promise: s2Hang, resolve: resolveS2 } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(s2Hang as ReturnType<typeof api.getMessages>);

      const p2 = messages.loadOlder();

      // Resolve the stale session A promise — this must NOT
      // clear loadOlderPromise, which now belongs to session B
      const s1Chunk = Array.from({ length: 100 }, (_, i) => makeMessage(899 - i));
      resolveS1(makeMessagesResponse(s1Chunk));
      await p1;

      // Session B's loadOlder should still be recognized as
      // in-flight: a third loadOlder call should return the
      // existing promise, not start a new one
      const callsBefore = vi.mocked(api.getMessages).mock.calls.length;
      const p3 = messages.loadOlder();
      expect(vi.mocked(api.getMessages).mock.calls.length).toBe(callsBefore);

      // Resolve session B's loadOlder
      const s2Chunk = Array.from({ length: 100 }, (_, i) => makeMessage(899 - i));
      resolveS2(makeMessagesResponse(s2Chunk));
      await Promise.all([p2, p3]);

      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map((m) => m.ordinal);
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it("should not surface abort error from ensureOrdinalLoaded on session switch", async () => {
      await setupProgressiveSession();

      const { promise: hang, reject: rejectHang } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValue(hang as ReturnType<typeof api.getMessages>);

      const p = messages.ensureOrdinalLoaded(0);

      rejectHang(generatedCancelError());
      messages.clear();

      await expect(p).resolves.toBeUndefined();
    });
  });
});
