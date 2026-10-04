import { SessionsService } from "../api/generated/index";
import type { DbMessage as Message } from "../api/generated/index.js";
import { ApiError, isAbortError } from "../api/runtime.js";
import { clearContentCaches } from "../utils/content-parser.js";
import { computeMainModelInfo, type ModelEffort } from "../utils/model.js";
import { buildReadProgressToken, readProgress } from "./read-progress.svelte.js";
import { sessions } from "./sessions.svelte.js";

const MESSAGE_PAGE_SIZE = 1000;
const FULL_SESSION_MESSAGE_THRESHOLD = 3_000;

interface FetchPageOptions {
  from: number;
  limit: number;
  direction: "asc" | "desc";
  signal: AbortSignal;
}

const MAX_LOAD_ATTEMPTS = 3;

interface PageQuery {
  from?: number;
  limit: number;
  direction: "asc" | "desc";
}

/** How a read bound to the loaded revision ended: finished, outpaced by a reload, or refused by a moved transcript. */
type RevisionedRead = "done" | "moved" | "reload";

/** How a refresh of the loaded window ended. */
type WindowRefresh = "landed" | "empty" | "superseded";

/** Rows from one transcript revision, as the messages API reported it. */
interface MessagePage {
  messages: Message[];
  revision: string;
}

// A bound read answers 409 once the transcript has moved past the revision it named.
function isRevisionChange(err: unknown): boolean {
  return err instanceof ApiError && err.status === 409;
}

export class MessagesStore {
  messages: Message[] = $state([]);
  loading: boolean = $state(false);
  sessionId: string | null = $state(null);
  messageCount: number = $state(0);
  activeSessionToken: string | null = $state(null);
  activeSessionUnreadOrdinal: number | null = $state(null);
  hasOlder: boolean = $state(false);
  loadingOlder: boolean = $state(false);
  historyComplete: boolean = $state(false);
  /**
   * The transcript revision every row in messages came from, taken from the
   * message responses that delivered them. Null until the first page lands.
   */
  loadedRevision: string | null = $state(null);
  private reloading: boolean = $state(false);
  private _stableMainModelInfo: ModelEffort = $state({
    model: "",
    reasoningEffort: "",
  });
  mainModelInfo: ModelEffort = $derived(
    this.loading ? this._stableMainModelInfo : computeMainModelInfo(this.messages),
  );
  mainModel: string = $derived(this.mainModelInfo.model);
  private abortController: AbortController | null = null;
  private cancelledSessionId: string | null = null;
  // The session id alone cannot tell a stale load's late 404 apart
  // from the current load for the same session, so failures check
  // this before reporting a missing session.
  private loadGeneration: number = 0;
  private reloadPromise: Promise<void> | null = null;
  private reloadSessionId: string | null = null;
  private pendingReload: boolean = false;
  // Back-to-back reloads a moving transcript refused; bounded so a busy session can't reload forever.
  private revisionConflicts: number = 0;
  // Each window load takes a ticket when it starts; a replacement lands only if no later-started load already replaced the window.
  private loadTicket: number = 0;
  private windowTicket: number = 0;
  // The latest-started full load owns `loading`, so an overtaken one can't clear it under its replacement.
  private loadingOwner: number = 0;
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
    const resumesCancelledLoad = this.sessionId === id && this.cancelledSessionId === id;
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
      this.activeSessionToken = null;
      this.activeSessionUnreadOrdinal = null;
    }
    this.sessionId = id;
    this.cancelledSessionId = null;
    this.loading = true;
    const owner = ++this.loadingOwner;

    const generation = ++this.loadGeneration;
    const ac = new AbortController();
    this.abortController = ac;

    // Taken before the metadata read, so a reload that starts while it is in flight outranks this load.
    const ticket = ++this.loadTicket;
    try {
      let countHint: number | null = null;
      let pendingToken: string | null = null;
      try {
        const sess = await SessionsService.getApiV1SessionsById({ id }, { signal: ac.signal });
        countHint = sess.message_count ?? 0;
        pendingToken = buildReadProgressToken(sess);
      } catch (err) {
        if (isAbortError(err)) return;
        // This probe is the only detail fetch a plain click on an
        // already-hydrated sidebar row makes, so it is what detects
        // a session deleted behind a cached row.
        if (this.loadGeneration === generation) {
          sessions.markActiveSessionMissing(id, err);
        }
        console.warn("Failed to fetch session metadata:", err);
      }

      const landed =
        countHint !== null && countHint > FULL_SESSION_MESSAGE_THRESHOLD
          ? await this.loadProgressively(id, ac.signal, ticket)
          : await this.loadAllMessages(id, ac.signal, countHint ?? undefined, ticket);
      // A load a reload overtook leaves the newer read-progress token in place.
      if (landed && this.sessionId === id) {
        const token = this.acceptedToken(pendingToken);
        this.publishOrDeferSessionToken(
          token,
          null,
          readMarker !== null && readMarker.token !== token,
        );
      }
    } catch (err) {
      if (isAbortError(err)) return;
      if (this.sessionId === id) this.historyComplete = false;
      if (this.loadGeneration === generation) {
        sessions.markActiveSessionMissing(id, err);
      }
      console.warn("Failed to load session messages:", err);
    } finally {
      if (this.sessionId === id && this.loadingOwner === owner) {
        this.loading = false;
        this.updateStableMainModelInfo();
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
    this._stableMainModelInfo = { model: "", reasoningEffort: "" };
    this.messageCount = 0;
    this.activeSessionToken = null;
    this.activeSessionUnreadOrdinal = null;
    this.hasOlder = false;
    this.loadingOlder = false;
    this.historyComplete = false;
    this.loadedRevision = null;
    this.reloading = false;
    this.reloadPromise = null;
    this.reloadSessionId = null;
    this.pendingReload = false;
    this.revisionConflicts = 0;
    this.loadOlderPromise = null;
    this.pendingSessionToken = null;
    this.hasPendingSessionToken = false;
    this.pendingSessionUnreadOrdinal = null;
  }

  cancelInFlight(): void {
    // A request that rejected just before the abort is not an
    // AbortError, so retire the load's generation as well.
    this.loadGeneration++;
    const hasInFlightRead =
      this.loading ||
      this.loadingOlder ||
      this.reloadPromise !== null ||
      this.loadOlderPromise !== null;
    if (hasInFlightRead && this.abortController && !this.abortController.signal.aborted) {
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

  /** Fetch one page, bound to revision when one is given so a moved transcript answers 409. */
  private async fetchMessagePage(
    id: string,
    query: PageQuery,
    signal: AbortSignal,
    revision: string | null,
  ): Promise<MessagePage> {
    const res = await SessionsService.getApiV1SessionsByIdMessages(
      { id },
      revision ? { ...query, expected_revision: revision } : query,
      { signal },
    );
    return { messages: res.messages, revision: res.transcript_revision ?? "" };
  }

  /** Take a refreshed window that starts at the loaded window's oldest row. */
  /** False when a load that started later already replaced the window. */
  private acceptWindow(page: MessagePage, append: boolean, ticket: number): boolean {
    if (page.revision !== this.loadedRevision && ticket < this.windowTicket) return false;
    clearContentCaches();
    if (page.revision !== this.loadedRevision) {
      // Rows from two transcript revisions never share the list, so a new revision replaces the window.
      this.windowTicket = ticket;
      this.messages = page.messages;
      this.hasOlder = page.messages[0]!.ordinal > 0;
      this.messageCount = Math.max(this.messageCount, page.messages.at(-1)!.ordinal + 1);
    } else {
      const oldest = this.messages[0]?.ordinal ?? 0;
      const newest = this.messages.at(-1)?.ordinal ?? -1;
      const kept = append
        ? page.messages
        : page.messages.filter((m) => m.ordinal >= oldest && m.ordinal <= newest);
      const updates = new Map(kept.map((m) => [m.ordinal, m]));
      const existingOrdinals = new Set(this.messages.map((m) => m.ordinal));
      const appended = append ? page.messages.filter((m) => !existingOrdinals.has(m.ordinal)) : [];
      this.messages = [...this.messages.map((m) => updates.get(m.ordinal) ?? m), ...appended];
    }
    this.loadedRevision = page.revision;
    this.updateStableMainModelInfo();
    return true;
  }

  /**
   * Whether a page fetched while the window held revision `held` can join it:
   * the window must not have moved on, and the page must share its revision.
   */
  private pageJoinsWindow(held: string | null, page: MessagePage): boolean {
    return this.loadedRevision === held && (held === null || page.revision === held);
  }

  /** Fetch consecutive pages that all belong to the first page's revision. */
  private async fetchPages(id: string, opts: FetchPageOptions): Promise<MessagePage> {
    const loaded: Message[] = [];
    let from = opts.from;
    let revision: string | null = null;

    for (;;) {
      const res = await this.fetchMessagePage(
        id,
        { from, limit: opts.limit, direction: opts.direction },
        opts.signal,
        revision,
      );
      revision ??= res.revision;
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

    return { messages: loaded, revision: revision ?? "" };
  }

  /** Load from the first message; false when the session changed or a later load overtook this one. */
  private async loadAllMessages(
    id: string,
    signal: AbortSignal,
    messageCountHint?: number,
    firstTicket?: number,
  ): Promise<boolean> {
    for (let attempt = 1; ; attempt++) {
      const ticket = attempt === 1 && firstTicket !== undefined ? firstTicket : ++this.loadTicket;
      try {
        return await this.loadAllMessagesOnce(id, signal, ticket, messageCountHint);
      } catch (err) {
        if (!isRevisionChange(err)) throw err;
        // A later load already replaced the window, so this one retires instead of restarting.
        if (ticket < this.windowTicket) return false;
        // A sync landed between pages; start over so every row shares one revision.
        if (attempt >= MAX_LOAD_ATTEMPTS) throw err;
      }
    }
  }

  private async loadAllMessagesOnce(
    id: string,
    signal: AbortSignal,
    ticket: number,
    messageCountHint?: number,
  ): Promise<boolean> {
    this.historyComplete = false;
    let from = 0;
    let loaded: Message[] = [];
    let complete = false;
    let revision: string | null = null;

    for (;;) {
      const res = await this.fetchMessagePage(
        id,
        { from, limit: MESSAGE_PAGE_SIZE, direction: "asc" },
        signal,
        revision,
      );
      if (this.sessionId !== id) return false;
      revision ??= res.revision;
      if (loaded.length === 0) {
        // A load that started later already replaced the window.
        if (ticket < this.windowTicket) return false;
        this.windowTicket = ticket;
        // Rows already parsed may come back rewritten under the same IDs and lengths.
        clearContentCaches();
      } else if (this.windowTicket !== ticket) {
        // A newer revision replaced the window while this page was in flight.
        return false;
      }
      if (res.messages.length === 0) {
        if (loaded.length === 0) {
          // The session has no rows now, so nothing from an earlier revision may stay on screen.
          this.messages = [];
          this.loadedRevision = revision;
        }
        complete = true;
        break;
      }

      loaded = [...loaded, ...res.messages];
      // Every row on screen now comes from this one revision, so it can be published page by page.
      this.messages = loaded;
      this.loadedRevision = revision;

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
    if (this.sessionId !== id) return false;
    this.messageCount = messageCountHint ?? (newest ? newest.ordinal + 1 : this.messages.length);
    this.hasOlder = false;
    this.historyComplete = complete;
    return true;
  }

  /** Load the newest page; false when the session changed or a later load overtook this one. */
  private async loadProgressively(
    id: string,
    signal: AbortSignal,
    ticket = ++this.loadTicket,
  ): Promise<boolean> {
    const firstRes = await this.fetchMessagePage(
      id,
      { limit: MESSAGE_PAGE_SIZE, direction: "desc" },
      signal,
      null,
    );
    // A load that started later already replaced the window.
    if (this.sessionId !== id || ticket < this.windowTicket) return false;
    this.windowTicket = ticket;

    // Rows parsed while this page was in flight may come back rewritten under the same IDs and lengths.
    clearContentCaches();
    this.messages = [...firstRes.messages].reverse();
    this.loadedRevision = firstRes.revision;
    this.historyComplete = false;
    const newest = this.messages[this.messages.length - 1];
    this.messageCount = newest ? newest.ordinal + 1 : 0;
    const oldest = this.messages[0]?.ordinal;
    this.hasOlder = oldest !== undefined ? oldest > 0 : false;
    this.historyComplete = !this.hasOlder;
    return true;
  }

  /** Refresh from `from` onward and say whether rows landed, none came back, or a later load overtook this one. */
  private async loadFrom(id: string, from: number, signal: AbortSignal): Promise<WindowRefresh> {
    const ticket = ++this.loadTicket;
    const page = await this.fetchPages(id, {
      from,
      limit: MESSAGE_PAGE_SIZE,
      direction: "asc",
      signal,
    });
    if (this.sessionId !== id) return "superseded";
    if (page.messages.length === 0) return "empty";
    return this.acceptWindow(page, true, ticket) ? "landed" : "superseded";
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
    const revision = this.loadedRevision;
    try {
      const res = await this.fetchMessagePage(
        id,
        { from: oldest - 1, limit: MESSAGE_PAGE_SIZE, direction: "desc" },
        signal,
        revision,
      );
      if (this.sessionId !== id) return;
      // A reload replaced the window while this page was in flight.
      if (!this.pageJoinsWindow(revision, res)) return;
      this.loadedRevision = res.revision;
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
      if (this.sessionId === id && isRevisionChange(err)) {
        void this.reload();
        return;
      }
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

  /** Complete either a missing prefix or a failed forward load for session find. */
  async ensureHistoryLoaded(): Promise<void> {
    const id = this.sessionId;
    const signal = this.abortController?.signal;
    const current = () =>
      this.sessionId === id && this.abortController?.signal === signal && !signal?.aborted;
    if (!id || !signal || !current() || this.loading) return;
    if (this.hasOlder) {
      await this.ensureOrdinalLoaded(0);
      if (!current() || this.hasOlder) return;
    }
    if (this.historyComplete) return;
    if (this.loadOlderPromise) {
      await this.loadOlderPromise;
      if (!current() || this.historyComplete) return;
    }
    const pending = this.loadRemainingMessages(id, signal).finally(() => {
      if (this.loadOlderPromise === pending) this.loadOlderPromise = null;
    });
    this.loadOlderPromise = pending;
    await pending;
  }

  /** Resume the missing tail without removing already loaded rows or their cursor. */
  private async loadRemainingMessages(id: string, signal: AbortSignal): Promise<void> {
    for (let attempt = 1; ; attempt++) {
      const outcome = await this.loadRemainingMessagesOnce(id, signal);
      if (outcome === "done" || attempt >= MAX_LOAD_ATTEMPTS) return;
      // The transcript moved underneath this read; catch the window up and try again.
      if (outcome === "reload") await this.reload();
      if (this.sessionId !== id || signal.aborted || this.historyComplete) return;
      // The reload may have dropped back to the newest page, so the prefix needs loading again first.
      if (this.hasOlder) await this.doEnsureOrdinal(id, 0);
      if (this.sessionId !== id || signal.aborted || this.historyComplete) return;
    }
  }

  private async loadRemainingMessagesOnce(
    id: string,
    signal: AbortSignal,
  ): Promise<RevisionedRead> {
    const current = () =>
      this.sessionId === id && this.abortController?.signal === signal && !signal.aborted;
    this.loadingOlder = true;
    try {
      let from = (this.messages.at(-1)?.ordinal ?? -1) + 1;
      let revision = this.loadedRevision;
      for (;;) {
        const res = await this.fetchMessagePage(
          id,
          { from, limit: MESSAGE_PAGE_SIZE, direction: "asc" },
          signal,
          revision,
        );
        if (!current()) return "done";
        // A reload replaced the window while this page was in flight.
        if (!this.pageJoinsWindow(revision, res)) return "moved";
        revision = res.revision;
        this.loadedRevision = revision;
        if (res.messages.length === 0) {
          this.historyComplete = !this.hasOlder;
          break;
        }
        const nextFrom = res.messages.at(-1)!.ordinal + 1;
        if (nextFrom <= from) throw new Error("Session history pagination made no progress");
        // Concurrent SSE refreshes may already have appended some of this page.
        // Preserve their objects and never introduce duplicate ordinals.
        const existing = new Set(this.messages.map((message) => message.ordinal));
        const added = res.messages.filter((message) => {
          if (existing.has(message.ordinal)) return false;
          existing.add(message.ordinal);
          return true;
        });
        clearContentCaches();
        this.messages = [...this.messages, ...added].sort((a, b) => a.ordinal - b.ordinal);
        this.messageCount = Math.max(this.messageCount, this.messages.at(-1)!.ordinal + 1);
        if (res.messages.length < MESSAGE_PAGE_SIZE) {
          this.historyComplete = !this.hasOlder;
          break;
        }
        from = nextFrom;
      }
      this.publishPendingSessionToken(id);
      return "done";
    } catch (error) {
      if (isAbortError(error) || !current()) return "done";
      if (isRevisionChange(error)) return "reload";
      this.historyComplete = false;
      console.warn("Failed to complete session history:", error);
      return "done";
    } finally {
      if (current()) this.loadingOlder = false;
    }
  }

  private async doEnsureOrdinal(id: string, targetOrdinal: number): Promise<void> {
    for (let attempt = 1; ; attempt++) {
      const outcome = await this.ensureOrdinalOnce(id, targetOrdinal);
      if (outcome === "done" || attempt >= MAX_LOAD_ATTEMPTS) return;
      // The transcript moved underneath this read; catch the window up and try again.
      if (outcome === "reload") await this.reload();
      const oldest = this.messages[0]?.ordinal;
      if (
        this.sessionId !== id ||
        oldest === undefined ||
        oldest <= targetOrdinal ||
        !this.hasOlder
      ) {
        return;
      }
    }
  }

  private async ensureOrdinalOnce(id: string, targetOrdinal: number): Promise<RevisionedRead> {
    const signal = this.abortController?.signal;
    if (!signal || signal.aborted) return "done";

    this.loadingOlder = true;
    try {
      let from = this.messages[0]!.ordinal - 1;
      let lastOldest = this.messages[0]!.ordinal;
      const chunks: Message[][] = [];
      const held = this.loadedRevision;
      let revision = held;

      while (from >= 0) {
        const res = await this.fetchMessagePage(
          id,
          { from, limit: MESSAGE_PAGE_SIZE, direction: "desc" },
          signal,
          revision,
        );
        if (this.sessionId !== id) return "done";
        // A reload replaced the window while this page was in flight.
        if (this.loadedRevision !== held || (revision !== null && res.revision !== revision)) {
          return "moved";
        }
        revision = res.revision;
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

      if (this.sessionId !== id) return "done";

      if (chunks.length > 0) {
        const merged = chunks.reverse().flat();
        this.messages = [...merged, ...this.messages];
        this.loadedRevision = revision;
      }

      const oldestNow = this.messages[0]?.ordinal;
      this.hasOlder = oldestNow !== undefined && oldestNow > 0;
      this.historyComplete = !this.hasOlder;
      this.publishPendingSessionToken(id);
      return "done";
    } catch (err) {
      if (isAbortError(err)) return "done";
      if (this.sessionId === id && isRevisionChange(err)) return "reload";
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Failed to load older messages for ordinal:", err);
      return "done";
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
      const sess = await SessionsService.getApiV1SessionsById({ id }, { signal });
      if (this.sessionId !== id) return;

      const pendingToken = buildReadProgressToken(sess);
      const newCount = sess.message_count ?? 0;
      const oldCount = this.messageCount;
      let landed = true;
      if (newCount === oldCount) {
        const refreshed = await this.refreshLoadedWindow(id, signal);
        if (this.sessionId !== id) return;
        if (refreshed === "superseded") {
          landed = false;
        } else if (refreshed === "empty") {
          // No window to refresh, or it came back empty because the transcript shrank underneath it.
          landed = await this.fullReload(id, signal, newCount);
        } else {
          const newest = this.messages[this.messages.length - 1];
          this.historyComplete =
            this.messages[0]?.ordinal === 0 && newest?.ordinal === oldCount - 1;
        }
      } else if (newCount > oldCount && this.messages.length > 0) {
        const oldestOrdinal = this.messages[0]!.ordinal;
        const refreshed = await this.loadFrom(id, oldestOrdinal, signal);
        if (this.sessionId !== id) return;

        const newest = this.messages[this.messages.length - 1];
        if (refreshed === "superseded") {
          landed = false;
        } else if (refreshed === "empty" || !newest || newest.ordinal !== newCount - 1) {
          // An empty refresh means the transcript shrank underneath the window.
          landed = await this.fullReload(id, signal, newCount);
        } else {
          this.messageCount = newCount;
          this.historyComplete = this.messages[0]?.ordinal === 0 && newest.ordinal === newCount - 1;
        }
      } else {
        landed = await this.fullReload(id, signal, newCount);
      }

      // A reload a newer load overtook leaves that load's read-progress token in place.
      if (landed && this.sessionId === id) {
        const token = this.acceptedToken(pendingToken);
        const unreadOrdinal =
          token !== previousToken ? earliestChangedOrdinal(previousMessages, this.messages) : null;
        this.publishOrDeferSessionToken(token, unreadOrdinal);
        this.revisionConflicts = 0;
      }
    } catch (err) {
      if (isAbortError(err)) return;
      // A sync landed between pages; loadedRevision still names only rows that arrived, and another pass catches up.
      if (this.sessionId === id && isRevisionChange(err)) {
        if (++this.revisionConflicts < MAX_LOAD_ATTEMPTS) this.pendingReload = true;
        return;
      }
      if (this.sessionId === id) this.historyComplete = false;
      console.warn("Reload failed:", err);
    }
  }

  /** The read-progress token for the rows on screen: their revision, or the metadata's when the backend reports none. */
  private acceptedToken(metadataToken: string | null): string | null {
    return this.loadedRevision?.trim() || metadataToken;
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

  private async refreshLoadedWindow(id: string, signal: AbortSignal): Promise<WindowRefresh> {
    const oldest = this.messages[0];
    const newest = this.messages[this.messages.length - 1];
    if (!oldest || !newest) return "empty";

    const ticket = ++this.loadTicket;
    const refreshed = await this.fetchPages(id, {
      from: oldest.ordinal,
      limit: MESSAGE_PAGE_SIZE,
      direction: "asc",
      signal,
    });
    if (this.sessionId !== id) return "superseded";
    if (refreshed.messages.length === 0) return "empty";
    return this.acceptWindow(refreshed, false, ticket) ? "landed" : "superseded";
  }

  /** Reload from scratch; false when the session changed or a later load overtook this one. */
  private async fullReload(
    id: string,
    signal: AbortSignal,
    messageCountHint?: number,
  ): Promise<boolean> {
    clearContentCaches();
    this.loading = true;
    const owner = ++this.loadingOwner;
    try {
      return messageCountHint !== undefined && messageCountHint > FULL_SESSION_MESSAGE_THRESHOLD
        ? await this.loadProgressively(id, signal)
        : await this.loadAllMessages(id, signal, messageCountHint);
    } finally {
      if (this.sessionId === id && this.loadingOwner === owner) {
        this.loading = false;
        this.updateStableMainModelInfo();
      }
    }
  }

  private updateStableMainModelInfo() {
    this._stableMainModelInfo = computeMainModelInfo(this.messages);
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
    reasoningEffort: message.reasoning_effort ?? "",
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
