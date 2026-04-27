import { describe, it, expect, vi, beforeEach } from 'vitest';
import { messages } from './messages.svelte.js';
import * as api from '../api/client.js';
import type {
  Message,
  MessagesResponse,
  Session,
} from '../api/types.js';

// Mock the API client
vi.mock('../api/client.js', () => ({
  getMessages: vi.fn(),
  getSession: vi.fn(),
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

function makeSession(
  id: string,
  messageCount: number,
): Session {
  return {
    id,
    project: 'project-alpha',
    machine: 'test-machine',
    agent: 'test-agent',
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
    session_id: 's1',
    ordinal,
    role: ordinal % 2 === 0 ? 'user' : 'assistant',
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

function makeMessagesResponse(
  rows: Message[],
): MessagesResponse {
  return {
    messages: rows,
    count: rows.length,
  };
}

async function setupSession(
  sessionId: string,
  messageCount: number,
  msgs: Message[] = [],
) {
  vi.mocked(api.getSession).mockResolvedValue(
    makeSession(sessionId, messageCount),
  );
  vi.mocked(api.getMessages).mockResolvedValue(
    makeMessagesResponse(msgs),
  );
  await messages.loadSession(sessionId);
}

describe('MessagesStore', () => {
  beforeEach(() => {
    messages.clear();
    vi.clearAllMocks();
  });

  it('should clear reload state when loading a new session', async () => {
    await setupSession('s1', 10);
    expect(messages.sessionId).toBe('s1');

    // Trigger a reload that hangs
    const { promise: pendingReload, resolve: resolveReload } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(pendingReload);

    const p1 = messages.reload();

    // Switch to session s2
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s2', 5),
    );
    await messages.loadSession('s2');

    expect(messages.sessionId).toBe('s2');

    // A new reload should create a fresh promise, not reuse p1
    const { promise: s2Reload, resolve: resolveS2 } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s2Reload);
    const p2 = messages.reload();
    expect(p2).not.toBe(p1);

    // Resolve dangling promises to clean up
    resolveReload(makeSession('s1', 10));
    resolveS2(makeSession('s2', 5));
    await Promise.all([p1, p2]);
  });

  it('should retain mainModel during reload', async () => {
    const msgs = Array.from({ length: 5 }, (_, i) => ({
      ...makeMessage(i),
      model: 'claude-3-opus',
    }));
    await setupSession('s1', 5, msgs);

    expect(messages.mainModel).toBe('claude-3-opus');

    // Start a reload that hangs — mainModel must stay stable.
    const { promise: hang, resolve: resolveHang } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(hang);

    const p = messages.reload();

    // While reload is in flight, mainModel should still be
    // computed from the existing messages, not blank.
    expect(messages.mainModel).toBe('claude-3-opus');

    resolveHang(makeSession('s1', 5));
    await p;

    expect(messages.mainModel).toBe('claude-3-opus');
  });

  it('should not carry over mainModel to a different session', async () => {
    const s1Msgs = Array.from({ length: 3 }, (_, i) => ({
      ...makeMessage(i),
      model: 'claude-3-opus',
    }));
    await setupSession('s1', 3, s1Msgs);
    expect(messages.mainModel).toBe('claude-3-opus');

    // Switch to s2 with a different model.
    const s2Msgs = Array.from({ length: 3 }, (_, i) => ({
      ...makeMessage(i),
      model: 'claude-3-sonnet',
    }));
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s2', 3),
    );
    vi.mocked(api.getMessages).mockResolvedValue(
      makeMessagesResponse(s2Msgs),
    );
    await messages.loadSession('s2');

    // Must show s2's model, not s1's.
    expect(messages.mainModel).toBe('claude-3-sonnet');
  });

  it('should not reuse reload promise from different session', async () => {
    await setupSession('s1', 10);

    // Start reload for s1
    const { promise: s1Promise, resolve: resolveS1 } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s1Promise);

    const p1 = messages.reload();

    // Switch to s2
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s2', 5),
    );
    await messages.loadSession('s2');

    // Start reload for s2 — must be a new promise
    const { promise: s2Promise, resolve: resolveS2 } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(s2Promise);

    const p2 = messages.reload();

    expect(p2).not.toBe(p1);

    resolveS1(makeSession('s1', 10));
    resolveS2(makeSession('s2', 5));
    await Promise.all([p1, p2]);
  });

  it('should coalesce reloads for the same session', async () => {
    await setupSession('s1', 10);

    // Start reload
    const { promise: s1Promise, resolve: resolveS1 } =
      createDeferred<Session>();
    vi.mocked(api.getSession)
      .mockReturnValueOnce(s1Promise)
      .mockResolvedValue(makeSession('s1', 10));

    const p1 = messages.reload();
    const p2 = messages.reload();

    // Coalesced: same promise returned
    expect(p1).toBe(p2);

    resolveS1(makeSession('s1', 10));
    await p1;
  });

  it('should no-op ensureOrdinalLoaded when full session is already loaded', async () => {
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s1', 20),
    );
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse(
        Array.from(
          { length: 20 },
          (_, i) => makeMessage(i),
        ),
      ),
    );

    await messages.loadSession('s1');

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

  it('should not clear pending reload of a new session when old session reload finishes', async () => {
    // 1. Setup Session A
    await setupSession('s1', 10);

    // 2. Start Reload for Session A (P1) — hangs
    const { promise: p1Promise, resolve: resolveP1 } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(p1Promise);

    const p1 = messages.reload();

    // 3. Switch to Session B
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s2', 5),
    );
    await messages.loadSession('s2');

    // 4. Start Reload for Session B (P2) — hangs
    const { promise: p2Promise, resolve: resolveP2 } =
      createDeferred<Session>();
    vi.mocked(api.getSession).mockReturnValue(p2Promise);

    const p2 = messages.reload();

    // 5. Coalesced reload for Session B
    const p3 = messages.reload();
    expect(p3).toBe(p2); // Should reuse P2

    // 6. Resolve P1 (Session A).
    // This should NOT interfere with Session B's pending reload.
    const callsBeforeP1 =
      vi.mocked(api.getSession).mock.calls.length;
    resolveP1(makeSession('s1', 10));
    await new Promise(resolve => setTimeout(resolve, 0));

    // P1 completing must not trigger an auto-reload for
    // Session B — getSession call count should be unchanged
    expect(
      vi.mocked(api.getSession).mock.calls.length,
    ).toBe(callsBeforeP1);

    // 7. Resolve P2 (Session B).
    // The pending reload should trigger automatically and
    // update state with the new count (6).
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s2', 6),
    );
    vi.mocked(api.getMessages).mockResolvedValue(
      makeMessagesResponse([]),
    );
    const callsBeforeP2 =
      vi.mocked(api.getSession).mock.calls.length;
    resolveP2(makeSession('s2', 5));

    // Wait for the automatic pending reload to fire and
    // call getSession again
    await vi.waitFor(() => {
      expect(
        vi.mocked(api.getSession).mock.calls.length,
      ).toBeGreaterThan(callsBeforeP2);
    });

    // The auto-reload fetched session with count=6,
    // confirming it actually ran and updated state
    expect(messages.messageCount).toBe(6);
  });

  it('should fallback to full reload if incremental fetch is out of sync', async () => {
    // 1. Initial State: Session 's1' with 2 messages
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s1', 2),
    );
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession('s1');
    expect(messages.messageCount).toBe(2);

    // 2. Prepare for Reload
    // New state on server: count=4.
    // Incremental fetch returns only [2], missing [3].
    // This mismatch should trigger full reload.

    vi.mocked(api.getSession).mockResolvedValueOnce(
      makeSession('s1', 4),
    );

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(2)]),
    );

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([
        makeMessage(1),
        makeMessage(0),
        makeMessage(2),
        makeMessage(3),
      ]),
    );

    await messages.reload();

    expect(messages.messageCount).toBe(4);
    expect(messages.messages.length).toBe(4);
    expect(messages.messages[3]!.ordinal).toBe(3);

    expect(vi.mocked(api.getMessages)).toHaveBeenLastCalledWith(
      's1',
      expect.objectContaining({
        from: 0,
        limit: 1000,
        direction: 'asc',
      }),
      expect.objectContaining({
        signal: expect.any(AbortSignal),
      }),
    );
  });

  it('should not update messageCount prematurely if incremental fetch fails and triggers full reload', async () => {
    // 1. Initial State: Session 's1' with 2 messages
    vi.mocked(api.getSession).mockResolvedValue(
      makeSession('s1', 2),
    );
    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(0), makeMessage(1)]),
    );

    await messages.loadSession('s1');
    expect(messages.messageCount).toBe(2);

    vi.mocked(api.getSession).mockResolvedValueOnce(
      makeSession('s1', 4),
    );

    vi.mocked(api.getMessages).mockResolvedValueOnce(
      makeMessagesResponse([makeMessage(2)]),
    );

    // Full reload — delayed via deferred
    const { promise: fullReload, resolve: resolveFullReload } =
      createDeferred<MessagesResponse>();
    vi.mocked(api.getMessages).mockReturnValueOnce(
      fullReload as ReturnType<typeof api.getMessages>,
    );

    const reloadPromise = messages.reload();

    // Wait for the full reload call to be initiated
    await vi.waitFor(() => {
      expect(
        vi.mocked(api.getMessages),
      ).toHaveBeenCalledTimes(3);
    });

    // messageCount should still be 2 until full reload
    // completes
    expect(messages.messageCount).toBe(2);

    resolveFullReload(
      makeMessagesResponse([
        makeMessage(0),
        makeMessage(1),
        makeMessage(2),
        makeMessage(3),
      ]),
    );

    await reloadPromise;

    expect(messages.messageCount).toBe(4);
  });

  describe('loadOlder abort handling', () => {
    async function setupProgressiveSession() {
      // Progressive loading triggers when count > 20_000.
      // The first desc page returns ordinals 900..999 (reversed
      // to 900..999 ascending). hasOlder is true because
      // oldest ordinal (900) > 0.
      const count = 25_000;
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession('s1', count),
      );
      const descPage = Array.from(
        { length: 100 },
        (_, i) => makeMessage(999 - i),
      );
      vi.mocked(api.getMessages).mockResolvedValueOnce(
        makeMessagesResponse(descPage),
      );

      await messages.loadSession('s1');
      expect(messages.hasOlder).toBe(true);
      expect(messages.messages[0]!.ordinal).toBe(900);
    }

    it('should not surface abort error as unhandled rejection from loadOlder', async () => {
      await setupProgressiveSession();

      // Make getMessages hang until aborted
      const { promise: hang, reject: rejectHang } =
        createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValue(
        hang as ReturnType<typeof api.getMessages>,
      );

      const olderPromise = messages.loadOlder();
      expect(messages.loadingOlder).toBe(true);

      // Simulate session switch which aborts in-flight requests
      rejectHang(
        new DOMException('The operation was aborted.', 'AbortError'),
      );
      messages.clear();

      // Should resolve without throwing
      await expect(olderPromise).resolves.toBeUndefined();
    });

    it('should serialize concurrent loadOlder and ensureOrdinalLoaded', async () => {
      await setupProgressiveSession();

      // First loadOlder call — hangs
      const {
        promise: firstHang,
        resolve: resolveFirst,
      } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        firstHang as ReturnType<typeof api.getMessages>,
      );

      const p1 = messages.loadOlder();

      // ensureOrdinalLoaded should wait for the in-flight
      // loadOlder before starting its own fetch. After
      // loadOlder resolves (800-899), ensureOrdinal needs
      // data further back (700-799).
      const ensureChunk = Array.from(
        { length: 100 },
        (_, i) => makeMessage(799 - i),
      );
      vi.mocked(api.getMessages)
        .mockResolvedValueOnce(
          makeMessagesResponse(ensureChunk),
        )
        .mockResolvedValueOnce(
          makeMessagesResponse([]),
        );

      const p2 = messages.ensureOrdinalLoaded(0);

      // p1 still pending — getMessages should only have been
      // called once so far (the loadOlder call)
      expect(
        vi.mocked(api.getMessages),
      ).toHaveBeenCalledTimes(2); // 1 from loadSession + 1 from loadOlder

      // Resolve the first loadOlder
      const loadOlderChunk = Array.from(
        { length: 100 },
        (_, i) => makeMessage(899 - i),
      );
      resolveFirst(makeMessagesResponse(loadOlderChunk));

      await p1;
      await p2;

      // Both completed without errors; loadingOlder is reset
      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map(
        (m) => m.ordinal,
      );
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it('should not allow overlapping loadOlder calls', async () => {
      await setupProgressiveSession();
      const callsBefore =
        vi.mocked(api.getMessages).mock.calls.length;

      const { promise: hang, resolve: resolveHang } =
        createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        hang as ReturnType<typeof api.getMessages>,
      );

      const p1 = messages.loadOlder();
      // Second call while first is in-flight should not start
      // another fetch
      const p2 = messages.loadOlder();

      // Only one additional getMessages call was made
      expect(
        vi.mocked(api.getMessages).mock.calls.length -
          callsBefore,
      ).toBe(1);

      const olderChunk = Array.from(
        { length: 100 },
        (_, i) => makeMessage(899 - i),
      );
      resolveHang(makeMessagesResponse(olderChunk));
      await Promise.all([p1, p2]);

      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map(
        (m) => m.ordinal,
      );
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it('should not let stale loadOlder promise clear a newer session loadOlderPromise', async () => {
      await setupProgressiveSession();

      // Start loadOlder for session A — hangs
      const {
        promise: s1Hang,
        resolve: resolveS1,
      } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        s1Hang as ReturnType<typeof api.getMessages>,
      );

      const p1 = messages.loadOlder();

      // Switch to session B (progressive)
      const s2Count = 25_000;
      vi.mocked(api.getSession).mockResolvedValue(
        makeSession('s2', s2Count),
      );
      const s2DescPage = Array.from(
        { length: 100 },
        (_, i) => makeMessage(999 - i),
      );
      vi.mocked(api.getMessages).mockResolvedValueOnce(
        makeMessagesResponse(s2DescPage),
      );
      await messages.loadSession('s2');
      expect(messages.hasOlder).toBe(true);

      // Start loadOlder for session B — hangs
      const {
        promise: s2Hang,
        resolve: resolveS2,
      } = createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValueOnce(
        s2Hang as ReturnType<typeof api.getMessages>,
      );

      const p2 = messages.loadOlder();

      // Resolve the stale session A promise — this must NOT
      // clear loadOlderPromise, which now belongs to session B
      const s1Chunk = Array.from(
        { length: 100 },
        (_, i) => makeMessage(899 - i),
      );
      resolveS1(makeMessagesResponse(s1Chunk));
      await p1;

      // Session B's loadOlder should still be recognized as
      // in-flight: a third loadOlder call should return the
      // existing promise, not start a new one
      const callsBefore =
        vi.mocked(api.getMessages).mock.calls.length;
      const p3 = messages.loadOlder();
      expect(
        vi.mocked(api.getMessages).mock.calls.length,
      ).toBe(callsBefore);

      // Resolve session B's loadOlder
      const s2Chunk = Array.from(
        { length: 100 },
        (_, i) => makeMessage(899 - i),
      );
      resolveS2(makeMessagesResponse(s2Chunk));
      await Promise.all([p2, p3]);

      expect(messages.loadingOlder).toBe(false);

      // Ordinals should be strictly ascending with no duplicates
      const ordinals = messages.messages.map(
        (m) => m.ordinal,
      );
      for (let i = 1; i < ordinals.length; i++) {
        expect(ordinals[i]).toBeGreaterThan(ordinals[i - 1]!);
      }
      expect(new Set(ordinals).size).toBe(ordinals.length);
    });

    it('should not surface abort error from ensureOrdinalLoaded on session switch', async () => {
      await setupProgressiveSession();

      const { promise: hang, reject: rejectHang } =
        createDeferred<MessagesResponse>();
      vi.mocked(api.getMessages).mockReturnValue(
        hang as ReturnType<typeof api.getMessages>,
      );

      const p = messages.ensureOrdinalLoaded(0);

      rejectHang(
        new DOMException('The operation was aborted.', 'AbortError'),
      );
      messages.clear();

      await expect(p).resolves.toBeUndefined();
    });
  });
});
