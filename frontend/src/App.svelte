<script lang="ts">
  import { onMount, untrack } from "svelte";
  import AppHeader from "./lib/components/layout/AppHeader.svelte";
  import ThreeColumnLayout from "./lib/components/layout/ThreeColumnLayout.svelte";
  import SessionBreadcrumb from "./lib/components/layout/SessionBreadcrumb.svelte";
  import StatusBar from "./lib/components/layout/StatusBar.svelte";
  import SessionList from "./lib/components/sidebar/SessionList.svelte";
  import MessageList from "./lib/components/content/MessageList.svelte";
  import ActivityMinimap from "./lib/components/content/ActivityMinimap.svelte";
  import { sessionActivity } from "./lib/stores/sessionActivity.svelte.js";
  import CommandPalette from "./lib/components/command-palette/CommandPalette.svelte";
  import AboutModal from "./lib/components/modals/AboutModal.svelte";
  import ShortcutsModal from "./lib/components/modals/ShortcutsModal.svelte";
  import PublishModal from "./lib/components/modals/PublishModal.svelte";
  import ResyncModal from "./lib/components/modals/ResyncModal.svelte";
  import UpdateModal from "./lib/components/modals/UpdateModal.svelte";
  import ConfirmDeleteModal from "./lib/components/modals/ConfirmDeleteModal.svelte";
  import AnalyticsPage from "./lib/components/analytics/AnalyticsPage.svelte";
  import InsightsPage from "./lib/components/insights/InsightsPage.svelte";
  import PinnedPage from "./lib/components/pinned/PinnedPage.svelte";
  import TrashPage from "./lib/components/trash/TrashPage.svelte";
  import SettingsPage from "./lib/components/settings/SettingsPage.svelte";
  import { sessions } from "./lib/stores/sessions.svelte.js";
  import { messages } from "./lib/stores/messages.svelte.js";
  import { sync } from "./lib/stores/sync.svelte.js";
  import { ui } from "./lib/stores/ui.svelte.js";
  import { router } from "./lib/stores/router.svelte.js";
  import { starred } from "./lib/stores/starred.svelte.js";
  import { pins } from "./lib/stores/pins.svelte.js";
  import { settings } from "./lib/stores/settings.svelte.js";
  import { setAuthToken, getAuthToken, setServerUrl, getBase } from "./lib/api/client.js";
  import { setupVisibilityHealthCheck } from "./lib/utils/health.js";
  import { registerShortcuts } from "./lib/utils/keyboard.js";
  import { shouldAutoSwitchTranscriptModeToNormal } from "./lib/utils/transcript-mode.js";

  let globalAuthToken: string = $state("");

  function handleGlobalAuth() {
    const token = globalAuthToken.trim();
    if (!token) return;
    setAuthToken(token);
    // Full reload ensures all stores (settings, sessions, starred,
    // sync, pins, etc.) reinitialize with the new credentials.
    window.location.reload();
  }
  import type { DisplayItem } from "./lib/utils/display-items.js";
  import {
    parseContent,
    enrichSegments,
  } from "./lib/utils/content-parser.js";

  let messageListRef:
    | {
        scrollToOrdinal: (o: number) => void;
        getDisplayItems: () => DisplayItem[];
        getNormalDisplayItems: () => DisplayItem[];
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
        if (ui.isMobileViewport) {
          ui.closeSidebar();
        }
        messages.loadSession(id);
        sessions.loadChildSessions(id);
        sync.watchSession(id, () => {
          messages.reload();
          sessions.refreshActiveSession();
          sessions.loadChildSessions(id);
          if (ui.activityMinimapOpen) {
            sessionActivity.reload(id);
          } else {
            sessionActivity.invalidate();
          }
        });
        pins.loadForSession(id);
      } else {
        sessionActivity.clear();
        messages.clear();
        sessions.childSessions = new Map();
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
      const normalItems =
        messageListRef.getNormalDisplayItems();
      const found = items.some((item) =>
        item.ordinals.includes(ordinal),
      );

      if (!found) {
        if (
          shouldAutoSwitchTranscriptModeToNormal(
            ui.transcriptMode,
            ordinal,
            items,
            normalItems,
          )
        ) {
          ui.setTranscriptMode("normal");
          return; // effect re-runs with normal transcript mode
        }

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
            parseContent(
              msg.content,
              msg.has_tool_use,
              msg.id,
              msg.content_length,
            ),
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

  // React to route changes: initialize session filters from URL params.
  // Only track route and params — NOT sessionId. When the URL sync
  // effect deselects a session (changing sessionId), we must not
  // re-run initFromParams or it will reset filters the user just set.
  $effect(() => {
    const _route = router.route;
    const params = router.params;
    untrack(() => {
      const sid = router.sessionId;
      if (!sid) {
        sessions.initFromParams(params);
      }
      sessions.load();
      sessions.loadProjects();
      sessions.loadAgents();
    });
  });

  // Deep-link: select session from URL and handle ?msg param.
  $effect(() => {
    const sid = router.sessionId;
    const msgParam = router.params["msg"] ?? null;
    untrack(() => {
      if (sid) {
        if (sid !== sessions.activeSessionId) {
          sessions.navigateToSession(sid);
        }
        if (msgParam) {
          if (msgParam === "last") {
            ui.pendingScrollOrdinal = -1;
            ui.pendingScrollSession = sid;
          } else {
            const ordinal = parseInt(msgParam, 10);
            if (Number.isFinite(ordinal)) {
              ui.scrollToOrdinal(ordinal, sid);
            }
          }
        }
      } else if (router.route === "sessions") {
        if (sessions.activeSessionId !== null) {
          sessions.deselectSession();
        }
      }
    });
  });

  // Resolve msg=last once messages are loaded.
  $effect(() => {
    const pending = ui.pendingScrollOrdinal;
    const loading = messages.loading;
    const msgs = messages.messages;
    untrack(() => {
      if (pending !== -1 || loading || msgs.length === 0) return;
      const target = ui.pendingScrollSession;
      if (target !== null && target !== messages.sessionId) return;
      const lastOrdinal = msgs[msgs.length - 1]!.ordinal;
      ui.scrollToOrdinal(lastOrdinal, target ?? undefined);
    });
  });

  // Build URL params from current session filters.
  function buildFilterParams(): Record<string, string> {
    const f = sessions.filters;
    const p: Record<string, string> = {};
    if (f.project) p.project = f.project;
    if (f.machine) p.machine = f.machine;
    if (f.agent) p.agent = f.agent;
    if (f.date) p.date = f.date;
    if (f.dateFrom) p.date_from = f.dateFrom;
    if (f.dateTo) p.date_to = f.dateTo;
    if (f.recentlyActive) p.active_since = "true";
    if (f.hideUnknownProject) p.exclude_project = "unknown";
    if (f.minMessages > 0) p.min_messages = String(f.minMessages);
    if (f.maxMessages > 0) p.max_messages = String(f.maxMessages);
    if (f.minUserMessages > 0) p.min_user_messages = String(f.minUserMessages);
    if (!f.includeOneShot) p.include_one_shot = "false";
    if (f.includeAutomated) p.include_automated = "true";
    return p;
  }

  // Sync active session to URL.
  $effect(() => {
    const activeId = sessions.activeSessionId;
    const currentUrlSessionId = router.sessionId;
    untrack(() => {
      if (router.route !== "sessions") return;
      if (activeId === currentUrlSessionId) return;
      if (activeId) {
        router.navigateToSession(activeId);
      } else {
        router.navigateFromSession(buildFilterParams());
      }
    });
  });

  function showAbout() {
    if (ui.activeModal === "resync" && sync.syncing) return;
    ui.activeModal = "about";
  }

  onMount(() => {
    globalAuthToken = getAuthToken();
    settings.load();
    starred.load();
    sync.loadStatus();
    sync.loadStats();
    sync.loadVersion();
    sync.checkForUpdate();
    sync.startPolling();

    const healthCleanup = setupVisibilityHealthCheck(getBase);

    window.addEventListener("show-about", showAbout);
    const cleanup = registerShortcuts({ navigateMessage });
    return () => {
      healthCleanup();
      cleanup();
      window.removeEventListener("show-about", showAbout);
      sync.stopPolling();
      sync.unwatchSession();
    };
  });

</script>

{#if settings.needsAuth && router.route !== "settings"}
  <div class="auth-overlay">
    <div class="auth-card">
      <h2 class="auth-card-title">Authentication Required</h2>
      <p class="auth-card-desc">
        This server requires an auth token to access. Enter the token
        shown on the server's console or settings page.
      </p>
      <div class="auth-card-field">
        <input
          class="auth-card-input"
          type="password"
          placeholder="Paste auth token"
          bind:value={globalAuthToken}
          onkeydown={(e) => { if (e.key === "Enter") handleGlobalAuth(); }}
        />
        <button
          class="auth-card-btn"
          disabled={!globalAuthToken.trim()}
          onclick={handleGlobalAuth}
        >
          Authenticate
        </button>
      </div>
      <button
        class="auth-card-disconnect"
        onclick={() => {
          setAuthToken("");
          setServerUrl("");
          settings.needsAuth = false;
          settings.load();
        }}
      >
        Disconnect and reset
      </button>
    </div>
  </div>
{:else}

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
{:else if router.route === "settings"}
  <div class="page-scroll">
    <SettingsPage />
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
        {#if ui.activityMinimapOpen && sessions.activeSessionId}
          <ActivityMinimap
            sessionId={sessions.activeSessionId}
          />
        {/if}
        <MessageList bind:this={messageListRef} />
      {:else}
        <AnalyticsPage />
      {/if}
    {/snippet}
  </ThreeColumnLayout>
{/if}

<StatusBar />

{#if ui.activeModal === "about"}
  <AboutModal />
{/if}

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

{#if ui.activeModal === "update"}
  <UpdateModal />
{/if}

{#if ui.activeModal === "confirmDelete"}
  <ConfirmDeleteModal />
{/if}

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

  .auth-overlay {
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100vh;
    background: var(--bg-default);
  }

  .auth-card {
    text-align: center;
    max-width: 420px;
    padding: 32px 24px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: 12px;
    box-shadow: var(--shadow-lg);
  }

  .auth-card-title {
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0 0 8px;
  }

  .auth-card-desc {
    font-size: 13px;
    color: var(--text-muted);
    margin: 0 0 20px;
  }

  .auth-card-field {
    display: flex;
    gap: 8px;
  }

  .auth-card-input {
    flex: 1;
    height: 34px;
    padding: 0 12px;
    border-radius: 6px;
    font-size: 13px;
    font-family: var(--font-mono, monospace);
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
  }

  .auth-card-input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .auth-card-btn {
    height: 34px;
    padding: 0 16px;
    border-radius: 6px;
    font-size: 13px;
    font-weight: 500;
    color: white;
    background: var(--accent-blue);
    border: none;
    cursor: pointer;
    white-space: nowrap;
  }

  .auth-card-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }

  .auth-card-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .auth-card-disconnect {
    margin-top: 12px;
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 12px;
    cursor: pointer;
    text-decoration: underline;
  }

  .auth-card-disconnect:hover {
    color: var(--text-secondary);
  }
</style>
