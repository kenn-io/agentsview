<script lang="ts">
  import { onMount, untrack } from "svelte";
  import AppHeader from "./lib/components/layout/AppHeader.svelte";
  import ThreeColumnLayout from "./lib/components/layout/ThreeColumnLayout.svelte";
  import SessionBreadcrumb from "./lib/components/layout/SessionBreadcrumb.svelte";
  import StatusBar from "./lib/components/layout/StatusBar.svelte";
  import SessionList from "./lib/components/sidebar/SessionList.svelte";
  import MessageList from "./lib/components/content/MessageList.svelte";
  import CommandPalette from "./lib/components/command-palette/CommandPalette.svelte";
  import ShortcutsModal from "./lib/components/modals/ShortcutsModal.svelte";
  import PublishModal from "./lib/components/modals/PublishModal.svelte";
  import ResyncModal from "./lib/components/modals/ResyncModal.svelte";
  import AnalyticsPage from "./lib/components/analytics/AnalyticsPage.svelte";
  import InsightsPage from "./lib/components/insights/InsightsPage.svelte";
  import PinnedPage from "./lib/components/pinned/PinnedPage.svelte";
  import TrashPage from "./lib/components/trash/TrashPage.svelte";
  import { sessions } from "./lib/stores/sessions.svelte.js";
  import { messages } from "./lib/stores/messages.svelte.js";
  import { sync } from "./lib/stores/sync.svelte.js";
  import { ui } from "./lib/stores/ui.svelte.js";
  import { router } from "./lib/stores/router.svelte.js";
  import { starred } from "./lib/stores/starred.svelte.js";
  import { pins } from "./lib/stores/pins.svelte.js";
  import { registerShortcuts } from "./lib/utils/keyboard.js";
  import type { DisplayItem } from "./lib/utils/display-items.js";
  import {
    parseContent,
    enrichSegments,
  } from "./lib/utils/content-parser.js";

  let messageListRef:
    | {
        scrollToOrdinal: (o: number) => void;
        getDisplayItems: () => DisplayItem[];
      }
    | undefined = $state(undefined);

  // Load active session's messages when selection changes.
  // Only track activeSessionId — untrack the rest to prevent
  // reactive loops from messages.loading / messages.messages.
  $effect(() => {
    const id = sessions.activeSessionId;
    untrack(() => {
      // Preserve selection when a pending scroll is queued
      // for this specific session (e.g. search result
      // navigation sets session + ordinal before this effect
      // fires). Clear if the pending scroll targets a
      // different session or there is no pending scroll.
      const pendingMatchesSession =
        ui.pendingScrollOrdinal !== null &&
        (ui.pendingScrollSession === null ||
          ui.pendingScrollSession === id);
      if (!pendingMatchesSession) {
        ui.clearSelection();
        ui.pendingScrollOrdinal = null;
        ui.pendingScrollSession = null;
      }
      if (id) {
        messages.loadSession(id);
        sync.watchSession(id, () => messages.reload());
        pins.loadForSession(id);
      } else {
        messages.clear();
        sync.unwatchSession();
        pins.clearSession();
      }
    });
  });

  // Scroll to pending ordinal once messages finish loading.
  // If the target message is hidden specifically because thinking
  // is disabled, auto-enable thinking so the message becomes visible.
  // Messages hidden by other block filters (tool/code/user/assistant)
  // are left alone — auto-changing unrelated filters is unexpected.
  $effect(() => {
    const ordinal = ui.pendingScrollOrdinal;
    const loading = messages.loading;
    const thinkingVisible = ui.isBlockVisible("thinking");
    untrack(() => {
      if (ordinal === null || loading || !messageListRef) return;

      const items = messageListRef.getDisplayItems();
      const found = items.some((item) =>
        item.ordinals.includes(ordinal),
      );

      if (!found) {
        // Only auto-enable thinking if the ordinal is loaded
        // but filtered out *specifically* due to hidden thinking.
        // If it's outside the loaded window, don't change filters.
        // Auto-enable thinking filter when navigating to a message
        // that contains a thinking block.
        const msg = messages.messages.find(
          (m) => m.ordinal === ordinal,
        );
        if (msg && !thinkingVisible) {
          const segs = enrichSegments(
            parseContent(msg.content, msg.has_tool_use),
            msg.tool_calls,
          );
          const hasThinkingSegment = segs.some(
            (s) => s.type === "thinking",
          );
          if (hasThinkingSegment) {
            ui.setBlockVisible("thinking", true);
            return; // effect re-runs with thinking visible
          }
        }
      }

      messageListRef.scrollToOrdinal(ordinal);
      // Ensure highlight is set (the session-change effect
      // may have cleared it before this effect ran).
      ui.selectedOrdinal = ordinal;
      ui.pendingScrollOrdinal = null;
      ui.pendingScrollSession = null;
    });
  });

  function navigateMessage(delta: number) {
    const items = messageListRef?.getDisplayItems();
    if (!items || items.length === 0) return;

    const sorted = ui.sortNewestFirst
      ? [...items].reverse()
      : items;

    const selected = ui.selectedOrdinal;
    if (selected === null) {
      const first = sorted[0]!;
      ui.selectOrdinal(first.ordinals[0]!);
      messageListRef?.scrollToOrdinal(first.ordinals[0]!);
      return;
    }

    const curIdx = sorted.findIndex((item) =>
      item.ordinals.includes(selected),
    );
    const nextIdx = Math.max(
      0,
      Math.min(sorted.length - 1, curIdx + delta),
    );
    if (nextIdx === curIdx) return;

    const next = sorted[nextIdx]!;
    ui.selectOrdinal(next.ordinals[0]!);
    messageListRef?.scrollToOrdinal(next.ordinals[0]!);
  }

  // React to route changes: initialize session filters from URL params
  $effect(() => {
    const _route = router.route;
    const params = router.params;
    untrack(() => {
      sessions.initFromParams(params);
      sessions.load();
      sessions.loadProjects();
      sessions.loadAgents();
    });
  });

  onMount(() => {
    starred.load();
    sync.loadStatus();
    sync.loadStats();
    sync.loadVersion();
    sync.startPolling();

    const cleanup = registerShortcuts({ navigateMessage });
    return () => {
      cleanup();
      sync.stopPolling();
      sync.unwatchSession();
    };
  });

</script>

<AppHeader />

{#if router.route === "insights"}
  <div class="page-scroll">
    <InsightsPage />
  </div>
{:else if router.route === "pinned"}
  <div class="page-scroll">
    <PinnedPage />
  </div>
{:else if router.route === "trash"}
  <div class="page-scroll">
    <TrashPage />
  </div>
{:else}
  <ThreeColumnLayout>
    {#snippet sidebar()}
      <SessionList />
    {/snippet}

    {#snippet content()}
      {#if sessions.activeSessionId}
        {@const session = sessions.activeSession}
        <SessionBreadcrumb
          session={session}
          onBack={() => sessions.deselectSession()}
        />
        <MessageList bind:this={messageListRef} />
      {:else}
        <AnalyticsPage />
      {/if}
    {/snippet}
  </ThreeColumnLayout>
{/if}

<StatusBar />

{#if ui.activeModal === "commandPalette"}
  <CommandPalette />
{/if}

{#if ui.activeModal === "shortcuts"}
  <ShortcutsModal />
{/if}

{#if ui.activeModal === "publish"}
  <PublishModal />
{/if}

{#if ui.activeModal === "resync"}
  <ResyncModal />
{/if}

{#if sessions.recentlyDeleted.length > 0}
  <div class="undo-toast">
    <span>Session deleted</span>
    <button
      class="undo-btn"
      onclick={async (e) => {
        const btn = e.currentTarget;
        if (btn.disabled) return;
        const last = sessions.recentlyDeleted[sessions.recentlyDeleted.length - 1];
        if (!last) return;
        btn.disabled = true;
        try {
          await sessions.restoreSession(last.id);
        } catch {
          // restore failed — toast will remain
        } finally {
          btn.disabled = false;
        }
      }}
    >
      Undo
    </button>
  </div>
{/if}

<style>
  .page-scroll {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
  }

  .undo-toast {
    position: fixed;
    bottom: 40px;
    left: 50%;
    transform: translateX(-50%);
    display: flex;
    align-items: center;
    gap: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: 8px;
    padding: 10px 18px;
    box-shadow: 0 6px 24px rgba(0, 0, 0, 0.3);
    z-index: 10000;
    font-size: 13px;
    color: var(--text-primary);
    animation: slide-up 0.2s ease-out;
  }

  @keyframes slide-up {
    from {
      opacity: 0;
      transform: translateX(-50%) translateY(10px);
    }
    to {
      opacity: 1;
      transform: translateX(-50%) translateY(0);
    }
  }

  .undo-btn {
    background: none;
    border: none;
    color: var(--accent-blue);
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    padding: 2px 6px;
    border-radius: 4px;
  }

  .undo-btn:hover {
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
  }
</style>
