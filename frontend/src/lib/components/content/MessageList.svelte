<script lang="ts">
  import { errorTelemetry } from "../../utils/error-telemetry.js";
  import { onDestroy, tick, untrack } from "svelte";
  import { SvelteSet } from "svelte/reactivity";
  import { Button, EmptyState } from "@kenn-io/kit-ui";
  // kit-ui-check-ignore: MessageList uses the local TanStack wrapper for pinned-message scroll reconciliation and per-session measurement cache resets; kit-ui VirtualList does not expose those controls yet.
  import type { Virtualizer } from "@tanstack/virtual-core";
  import { messages } from "../../stores/messages.svelte.js";
  import { ui, type ScrollCall } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { readProgress } from "../../stores/read-progress.svelte.js";
  import { CircleQuestionMarkIcon, MessageSquareIcon } from "../../icons.js";
  import { cancelScrollReconcile, createVirtualizer } from "../../virtual/createVirtualizer.svelte.js";
  import MessageContent from "./MessageContent.svelte";
  import CompactBoundaryDivider from "./CompactBoundaryDivider.svelte";
  import EmptyMessagesRow from "./EmptyMessagesRow.svelte";
  import SystemBoundaryCard from "../system/SystemBoundaryCard.svelte";
  import ToolCallGroup from "./ToolCallGroup.svelte";
  import type { DbMessage as Message } from "../../api/generated/index.js";
  import type { DisplayItem } from "../../utils/display-items.js";
  import {
    isSystemBoundaryMessage,
    isSystemMessage,
  } from "../../utils/messages.js";
  import { resolveMessageLayout } from "../../utils/message-layout.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { sessionActivity } from "../../stores/sessionActivity.svelte.js";
  import SessionFindView from "./SessionFindView.svelte";
  import {
    getLatestDisplayIndex,
    type ScrollAlign,
  } from "./message-scroll.js";
  import { m } from "../../i18n/index.js";
  import { settleVirtualScroll } from "./staged-scroll.js";
  import { toolCallFingerprint } from "../../utils/tool-call-fingerprint.js";
  import { revealMatch } from "../../search/reveal.js";
  import type { Match } from "../../search/session-index.js";
  import {
    keepsAnswerBeforeTrailingTools,
    projectSessionScope,
  } from "../../search/session-scope.js";

  let containerRef: HTMLDivElement | undefined = $state(undefined);
  let containerScrollTop = $state(0);
  let scrollRaf: number | null = null;
  let lastScrollRequest = 0;
  let destroyed = false;
  let activeFollowScrollRequest: number | null = null;
  let followingScrollRaf: number | null = null;
  let followSettleTimer:
    | ReturnType<typeof setTimeout>
    | null = null;
  let visibleProgressSignature: string | null = $state(null);
  let visibleProgressRaf: number | null = null;
  let unreadTraversalKey: string | null = null;
  let unreadBoundarySeen = false;
  let unreadLatestSeen = false;

  let baseMessages: Message[] = $derived.by(() =>
    messages.messages.filter((m) => !isSystemMessage(m)),
  );

  // Share transcript row visibility and searchable block filters with the index.
  let sessionScope = $derived(
    projectSessionScope({
      messages: messages.messages,
      transcriptMode: ui.transcriptMode,
      visibleBlocks: ui.visibleBlocks,
      hasBlockFilters: ui.hasBlockFilters,
      keepAnswerBeforeTrailingTools: keepsAnswerBeforeTrailingTools(
        settings.sessionProviders,
        sessions.activeSession?.agent,
      ),
    }),
  );

  let displayItemsAsc = $derived(sessionScope.items);

  let normalDisplayItemsAsc = $derived(sessionScope.normalItems);

  let displayedOrdinals = $derived.by(() =>
    displayItemsAsc.flatMap((item) => item.ordinals),
  );

  let displayedOrdinalsSignature = $derived(
    displayedOrdinals.join(","),
  );

  function itemAt(index: number, newestFirst = ui.sortNewestFirst) {
    if (newestFirst) {
      const mapped = displayItemsAsc.length - 1 - index;
      return displayItemsAsc[mapped];
    }
    return displayItemsAsc[index];
  }

  const virtualizer = createVirtualizer(() => {
    const count = displayItemsAsc.length;
    const el = containerRef ?? null;
    const sid = sessions.activeSessionId ?? "";
    const newestFirst = ui.sortNewestFirst;
    return {
      count,
      getScrollElement: () => el,
      estimateSize: () => 120,
      overscan: 5,
      useAnimationFrameWithResizeObserver: true,
      measureCacheKey: sid,
      getItemKey: (index: number) => {
        const item = itemAt(index, newestFirst);
        if (!item) return `${sid}-${index}`;
        if (item.kind === "tool-group") {
          return `${sid}-tg-${item.ordinals[0]}`;
        }
        if (item.kind === "empty-group") {
          return `${sid}-eg-${item.ordinals[0]}`;
        }
        return `${sid}-m-${item.message.ordinal}`;
      },
    };
  });

  /** Svelte action: measure element for variable-height virtualizer */
  function measureElement(
    node: HTMLElement,
    virt: Virtualizer<HTMLElement, HTMLElement> | undefined,
  ) {
    virt?.measureElement(node);
    return {
      update(
        nextVirt:
          | Virtualizer<HTMLElement, HTMLElement>
          | undefined,
      ) {
        nextVirt?.measureElement(node);
      },
      destroy() {
        // Cleanup handled by virtualizer
      },
    };
  }

  function publishVisibleTimestamp() {
    const v = virtualizer.instance;
    if (!v) return;
    const items = v.getVirtualItems();
    // Skip overscanned items above the viewport.
    const scrollTop = v.scrollOffset ?? 0;
    for (const vi of items) {
      if (vi.end <= scrollTop) continue;
      const item =
        displayItemsAsc[
          ui.sortNewestFirst
            ? displayItemsAsc.length - 1 - vi.index
            : vi.index
        ];
      if (!item) continue;
      const ts =
        item.kind === "message"
          ? item.message.timestamp
          : item.timestamp;
      if (ts) {
        sessionActivity.firstVisibleTimestamp = ts;
        return;
      }
    }
    sessionActivity.firstVisibleTimestamp = null;
  }

  function recordVisibleProgress() {
    const v = virtualizer.instance;
    const sessionId = messages.sessionId;
    const currentToken = messages.activeSessionToken;
    const marker = sessionId
      ? readProgress.get(sessionId)
      : null;
    if (
      !v ||
      !sessionId ||
      !currentToken ||
      !marker ||
      marker.token === currentToken
    ) {
      return;
    }

    if (baseMessages.length === 0) {
      readProgress.markRead(
        sessionId,
        currentToken,
        latestRawLoadedOrdinal,
      );
      return;
    }

    const latestDisplayedOrdinal = displayedOrdinals.at(-1);
    if (latestDisplayedOrdinal === undefined) {
      readProgress.markRead(
        sessionId,
        currentToken,
        latestLoadedOrdinal,
      );
      return;
    }

    const top = v.scrollOffset ?? containerRef?.scrollTop ?? 0;
    const height = containerRef?.clientHeight || v.scrollRect?.height || 0;
    const bottom = top + height;
    let maxVisibleOrdinal: number | null = null;
    const visibleOrdinals = new Set<number>();

    for (const row of v.getVirtualItems()) {
      if (row.end <= top || row.start >= bottom) continue;
      const item = itemAt(row.index);
      if (!item) continue;
      if (item.kind === "message") {
        visibleOrdinals.add(item.message.ordinal);
        maxVisibleOrdinal = maxVisibleOrdinal === null
          ? item.message.ordinal
          : Math.max(maxVisibleOrdinal, item.message.ordinal);
        continue;
      }

      // A closed empty row stands for all its members; an open one has a
      // card per member, so only cards on screen count as read.
      const groupOrdinals = item.kind === "empty-group" && !emptyGroupOpen(item)
        ? item.ordinals
        : visibleToolGroupOrdinals(row.index);
      for (const ordinal of groupOrdinals) {
        visibleOrdinals.add(ordinal);
        maxVisibleOrdinal = maxVisibleOrdinal === null
          ? ordinal
          : Math.max(maxVisibleOrdinal, ordinal);
      }
    }

    if (maxVisibleOrdinal === null || latestLoadedOrdinal === null) return;

    const rawUnreadBoundary = unreadBoundaryOrdinal(
      latestLoadedOrdinal,
    );
    const unreadBoundary = displayedOrdinals.find((ordinal) =>
      ordinal >= rawUnreadBoundary
    ) ?? latestDisplayedOrdinal;
    const traversalKey =
      `${sessionId}|${currentToken}|${unreadBoundary}|${latestDisplayedOrdinal}`;
    if (unreadTraversalKey !== traversalKey) {
      unreadTraversalKey = traversalKey;
      unreadBoundarySeen = false;
      unreadLatestSeen = false;
    }
    if (visibleOrdinals.has(unreadBoundary)) {
      unreadBoundarySeen = true;
    }
    if (visibleOrdinals.has(latestDisplayedOrdinal)) {
      unreadLatestSeen = true;
    }

    if (ui.sortNewestFirst) {
      if (unreadBoundarySeen && unreadLatestSeen) {
        readProgress.markRead(
          sessionId,
          currentToken,
          latestLoadedOrdinal,
        );
      }
      return;
    }

    if (
      unreadBoundarySeen &&
      maxVisibleOrdinal >= latestDisplayedOrdinal
    ) {
      readProgress.markRead(
        sessionId,
        currentToken,
        latestLoadedOrdinal,
      );
      return;
    }
  }

  function visibleToolGroupOrdinals(
    rowIndex: number,
  ): number[] {
    if (!containerRef) return [];
    const row = containerRef.querySelector<HTMLElement>(
      `.virtual-row[data-index="${rowIndex}"]`,
    );
    if (!row) return [];
    const rootRect = containerRef.getBoundingClientRect();
    const ordinals: number[] = [];
    for (const node of row.querySelectorAll<HTMLElement>("[data-message-ordinal]")) {
      const ordinal = Number(node.dataset.messageOrdinal);
      if (!Number.isInteger(ordinal) || ordinal < 0) continue;
      const rect = node.getBoundingClientRect();
      if (rect.bottom <= rootRect.top || rect.top >= rootRect.bottom) continue;
      ordinals.push(ordinal);
    }
    return ordinals;
  }

  // Recompute visible timestamp when minimap opens or
  // message content changes (e.g. SSE reload).
  $effect(() => {
    if (ui.vitalsOpen) {
      // Track message array so the effect re-runs after
      // content changes while the minimap is open.
      void messages.messages.length;
      publishVisibleTimestamp();
    }
  });

  let latestLoadedOrdinal = $derived(
    baseMessages[baseMessages.length - 1]?.ordinal ?? null,
  );

  let latestRawLoadedOrdinal = $derived(
    messages.messages[messages.messages.length - 1]?.ordinal ?? null,
  );

  function unreadBoundaryOrdinal(
    latestOrdinal: number,
  ): number {
    const explicit = messages.activeSessionUnreadOrdinal;
    const earliestOrdinal = baseMessages[0]?.ordinal ?? latestOrdinal;
    const boundary = explicit ?? earliestOrdinal;
    return baseMessages.find((message) =>
      message.ordinal >= boundary
    )?.ordinal ?? latestOrdinal;
  }

  $effect(() => {
    const sessionId = messages.sessionId;
    const currentToken = messages.activeSessionToken;
    const loading = messages.loading;
    const latestOrdinal =
      latestLoadedOrdinal ?? latestRawLoadedOrdinal;
    if (!sessionId || !currentToken || loading) return;
    readProgress.baseline(sessionId, currentToken, latestOrdinal);
  });

  $effect(() => {
    const sessionId = messages.sessionId;
    const currentToken = messages.activeSessionToken;
    const loading = messages.loading;
    const count = messages.messageCount;
    const latest = latestDisplaySignature();
    const displayed = displayedOrdinalsSignature;
    const unreadOrdinal = messages.activeSessionUnreadOrdinal;
    if (!sessionId || !currentToken || loading || !containerRef) return;
    const signature =
      `${sessionId}|${currentToken}|${count}|${latest}|${unreadOrdinal}|${displayed}`;
    if (
      visibleProgressSignature === null ||
      !visibleProgressSignature.startsWith(`${sessionId}|`)
    ) {
      visibleProgressSignature = signature;
      scheduleVisibleProgress(sessionId, currentToken);
      return;
    }
    if (visibleProgressSignature === signature) return;
    visibleProgressSignature = signature;
    scheduleVisibleProgress(sessionId, currentToken);
  });

  function scheduleVisibleProgress(
    sessionId: string,
    currentToken: string,
  ) {
    if (visibleProgressRaf !== null) {
      cancelAnimationFrame(visibleProgressRaf);
    }
    visibleProgressRaf = requestAnimationFrame(() => {
      visibleProgressRaf = null;
      if (
        messages.sessionId !== sessionId ||
        messages.loading ||
        messages.activeSessionToken !== currentToken
      ) {
        return;
      }
      recordVisibleProgress();
    });
  }

  function handleScroll() {
    if (!containerRef) return;
    containerScrollTop = containerRef.scrollTop;
    if (scrollRaf !== null) return;
    scrollRaf = requestAnimationFrame(() => {
      scrollRaf = null;
      if (!containerRef) return;
      const items =
        virtualizer.instance?.getVirtualItems() ?? [];
      if (items.length > 0 && messages.hasOlder) {
        const firstVisible = items[0]!.index;
        const lastVisible =
          items[items.length - 1]!.index;
        const threshold = 30;
        if (
          (ui.sortNewestFirst &&
            lastVisible >=
              displayItemsAsc.length - threshold) ||
          (!ui.sortNewestFirst &&
            firstVisible <= threshold)
        ) {
          messages.loadOlder();
        }
      }

      if (ui.vitalsOpen) {
        publishVisibleTimestamp();
      }

      recordVisibleProgress();

    });
  }

  function handleManualScrollIntent() {
    endScrollOwnership();
    if (ui.followLatest) {
      cancelFollowLatestWork();
      ui.setFollowLatest(false);
    }
  }

  function manualScrollIntent(node: HTMLElement) {
    const handleKeydown = (event: KeyboardEvent) => {
      if (
        [
          "ArrowDown",
          "ArrowUp",
          "End",
          "Home",
          "PageDown",
          "PageUp",
          " ",
        ].includes(event.key)
      ) {
        handleManualScrollIntent();
      }
    };
    node.addEventListener("wheel", handleManualScrollIntent, {
      passive: true,
    });
    node.addEventListener("pointerdown", handleManualScrollIntent);
    node.addEventListener("touchmove", handleManualScrollIntent, {
      passive: true,
    });
    node.addEventListener("keydown", handleKeydown);
    return {
      destroy() {
        node.removeEventListener(
          "wheel",
          handleManualScrollIntent,
        );
        node.removeEventListener(
          "pointerdown",
          handleManualScrollIntent,
        );
        node.removeEventListener(
          "touchmove",
          handleManualScrollIntent,
        );
        node.removeEventListener("keydown", handleKeydown);
      },
    };
  }

  onDestroy(() => {
    destroyed = true;
    endScrollOwnership();
    if (visibleProgressRaf !== null) {
      cancelAnimationFrame(visibleProgressRaf);
      visibleProgressRaf = null;
    }
    if (scrollRaf !== null) {
      cancelAnimationFrame(scrollRaf);
      scrollRaf = null;
    }
    if (followingScrollRaf !== null) {
      cancelAnimationFrame(followingScrollRaf);
      followingScrollRaf = null;
    }
    if (followSettleTimer !== null) {
      clearTimeout(followSettleTimer);
      followSettleTimer = null;
    }
  });

  function cancelFollowLatestWork() {
    if (
      activeFollowScrollRequest !== null &&
      activeFollowScrollRequest === lastScrollRequest
    ) {
      endScrollOwnership(activeFollowScrollRequest);
    }
    activeFollowScrollRequest = null;
    if (followingScrollRaf !== null) {
      cancelAnimationFrame(followingScrollRaf);
      followingScrollRaf = null;
    }
    if (followSettleTimer !== null) {
      clearTimeout(followSettleTimer);
      followSettleTimer = null;
    }
  }

  function requestIsCurrent(reqId: number, sessionId: string | null): boolean {
    return !destroyed && reqId === lastScrollRequest &&
      messages.sessionId === sessionId && sessions.activeSessionId === sessionId;
  }

  function scrollToDisplayIndex(
    getIndex: () => number,
    waitFrames = 0,
    scrollRetries = 0,
    reqId = lastScrollRequest,
    align: ScrollAlign = "start",
    stillValid: () => boolean = () => true,
  ): Promise<boolean> {
    const sessionId = messages.sessionId;
    return settleVirtualScroll({
      getIndex, align, waitFrames, scrollRetries,
      getVirtualizer: () => virtualizer.instance,
      getCount: () => displayItemsAsc.length,
      isCurrent: () => requestIsCurrent(reqId, sessionId) && stillValid(),
      nextFrame: raf,
    });
  }

  function raf(): Promise<void> {
    return new Promise((r) => requestAnimationFrame(() => r()));
  }

  // Ordinals of empty messages whose collapsed row is open. Ordinals are
  // per session, so a session change closes every row.
  const openEmptyOrdinals = new SvelteSet<number>();
  $effect(() => {
    void messages.sessionId;
    untrack(() => openEmptyOrdinals.clear());
  });

  function setEmptyGroupOpen(ordinals: number[], open: boolean) {
    for (const ordinal of ordinals) {
      if (open) openEmptyOrdinals.add(ordinal);
      else openEmptyOrdinals.delete(ordinal);
    }
  }

  function emptyGroupOpen(item: DisplayItem): boolean {
    return item.ordinals.some((ordinal) => openEmptyOrdinals.has(ordinal));
  }

  // A click on a card inside an open empty row selects that card's message.
  function clickedEmptyMemberOrdinal(
    item: DisplayItem,
    target: EventTarget | null,
  ): number | undefined {
    if (item.kind !== "empty-group" || !(target instanceof Element)) return undefined;
    const card = target.closest<HTMLElement>("[data-message-ordinal]");
    const ordinal = Number(card?.dataset.messageOrdinal);
    return item.ordinals.includes(ordinal) ? ordinal : undefined;
  }

  // Distance from the top of a display row to the card of `ordinal` inside an
  // open empty row, so a jump lands on that card instead of the row's top.
  function emptyMemberOffset(index: number, ordinal: number): number {
    const item = itemAt(index);
    if (!containerRef || item?.kind !== "empty-group") return 0;
    const row = containerRef.querySelector<HTMLElement>(`.virtual-row[data-index="${index}"]`);
    const card = row?.querySelector<HTMLElement>(`[data-message-ordinal="${ordinal}"]`);
    if (!row || !card) return 0;
    return card.getBoundingClientRect().top - row.getBoundingClientRect().top;
  }

  function displayIndexOf(ordinal: number): number {
    const ascIndex = displayItemsAsc.findIndex((item) => item.ordinals.includes(ordinal));
    if (ascIndex < 0) return -1;
    return ui.sortNewestFirst ? displayItemsAsc.length - 1 - ascIndex : ascIndex;
  }

  // Updating until must re-arm the expiry timer, so this object uses deep reactive state.
  let ordinalJump: {
    reqId: number;
    ordinal: number;
    sessionId: string | null;
    until: number;
    offset?: number;
    sawLoadingOlder: boolean;
    targetHolds: () => boolean;
  } | null = $state(null);

  let ordinalJumpIndex = $derived.by(() => ordinalJump ? displayIndexOf(ordinalJump.ordinal) : -1);

  $effect(() => {
    const jump = ordinalJump;
    if (!jump || messages.loadingOlder) return;
    const timer = setTimeout(() => {
      if (ordinalJump !== jump) return;
      endScrollOwnership(jump.reqId);
    }, Math.max(0, jump.until - performance.now()));
    return () => clearTimeout(timer);
  });

  $effect(() => {
    const jump = ordinalJump;
    if (!jump) return;
    // TanStack may not notify when scrolling stays within the same visible rows.
    void containerScrollTop;
    const items = displayItemsAsc;
    const loadingOlder = messages.loadingOlder;
    const instance = virtualizer.instance;
    const now = performance.now();
    const targetHolds = jump.targetHolds();
    if (!requestIsCurrent(jump.reqId, jump.sessionId) || !targetHolds) {
      endScrollOwnership(jump.reqId);
      return;
    }
    if (loadingOlder) {
      jump.sawLoadingOlder = true;
    } else if (jump.sawLoadingOlder) {
      jump.until = now + 5000;
      jump.sawLoadingOlder = false;
    }
    const index = ordinalJumpIndex;
    if (index < 0 || index >= items.length) {
      endScrollOwnership(jump.reqId);
      return;
    }
    if (!instance) return;
    // getOffsetForIndex reads a measurement cache that stays stale after a count change until
    // TanStack's private getMeasurements() rebuilds it; getTotalSize() runs that rebuild publicly.
    // getOffsetForIndex also initializes scrollOffset through TanStack's private getter.
    instance.getTotalSize();
    const rowOffset = instance.getOffsetForIndex(index, "start")?.[0];
    if (rowOffset === undefined) return;
    const memberOffset = emptyMemberOffset(index, jump.ordinal);
    const offset = rowOffset + memberOffset;
    const previousOffset = untrack(() => jump.offset);
    jump.offset = offset;
    if (Math.abs(offset - (instance.scrollOffset ?? 0)) <= 1) return;
    if (previousOffset === offset) {
      // The viewport moved without a layout change, including focus and browser-find scrolling.
      endScrollOwnership(jump.reqId);
      return;
    }
    if (memberOffset > 0) instance.scrollToOffset(offset);
    else instance.scrollToIndex(index, { align: "start" });
  });

  function endScrollOwnership(reqId = lastScrollRequest): number {
    if (reqId !== lastScrollRequest) return lastScrollRequest;
    cancelScrollReconcile(untrack(() => virtualizer.instance));
    ordinalJump = null;
    return ++lastScrollRequest;
  }

  async function scrollToOrdinalInternal(ordinal: number, call?: ScrollCall) {
    const reqId = endScrollOwnership();
    const sessionId = messages.sessionId;
    const isCurrent = () => requestIsCurrent(reqId, sessionId);
    activeFollowScrollRequest = null;
    const abandon = () => {
      if (isCurrent() && ui.selectedOrdinal === ordinal) ui.selectedOrdinal = null;
      return false;
    };
    // A rewrite can renumber messages, so a jump to a tool call stops once its ordinal holds a different message.
    const targetHolds = () => {
      if (call === undefined) return true;
      // Without a tool ID, a position can name another call after a rewrite, so the revision must still match.
      if (!call.toolUseId && call.revision !== undefined && messages.loadedRevision !== call.revision) {
        return abandon();
      }
      if (call.index === undefined) return true;
      const message = messages.messages.find((m) => m.ordinal === ordinal);
      if (message === undefined) return true;
      const held = message.tool_calls?.[call.index];
      if (held === undefined || (held.tool_use_id ?? "") !== call.toolUseId) return abandon();
      // Some agents derive tool IDs from the call's position, so a rewrite can reuse one for another call.
      return (call.fingerprint === undefined ||
        toolCallFingerprint(held.tool_name, held.input_json ?? "") === call.fingerprint) || abandon();
    };
    if (!isCurrent() || !targetHolds()) return endScrollOwnership(reqId);

    if (!messages.messages.some((message) => message.ordinal === ordinal)) {
      await messages.ensureOrdinalLoaded(ordinal);
      if (!isCurrent() || !targetHolds()) return endScrollOwnership(reqId);
      // Loading needs one frame for Svelte and another for resize observation.
      await raf();
      if (!isCurrent()) return endScrollOwnership(reqId);
      await raf();
      if (!isCurrent() || !targetHolds()) return endScrollOwnership(reqId);
    }

    // A message inside a collapsed empty row has no card until the row opens.
    const emptyGroup = displayItemsAsc.find(
      (item) => item.kind === "empty-group" && item.ordinals.includes(ordinal),
    );
    if (emptyGroup && !emptyGroupOpen(emptyGroup)) {
      setEmptyGroupOpen(emptyGroup.ordinals, true);
      // Let the opened row render and be measured before the jump scrolls
      // into it. A row that grows above the scroll position makes the
      // virtualizer shift the scroll by the growth, past the target card.
      await tick();
      await raf();
      if (!isCurrent()) return endScrollOwnership(reqId);
      await raf();
      if (!isCurrent() || !targetHolds()) return endScrollOwnership(reqId);
    }

    const settled = await scrollToDisplayIndex(() => displayIndexOf(ordinal), 0, 0, reqId, "start", targetHolds);
    if (!isCurrent() || !targetHolds() || !settled) return endScrollOwnership(reqId);
    ordinalJump = { reqId, ordinal, sessionId, targetHolds, until: performance.now() + 5000, sawLoadingOlder: false };
  }

  export function scrollToOrdinal(ordinal: number, call?: ScrollCall) {
    void scrollToOrdinalInternal(ordinal, call);
  }

  function scrollToLatestInternal() {
    const reqId = endScrollOwnership();
    activeFollowScrollRequest = reqId;
    const idx = getLatestDisplayIndex(
      displayItemsAsc.length,
      ui.sortNewestFirst,
    );
    if (idx < 0) return;
    scrollToDisplayIndex(
      () => idx,
      0,
      0,
      reqId,
      ui.sortNewestFirst ? "start" : "end",
    );
    startFollowLatestSettle(reqId);
  }

  function forceLatestEdge() {
    if (!containerRef) return;
    containerRef.scrollTop = ui.sortNewestFirst
      ? 0
      : containerRef.scrollHeight;
  }

  function startFollowLatestSettle(reqId: number) {
    if (followSettleTimer !== null) {
      clearTimeout(followSettleTimer);
      followSettleTimer = null;
    }

    const tick = () => {
      followSettleTimer = null;
      if (
        reqId !== lastScrollRequest ||
        !ui.followLatest ||
        !containerRef
      ) {
        return;
      }

      forceLatestEdge();
      followSettleTimer = setTimeout(tick, 100);
    };

    tick();
  }

  function queueFollowLatestScroll() {
    if (!ui.followLatest) return;
    if (followingScrollRaf !== null) {
      cancelAnimationFrame(followingScrollRaf);
    }
    followingScrollRaf = requestAnimationFrame(() => {
      followingScrollRaf = null;
      if (!ui.followLatest) return;
      scrollToLatestInternal();
    });
  }

  function latestDisplaySignature(): string {
    const item = displayItemsAsc[displayItemsAsc.length - 1];
    if (!item) return "";
    if (item.kind !== "message") {
      return item.messages
        .map((m) => `${m.ordinal}:${m.content_length}:${m.timestamp}`)
        .join("|");
    }
    const m = item.message;
    return `${m.ordinal}:${m.content_length}:${m.timestamp}`;
  }

  function itemOrdinals(item: DisplayItem): number[] {
    if (item.kind === "message") return [item.message.ordinal];
    const source = ui.sortNewestFirst
      ? [...item.messages].reverse()
      : item.messages;
    return source.map((message) => message.ordinal);
  }

  $effect(() => {
    const follow = ui.followLatest;
    if (!follow) {
      cancelFollowLatestWork();
    }
  });

  $effect(() => {
    const follow = ui.followLatest;
    const request = ui.followLatestRequest;
    const count = displayItemsAsc.length;
    const latest = latestDisplaySignature();
    const newestFirst = ui.sortNewestFirst;
    const sessionId = messages.sessionId;
    if (!follow || count === 0 || !sessionId) return;
    void request;
    void latest;
    void newestFirst;
    queueFollowLatestScroll();
  });

  export function scrollToLatest() {
    scrollToLatestInternal();
  }

  export function getDisplayItems(): DisplayItem[] {
    return displayItemsAsc;
  }

  export function getNormalDisplayItems(): DisplayItem[] {
    return normalDisplayItemsAsc;
  }

  let searchRevealKey = $derived.by(() => {
    const match = inSessionSearch.resolvedCurrent;
    return match ? `${match.ordinal}:${match.blockKey}:${match.occurrence}` : "";
  });

  async function revealSearchMatch(match: Match, sessionId: string, reqId: number): Promise<boolean> {
    return revealMatch({
      ordinal: match.ordinal,
      blockKey: match.blockKey,
      getContainer: () => containerRef,
      isCurrent: () => requestIsCurrent(reqId, sessionId) && inSessionSearch.isActive,
      ensureLoaded: (ordinal) => messages.ensureOrdinalLoaded(ordinal),
      mountMessage: () => scrollToDisplayIndex(() => displayIndexOf(match.ordinal), 0, 0, reqId),
      scrollToOffset: (offset) => virtualizer.instance?.scrollToOffset(
        Math.round(offset), { align: "start" },
      ),
      afterUpdate: tick,
      nextFrame: raf,
    });
  }

  $effect(() => {
    const request = inSessionSearch.navigationRevision;
    const key = searchRevealKey;
    const sessionId = messages.sessionId;
    const count = displayItemsAsc.length;
    const newestFirst = ui.sortNewestFirst;
    if (!inSessionSearch.isActive || !key || !sessionId || !containerRef) return;
    void request;
    void count;
    void newestFirst;
    return untrack(() => {
      const match = inSessionSearch.resolvedCurrent;
      if (!match) return;
      const reqId = endScrollOwnership();
      activeFollowScrollRequest = null;
      ui.selectOrdinal(match.ordinal);
      ui.setFollowLatest(false);
      void revealSearchMatch(match, sessionId, reqId).then((revealed) => {
        if (!revealed) endScrollOwnership(reqId);
      }).catch((error: unknown) => {
        if (reqId === lastScrollRequest) console.warn("Could not reveal search occurrence", error);
        endScrollOwnership(reqId);
      });
      return () => {
        endScrollOwnership(reqId);
      };
    });
  });

  let effectiveLayout = $derived(
    resolveMessageLayout(ui.messageLayout, inSessionSearch.isActive),
  );

  let readProgressDivider = $derived.by(() => {
    const sessionId = messages.sessionId;
    const currentToken = messages.activeSessionToken;
    const marker = sessionId
      ? readProgress.get(sessionId)
      : null;
    const latestOrdinal = latestLoadedOrdinal;
    if (
      !sessionId ||
      !currentToken ||
      !marker ||
      marker.token === currentToken ||
      latestOrdinal === null
    ) {
      return null;
    }

    const unreadBoundary = unreadBoundaryOrdinal(latestOrdinal);

    const items = ui.sortNewestFirst
      ? [...displayItemsAsc].reverse()
      : displayItemsAsc;

    if (ui.sortNewestFirst) {
      if (messages.activeSessionUnreadOrdinal === null) {
        return null;
      }
      const dividerBoundary = marker.ordinal !== null &&
          unreadBoundary === marker.ordinal + 1
        ? marker.ordinal
        : unreadBoundary;
      for (const item of items) {
        for (const ordinal of itemOrdinals(item)) {
          if (ordinal <= dividerBoundary) {
            return {
              ordinal,
              label: m.read_progress_earlier_messages(),
            };
          }
        }
      }
      return null;
    }

    for (const item of items) {
      for (const ordinal of itemOrdinals(item)) {
        if (ordinal >= unreadBoundary) {
          return {
            ordinal,
            label: m.read_progress_new_messages(),
          };
        }
      }
    }
    return null;
  });
</script>

{#if !sessions.activeSessionId}
  <EmptyState title={m.message_list_empty()}>
    {#snippet icon()}
      <MessageSquareIcon size="36" strokeWidth="1.5" aria-hidden="true" />
    {/snippet}
  </EmptyState>
{:else if messages.loading && messages.messages.length === 0}
  <EmptyState title={m.message_list_loading()} />
{:else if sessions.activeSessionNotFound && messages.messages.length === 0}
  <div class="missing-session" {@attach errorTelemetry("session")}>
    <EmptyState
      title={m.message_list_session_not_found()}
      description={m.message_list_session_not_found_hint()}
    >
      {#snippet icon()}
        <CircleQuestionMarkIcon size="36" strokeWidth="1.5" aria-hidden="true" />
      {/snippet}
      <Button size="sm" onclick={() => void sessions.retryActiveSession()}>
        {m.message_list_session_not_found_retry()}
      </Button>
    </EmptyState>
  </div>
{:else}
  <SessionFindView
    items={displayItemsAsc}
    totalSize={virtualizer.instance?.getTotalSize() ?? 0}
    newestFirst={ui.sortNewestFirst}
    rowOffset={(index) => virtualizer.instance?.getOffsetForIndex(index, "start")?.[0] ?? index * 120}
  >
  <div
    class="message-list-scroll layout-{effectiveLayout}"
    bind:this={containerRef}
    data-session-id={sessions.activeSessionId}
    data-messages-session-id={messages.sessionId}
    data-loaded={!messages.loading}
    onscroll={handleScroll}
    use:manualScrollIntent
  >
    <div
      style="height: {virtualizer.instance?.getTotalSize() ?? 0}px; width: 100%; position: relative;"
    >
      {#each virtualizer.instance?.getVirtualItems() ?? [] as row (row.key)}
        {@const item = itemAt(row.index)}
        {#if item}
          <!-- svelte-ignore a11y_click_events_have_key_events -->
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            class="virtual-row"
            class:selected={ui.selectedOrdinal !== null &&
              item.ordinals.includes(ui.selectedOrdinal)}
            data-index={row.index}
            style="position: absolute; top: 0; left: 0; width: 100%; transform: translateY({row.start}px);"
            use:measureElement={virtualizer.instance}
            onclick={(event) => {
              const sel = window.getSelection();
              if (sel && sel.toString().length > 0) return;
              ui.selectOrdinal(clickedEmptyMemberOrdinal(item, event.target) ?? item.ordinals[0]!);
            }}
          >
            {#if item.kind !== "tool-group" && readProgressDivider !== null && item.ordinals.includes(readProgressDivider.ordinal)}
              <div class="read-progress-divider" role="separator" aria-label={m.read_progress_boundary()}>
                {readProgressDivider.label}
              </div>
            {/if}
            {#if item.kind === "tool-group"}
              <ToolCallGroup
                messages={item.messages}
                timestamp={item.timestamp}
                searchable={true}
                sortNewestFirst={ui.sortNewestFirst}
                divider={readProgressDivider !== null && item.ordinals.includes(readProgressDivider.ordinal)
                  ? readProgressDivider
                  : undefined}
              />
            {:else if item.kind === "empty-group"}
              <EmptyMessagesRow
                messages={item.messages}
                open={emptyGroupOpen(item)}
                onToggle={(open) => setEmptyGroupOpen(item.ordinals, open)}
              />
            {:else if item.message.is_compact_boundary}
              <CompactBoundaryDivider message={item.message} />
            {:else if isSystemBoundaryMessage(item.message)}
              <SystemBoundaryCard
                subtype={item.message.source_subtype}
                content={item.message.content}
                timestamp={item.message.timestamp}
              />
            {:else}
              <MessageContent
                message={item.message}
                searchOrdinal={item.message.ordinal}
              />
            {/if}
          </div>
        {/if}
      {/each}
    </div>
  </div>
  </SessionFindView>
{/if}

<style>
  .missing-session {
    display: contents;
  }

  .message-list-scroll {
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
    padding: 8px 0;
    overflow-anchor: none;
  }

  .virtual-row {
    padding: 5px 12px;
    overflow-anchor: none;
  }

  .virtual-row.selected > :global(*) {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
    border-radius: var(--radius-md, 6px);
  }

  .read-progress-divider {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
    color: var(--accent-blue);
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }

  .read-progress-divider::before,
  .read-progress-divider::after {
    content: "";
    height: 1px;
    flex: 1;
    background: color-mix(
      in srgb, var(--accent-blue) 35%, transparent
    );
  }

  /* ── Compact layout ── */
  .layout-compact {
    padding: 4px 0;
  }

  .layout-compact .virtual-row {
    padding: 2px 12px;
  }

  .layout-compact :global(.message) {
    padding: 6px 12px;
    border-radius: 0;
  }

  /* Pattern mode keeps the full bar width, which the line patterns need. */
  :global(:root:not([data-message-role-patterns])) .layout-compact :global(.message) {
    border-left-width: 2px;
    border-radius: 0;
  }

  .layout-compact :global(.message-header) {
    margin-bottom: 4px;
    gap: 6px;
  }

  .layout-compact :global(.role-icon) {
    width: 16px;
    height: 16px;
    font-size: 9px;
  }

  .layout-compact :global(.role-label) {
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-weight: 700;
  }

  .layout-compact :global(.timestamp),
  .layout-compact :global(.group-timestamp) {
    font-size: 10px;
  }

  .layout-compact :global(.text-content) {
    font-size: 13px;
    line-height: 1.55;
  }

  .layout-compact :global(.message-body) {
    gap: 4px;
  }

  /* ── Stream layout ── */
  .layout-stream {
    padding: 0;
  }

  .layout-stream .virtual-row {
    padding: 0;
  }

  .layout-stream :global(.message) {
    border-radius: 0;
    padding: 16px 24px;
  }

  /* Stream hides the role header, so pattern mode keeps the patterned bar
     as the role marker. */
  :global(:root:not([data-message-role-patterns])) .layout-stream :global(.message) {
    border-left: none;
  }

  .layout-stream :global(.message.is-user) {
    background: color-mix(
      in srgb,
      var(--role-user-accent) 5%,
      transparent
    ) !important;
  }

  .layout-stream :global(.message:not(.is-user)) {
    background: transparent !important;
  }

  .layout-stream :global(.message-header) {
    display: none;
  }

  .layout-stream :global(.text-content) {
    font-size: 14px;
    line-height: 1.75;
  }

  /* ── Skim layout ── */
  .layout-skim {
    padding: 0;
  }

  .layout-skim .virtual-row {
    padding: 0;
  }

  .layout-skim :global(.message-header) {
    display: none;
  }

  .layout-skim :global(.tool-block) {
    border-left: none;
    border-radius: 0;
  }

  .layout-skim :global(.tool-chevron) {
    display: none;
  }

  /* Keep the one-line summary, but make the header non-interactive so a
     click cannot silently toggle hidden collapse state (which would
     surprise the user on switching back to a full layout). Row selection
     still works via the virtual-row wrapper behind the header. */
  .layout-skim :global(.tool-header) {
    padding: 1px 12px;
    pointer-events: none;
  }

  .layout-skim :global(.tool-meta),
  .layout-skim :global(.tool-content),
  .layout-skim :global(.diff-view),
  .layout-skim :global(.output-header),
  .layout-skim :global(.history-header),
  .layout-skim :global(.result-history),
  .layout-skim :global(.show-more-btn) {
    display: none;
  }

  .layout-skim :global(.tool-group-header),
  .layout-skim :global(.pg-header) {
    display: none;
  }

  .layout-skim :global(.tool-group),
  .layout-skim :global(.parallel-group) {
    border: none;
    margin: 0;
    padding: 0;
    background: transparent;
  }

  .layout-skim :global(.subagent-inline) {
    display: none;
  }
</style>
