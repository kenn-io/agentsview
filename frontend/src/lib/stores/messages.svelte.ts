import { SessionsService } from "../api/generated/index";
import type { Message, MessagesResponse, Session } from "../api/types.js";
import { configureGeneratedClient, isAbortError, withAbort } from "../api/runtime.js";
import { clearContentCaches } from "../utils/content-parser.js";
import { computeMainModel } from "../utils/model.js";
import { buildReadProgressToken, readProgress } from "./read-progress.svelte.js";

const MESSAGE_PAGE_SIZE = 1000;
const FULL_SESSION_MESSAGE_THRESHOLD = 3_000;

interface FetchPageOptions {
  from: number;
  limit: number;
  direction: "asc" | "desc";
  signal: AbortSignal;
}

class MessagesStore {
  messages: Message[] = $state([]);
  loading: boolean = $state(false);
  sessionId: string | null = $state(null);
  messageCount: number = $state(0);
  activeSessionToken: string | null = $state(null);
  activeSessionUnreadOrdinal: number | null = $state(null);
  hasOlder: boolean = $state(false);
  loadingOlder: boolean = $state(false);
  historyComplete: boolean = $state(false);
  private reloading: boolean = $state(false);
  private _stableMainModel: string = $state("");
  mainModel: string = $derived(
    this.loading
      ? this._stableMainModel
      : this.messages.length > 0
        ? computeMainModel(this.messages)
        : "",
  );
  private abortController: AbortController | null = null;
  private cancelledSessionId: string | null = null;
  private reloadPromise: Promise<void> | null = null;
  private reloadSessionId: string | null = null;
  private pendingReload: boolean = false;
  private loadOlderPromise: Promise<void> | null = null;
  private pendingSessionToken: string | null = null;
  private hasPendingSessionToken: boolean = false;
  private pendingSessionUnreadOrdinal: number | null = null;

  resumeModelFor(sessionId: string): string {
    return this.sessionId === sessionId &&
      this.historyComplete &&
      !this.loading &&
      !this.loadingOlder &&
      !this.reloading
      ? this.mainModel
      : "";
  }

  async loadSession(id: string) {
    const resumesCancelledLoad =
      this.sessionId === id && this.cancelledSessionId === id;
    if (
      this.sessionId === id &&
      !resumesCancelledLoad &&
      (this.messages.length > 0 || this.loading)
    ) {
      return;
    }
    const readMarker = readProgress.get(id);
    if (!resumesCancelledLoad) {
      this.clear();
      this._stableMainModel = "";
      this.activeSessionToken = null;
      this.activeSessionUnreadOrdinal = null;
    }
    this.sessionId = id;
    this.cancelledSessionId = null;
    this.loading = true;

    const ac = new AbortController();
    this.abortController = ac;

    try {
      let countHint: number | null = null;
      let pendingToken: string | null = null;
      try {
        configureGeneratedClient();
        const sess = await withAbort(
          SessionsService.getApiV1SessionsId({ id }) as unknown as Promise<Session>,
          ac.signal,
        );
        countHint = sess.message_count ?? 0;
        pendingToken = buildReadProgressToken(sess);
      } catch (err) {
        if (isAbortError(err)) return;
        console.warn("Failed to fetch session metadata:", err);
      }

      if (countHint !== null && countHint > FULL_SESSION_MESSAGE_THRESHOLD) {
        await this.loadProgressively(id, ac.signal);
      } else {
        await this.loadAllMessages(id, ac.signal, countHint ?? undefined);
      }
      if (this.sessionId === id) {
        this.publishOrDeferSessionToken(
          pendingToken,
          null,
          readMarker !== null && readMarker.token !== pendingToken,
        );
      }
    } catch (err) {
      if (isAbortError(err)) return;
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Failed to load session messages:", err);
    } finally {
      if (this.sessionId === id) {
        this.loading = false;
        this._stableMainModel = this.messages.length > 0 ? computeMainModel(this.messages) : "";
      }
    }
  }

  reload(): Promise<void> {
    if (!this.sessionId) return Promise.resolve();

    if (this.reloadPromise && this.reloadSessionId === this.sessionId) {
      this.pendingReload = true;
      return this.reloadPromise;
    }

    const id = this.sessionId;
    this.reloadSessionId = id;
    this.reloading = true;

    const promise = this.reloadNow(id).finally(async () => {
      if (this.reloadPromise === promise) {
        this.reloadPromise = null;
        this.reloadSessionId = null;
      }
      const shouldReload = this.pendingReload && this.sessionId === id;
      if (shouldReload) {
        this.pendingReload = false;
        await this.reload();
      } else if (this.sessionId === id) {
        this.reloading = false;
      }
    });
    this.reloadPromise = promise;
    return promise;
  }

  clear() {
    this.cancelInFlight();
    this.messages = [];
    clearContentCaches();
    this.sessionId = null;
    this.cancelledSessionId = null;
    this.loading = false;
    this._stableMainModel = "";
    this.messageCount = 0;
    this.activeSessionToken = null;
    this.activeSessionUnreadOrdinal = null;
    this.hasOlder = false;
    this.loadingOlder = false;
    this.historyComplete = false;
    this.reloading = false;
    this.reloadPromise = null;
    this.reloadSessionId = null;
    this.pendingReload = false;
    this.loadOlderPromise = null;
    this.pendingSessionToken = null;
    this.hasPendingSessionToken = false;
    this.pendingSessionUnreadOrdinal = null;
  }

  cancelInFlight(): void {
    const hasInFlightRead =
      this.loading ||
      this.loadingOlder ||
      this.reloadPromise !== null ||
      this.loadOlderPromise !== null;
    if (
      hasInFlightRead &&
      this.abortController &&
      !this.abortController.signal.aborted
    ) {
      this.cancelledSessionId = this.sessionId;
      this.abortController.abort();
      this.abortController = null;
    }
    this.loading = false;
    this.loadingOlder = false;
    this.reloadPromise = null;
    this.reloadSessionId = null;
    this.pendingReload = false;
    this.loadOlderPromise = null;
  }

  private async fetchPages(
    id: string,
    opts: FetchPageOptions,
  ): Promise<Message[]> {
    const loaded: Message[] = [];
    let from = opts.from;

    for (;;) {
      configureGeneratedClient();
      const res = await withAbort(
        SessionsService.getApiV1SessionsIdMessages({
          id,
          from,
          limit: opts.limit,
          direction: opts.direction,
        }) as unknown as Promise<MessagesResponse>,
        opts.signal,
      );
      if (res.messages.length === 0) break;

      loaded.push(...res.messages);

      if (res.messages.length < opts.limit) break;
      const last = res.messages[res.messages.length - 1];
      if (!last) break;

      const nextFrom = opts.direction === "asc" ? last.ordinal + 1 : last.ordinal - 1;
      if (opts.direction === "asc" ? nextFrom <= from : nextFrom >= from) {
        break;
      }
      from = nextFrom;
    }

    return loaded;
  }

  private async loadAllMessages(id: string, signal: AbortSignal, messageCountHint?: number) {
    let from = 0;
    let loaded: Message[] = [];
    let complete = false;

    for (;;) {
      configureGeneratedClient();
      const res = await withAbort(
        SessionsService.getApiV1SessionsIdMessages({
          id,
          from,
          limit: MESSAGE_PAGE_SIZE,
          direction: "asc",
        }) as unknown as Promise<MessagesResponse>,
        signal,
      );
      if (this.sessionId !== id) return;
      if (res.messages.length === 0) {
        complete = true;
        break;
      }

      loaded = [...loaded, ...res.messages];
      this.messages = loaded;

      const newest = loaded[loaded.length - 1];
      this.messageCount = messageCountHint ?? (newest ? newest.ordinal + 1 : loaded.length);
      this.hasOlder = false;

      if (res.messages.length < MESSAGE_PAGE_SIZE) {
        complete = true;
        break;
      }
      const last = res.messages[res.messages.length - 1];
      if (!last) break;
      const nextFrom = last.ordinal + 1;
      if (nextFrom <= from) break;
      from = nextFrom;
    }

    const newest = this.messages[this.messages.length - 1];
    if (this.sessionId !== id) return;
    this.messageCount = messageCountHint ?? (newest ? newest.ordinal + 1 : this.messages.length);
    this.hasOlder = false;
    this.historyComplete = complete;
  }

  private async loadProgressively(id: string, signal: AbortSignal) {
    configureGeneratedClient();
    const firstRes = await withAbort(
      SessionsService.getApiV1SessionsIdMessages({
        id,
        limit: MESSAGE_PAGE_SIZE,
        direction: "desc",
      }) as unknown as Promise<MessagesResponse>,
      signal,
    );
    if (this.sessionId !== id) return;

    this.messages = [...firstRes.messages].reverse();
    this.historyComplete = false;
    const newest = this.messages[this.messages.length - 1];
    this.messageCount = newest ? newest.ordinal + 1 : 0;
    const oldest = this.messages[0]?.ordinal;
    this.hasOlder = oldest !== undefined ? oldest > 0 : false;
    this.historyComplete = !this.hasOlder;
  }

  private async loadFrom(id: string, from: number, signal: AbortSignal) {
    const pages = await this.fetchPages(id, {
      from,
      limit: MESSAGE_PAGE_SIZE,
      direction: "asc",
      signal,
    });
    if (this.sessionId !== id) return;
    if (pages.length > 0) {
      const updates = new Map(pages.map((m) => [m.ordinal, m]));
      const existingOrdinals = new Set(this.messages.map((m) => m.ordinal));
      const appended = pages.filter((m) => !existingOrdinals.has(m.ordinal));
      clearContentCaches();
      this.messages = [...this.messages.map((m) => updates.get(m.ordinal) ?? m), ...appended];
    }
  }

  async loadOlder() {
    if (!this.sessionId || this.loadOlderPromise || !this.hasOlder || this.messages.length === 0) {
      return this.loadOlderPromise ?? undefined;
    }

    const p = this.doLoadOlder().finally(() => {
      if (this.loadOlderPromise === p) {
        this.loadOlderPromise = null;
      }
    });
    this.loadOlderPromise = p;
    return p;
  }

  private async doLoadOlder() {
    const id = this.sessionId;
    if (!id || this.messages.length === 0) return;

    const oldest = this.messages[0]!.ordinal;
    if (oldest <= 0) {
      this.hasOlder = false;
      this.historyComplete = true;
      this.publishPendingSessionToken(id);
      return;
    }

    const signal = this.abortController?.signal;
    if (!signal || signal.aborted) return;

    this.loadingOlder = true;
    try {
      configureGeneratedClient();
      const res = await withAbort(
        SessionsService.getApiV1SessionsIdMessages({
          id,
          from: oldest - 1,
          limit: MESSAGE_PAGE_SIZE,
          direction: "desc",
        }) as unknown as Promise<MessagesResponse>,
        signal,
      );
      if (this.sessionId !== id) return;
      if (res.messages.length === 0) {
        this.hasOlder = false;
        this.historyComplete = true;
        this.publishPendingSessionToken(id);
        return;
      }
      const chunk = [...res.messages].reverse();
      this.messages.unshift(...chunk);
      this.hasOlder = chunk[0]!.ordinal > 0;
      this.historyComplete = !this.hasOlder;
      this.publishPendingSessionToken(id);
    } catch (err) {
      if (isAbortError(err)) return;
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Failed to load older messages:", err);
    } finally {
      if (this.sessionId === id) {
        this.loadingOlder = false;
      }
    }
  }

  async ensureOrdinalLoaded(targetOrdinal: number) {
    if (!this.sessionId || this.messages.length === 0) return;

    const id = this.sessionId;
    const oldestLoaded = this.messages[0]!.ordinal;
    if (oldestLoaded <= targetOrdinal) return;
    if (!this.hasOlder) return;

    if (this.loadOlderPromise) {
      await this.loadOlderPromise;
      if (!this.sessionId || this.sessionId !== id) return;
      if (this.messages.length === 0) return;
      if (this.messages[0]!.ordinal <= targetOrdinal) return;
    }

    const p = this.doEnsureOrdinal(id, targetOrdinal).finally(() => {
      if (this.loadOlderPromise === p) {
        this.loadOlderPromise = null;
      }
    });
    this.loadOlderPromise = p;
    return p;
  }

  private async doEnsureOrdinal(id: string, targetOrdinal: number) {
    const signal = this.abortController?.signal;
    if (!signal || signal.aborted) return;

    this.loadingOlder = true;
    try {
      let from = this.messages[0]!.ordinal - 1;
      let lastOldest = this.messages[0]!.ordinal;
      const chunks: Message[][] = [];

      while (from >= 0) {
        configureGeneratedClient();
        const res = await withAbort(
          SessionsService.getApiV1SessionsIdMessages({
            id,
            from,
            limit: MESSAGE_PAGE_SIZE,
            direction: "desc",
          }) as unknown as Promise<MessagesResponse>,
          signal,
        );
        if (this.sessionId !== id) return;
        if (res.messages.length === 0) {
          this.hasOlder = false;
          this.historyComplete = true;
          break;
        }

        const chunk = [...res.messages].reverse();
        chunks.push(chunk);
        const chunkOldest = chunk[0]!.ordinal;

        if (chunkOldest <= targetOrdinal) break;
        if (chunkOldest >= lastOldest) break;

        lastOldest = chunkOldest;
        from = chunkOldest - 1;
      }

      if (this.sessionId !== id) return;

      if (chunks.length > 0) {
        const merged = chunks.reverse().flat();
        this.messages = [...merged, ...this.messages];
      }

      const oldestNow = this.messages[0]?.ordinal;
      this.hasOlder = oldestNow !== undefined && oldestNow > 0;
      this.historyComplete = !this.hasOlder;
      this.publishPendingSessionToken(id);
    } catch (err) {
      if (isAbortError(err)) return;
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Failed to load older messages for ordinal:", err);
    } finally {
      if (this.sessionId === id) {
        this.loadingOlder = false;
      }
    }
  }

  private async reloadNow(id: string) {
    const signal = this.abortController?.signal;
    if (!signal || signal.aborted) return;

    const previousToken = this.activeSessionToken;
    const previousMessages = this.messages;

    try {
      configureGeneratedClient();
      const sess = await withAbort(
        SessionsService.getApiV1SessionsId({ id }) as unknown as Promise<Session>,
        signal,
      );
      if (this.sessionId !== id) return;

      const pendingToken = buildReadProgressToken(sess);
      const newCount = sess.message_count ?? 0;
      const oldCount = this.messageCount;
      if (newCount === oldCount) {
        const refreshed = await this.refreshLoadedWindow(id, signal);
        const newest = this.messages[this.messages.length - 1];
        this.historyComplete =
          refreshed && this.messages[0]?.ordinal === 0 && newest?.ordinal === oldCount - 1;
      } else if (newCount > oldCount && this.messages.length > 0) {
        const oldestOrdinal = this.messages[0]!.ordinal;
        await this.loadFrom(id, oldestOrdinal, signal);
        if (this.sessionId !== id) return;

        const newest = this.messages[this.messages.length - 1];
        if (!newest || newest.ordinal !== newCount - 1) {
          await this.fullReload(id, signal, newCount);
        } else {
          this.messageCount = newCount;
          this.historyComplete = this.messages[0]?.ordinal === 0 && newest.ordinal === newCount - 1;
        }
      } else {
        await this.fullReload(id, signal, newCount);
      }

      if (this.sessionId === id) {
        const unreadOrdinal =
          pendingToken !== previousToken
            ? earliestChangedOrdinal(previousMessages, this.messages)
            : null;
        this.publishOrDeferSessionToken(pendingToken, unreadOrdinal);
      }
    } catch (err) {
      if (isAbortError(err)) return;
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Reload failed:", err);
    }
  }

  private publishOrDeferSessionToken(
    token: string | null,
    unreadOrdinal: number | null,
    deferForOlder: boolean = true,
  ) {
    if (this.hasOlder && deferForOlder) {
      this.pendingSessionToken = token;
      this.hasPendingSessionToken = true;
      this.pendingSessionUnreadOrdinal = 0;
      return;
    }
    this.pendingSessionToken = null;
    this.hasPendingSessionToken = false;
    this.pendingSessionUnreadOrdinal = null;
    this.activeSessionToken = token;
    this.activeSessionUnreadOrdinal = unreadOrdinal;
  }

  private publishPendingSessionToken(id: string) {
    if (this.sessionId !== id || this.hasOlder || !this.hasPendingSessionToken) {
      return;
    }
    this.activeSessionToken = this.pendingSessionToken;
    this.activeSessionUnreadOrdinal = this.pendingSessionUnreadOrdinal;
    this.pendingSessionToken = null;
    this.hasPendingSessionToken = false;
    this.pendingSessionUnreadOrdinal = null;
  }

  private async refreshLoadedWindow(id: string, signal: AbortSignal): Promise<boolean> {
    const oldest = this.messages[0];
    const newest = this.messages[this.messages.length - 1];
    if (!oldest || !newest) return false;

    const refreshed = await this.fetchPages(id, {
      from: oldest.ordinal,
      limit: MESSAGE_PAGE_SIZE,
      direction: "asc",
      signal,
    });
    if (this.sessionId !== id || refreshed.length === 0) {
      return false;
    }

    const updates = new Map(
      refreshed
        .filter((m) => m.ordinal >= oldest.ordinal && m.ordinal <= newest.ordinal)
        .map((m) => [m.ordinal, m]),
    );
    clearContentCaches();
    this.messages = this.messages.map((m) => updates.get(m.ordinal) ?? m);
    return true;
  }

  private async fullReload(id: string, signal: AbortSignal, messageCountHint?: number) {
    clearContentCaches();
    this.loading = true;
    try {
      if (messageCountHint !== undefined && messageCountHint > FULL_SESSION_MESSAGE_THRESHOLD) {
        await this.loadProgressively(id, signal);
      } else {
        await this.loadAllMessages(id, signal, messageCountHint);
      }
    } finally {
      if (this.sessionId === id) {
        this.loading = false;
        this._stableMainModel = this.messages.length > 0 ? computeMainModel(this.messages) : "";
      }
    }
  }
}

function earliestChangedOrdinal(previous: Message[], current: Message[]): number | null {
  const previousByOrdinal = new Map(previous.map((message) => [message.ordinal, message]));
  const currentByOrdinal = new Map(current.map((message) => [message.ordinal, message]));
  const ordinals = new Set([...previousByOrdinal.keys(), ...currentByOrdinal.keys()]);
  let earliest: number | null = null;
  for (const ordinal of ordinals) {
    const before = previousByOrdinal.get(ordinal);
    const after = currentByOrdinal.get(ordinal);
    if (before !== undefined && after !== undefined && transcriptMessageEqual(before, after)) {
      continue;
    }
    earliest = earliest === null ? ordinal : Math.min(earliest, ordinal);
  }
  return earliest;
}

function transcriptMessageEqual(before: Message, after: Message): boolean {
  const visibleContent = (message: Message) => ({
    role: message.role,
    content: message.content,
    thinkingText: message.thinking_text,
    timestamp: message.timestamp,
    hasThinking: message.has_thinking,
    hasToolUse: message.has_tool_use,
    isSystem: message.is_system,
    model: message.model,
    contextTokens: message.context_tokens,
    outputTokens: message.output_tokens,
    hasContextTokens: message.has_context_tokens ?? false,
    hasOutputTokens: message.has_output_tokens ?? false,
    sourceSubtype: message.source_subtype ?? "",
    isCompactBoundary: message.is_compact_boundary ?? false,
    toolCalls: (message.tool_calls ?? []).map((call) => ({
      toolName: call.tool_name,
      category: call.category ?? "",
      toolUseId: call.tool_use_id ?? "",
      inputJson: call.input_json ?? "",
      skillName: call.skill_name ?? "",
      resultContent: call.result_content ?? "",
      subagentSessionId: call.subagent_session_id ?? "",
      resultEvents: (call.result_events ?? []).map((event) => ({
        toolUseId: event.tool_use_id ?? "",
        agentId: event.agent_id ?? "",
        subagentSessionId: event.subagent_session_id ?? "",
        source: event.source,
        status: event.status,
        content: event.content,
        timestamp: event.timestamp ?? "",
        eventIndex: event.event_index,
      })),
    })),
  });
  return JSON.stringify(visibleContent(before)) === JSON.stringify(visibleContent(after));
}

export const messages = new MessagesStore();
