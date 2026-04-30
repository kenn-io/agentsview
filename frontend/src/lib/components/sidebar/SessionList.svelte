<script lang="ts">
  import { onDestroy } from "svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { starred } from "../../stores/starred.svelte.js";
  import SessionItem from "./SessionItem.svelte";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import { formatNumber } from "../../utils/format.js";
  import { agentColor } from "../../utils/agents.js";
  import {
    type GroupMode,
    ITEM_HEIGHT,
    OVERSCAN,
    STORAGE_KEY_GROUP,
    getInitialGroupMode,
    buildGroupSections,
    buildDisplayItems,
    computeTotalSize,
    findStart,
    isSubagentDescendant,
    selectPrimaryId,
  } from "./session-list-utils.js";

  let containerRef: HTMLDivElement | undefined = $state(undefined);
  let scrollTop = $state(0);
  let viewportHeight = $state(0);
  let scrollRaf: number | null = $state(null);

  let groupMode: GroupMode = $state(getInitialGroupMode());
  let manualExpanded: Set<string> = $state(new Set());
  // Start all collapsed when grouping is first enabled.
  let collapseAll = $state(getInitialGroupMode() !== "none");
  // Track which continuation chains are expanded.
  let expandedGroups: Set<string> = $state(new Set());

  $effect(() => {
    if (typeof localStorage !== "undefined") {
      localStorage.setItem(STORAGE_KEY_GROUP, groupMode);
    }
  });

  let groups = $derived.by(() => {
    const all = sessions.groupedSessions;
    if (!starred.filterOnly) return all;
    return all
      .map((g) => {
        const filtered = g.sessions.filter((s) =>
          starred.isStarred(s.id),
        );
        // Recompute primarySessionId so it points to a
        // session that survived the filter, using the same
        // recency rule as buildSessionGroups.
        const primaryStillPresent = filtered.some(
          (s) => s.id === g.primarySessionId,
        );
        return {
          ...g,
          sessions: filtered,
          // Preserve full session list so ancestry helpers
          // can still walk the parent chain correctly.
          allSessions: g.sessions,
          primarySessionId: primaryStillPresent
            ? g.primarySessionId
            : selectPrimaryId(filtered, g.key),
        };
      })
      .filter((g) => g.sessions.length > 0);
  });

  // Build grouped structure when groupMode is not "none".
  let groupSections = $derived.by(() =>
    buildGroupSections(groups, groupMode),
  );

  // Derive effective collapsed set synchronously so the first
  // render is already collapsed (no flicker).
  let collapsed = $derived.by(() => {
    if (groupMode === "none") return new Set<string>();
    if (collapseAll) {
      return new Set(groupSections.map((s) => s.label));
    }
    // Invert: all labels minus the manually expanded ones.
    const all = new Set(groupSections.map((s) => s.label));
    for (const a of manualExpanded) all.delete(a);
    return all;
  });

  // Build flat display items for virtual scrolling.
  let displayItems = $derived.by(() =>
    buildDisplayItems(groups, groupSections, groupMode, collapsed, expandedGroups),
  );

  // When include_children is enabled the API total includes
  // child/subagent sessions.  The header should show the count of
  // root-level groups the user actually sees in the sidebar.
  let totalCount = $derived(
    starred.filterOnly
      ? groups.reduce((n, g) => n + g.sessions.length, 0)
      : groups.length,
  );
  let totalSize = $derived(computeTotalSize(displayItems));

  let visibleItems = $derived.by(() => {
    if (displayItems.length === 0) return [];
    const start = findStart(displayItems, scrollTop);
    const end = scrollTop + viewportHeight + OVERSCAN * ITEM_HEIGHT;
    const result: typeof displayItems = [];
    for (let i = start; i < displayItems.length; i++) {
      const item = displayItems[i]!;
      if (item.top > end) break;
      result.push(item);
    }
    return result;
  });

  function setGroupMode(mode: GroupMode) {
    groupMode = mode;
    collapseAll = mode !== "none";
    manualExpanded = new Set();
  }

  function toggleGroupByAgent() {
    setGroupMode(groupMode === "agent" ? "none" : "agent");
  }

  function toggleGroupByProject() {
    setGroupMode(groupMode === "project" ? "none" : "project");
  }

  function toggleGroup(label: string) {
    if (collapseAll) {
      collapseAll = false;
      manualExpanded = new Set([label]);
    } else {
      const next = new Set(manualExpanded);
      if (next.has(label)) {
        next.delete(label);
      } else {
        next.add(label);
      }
      manualExpanded = next;
    }
  }

  function toggleChainExpand(groupKey: string) {
    const next = new Set(expandedGroups);
    if (next.has(groupKey)) {
      next.delete(groupKey);
      // When collapsing a parent, also remove sub-group keys.
      if (!groupKey.includes(":")) {
        next.delete(`subagent:${groupKey}`);
        next.delete(`team:${groupKey}`);
      }
    } else {
      next.add(groupKey);
      // When expanding a parent, auto-expand sub-groups.
      if (!groupKey.includes(":")) {
        next.add(`subagent:${groupKey}`);
        next.add(`team:${groupKey}`);
      }
    }
    expandedGroups = next;
  }

  $effect(() => {
    if (!containerRef) return;
    viewportHeight = containerRef.clientHeight;
    const ro = new ResizeObserver(() => {
      if (!containerRef) return;
      viewportHeight = containerRef.clientHeight;
    });
    ro.observe(containerRef);
    return () => ro.disconnect();
  });

  // Clamp stale scrollTop when count shrinks.
  $effect(() => {
    if (!containerRef) return;
    const maxTop = Math.max(
      0,
      totalSize - containerRef.clientHeight,
    );
    if (scrollTop > maxTop) {
      scrollTop = maxTop;
      containerRef.scrollTop = maxTop;
    }
  });

  function handleScroll() {
    if (!containerRef) return;
    if (scrollRaf !== null) return;
    scrollRaf = requestAnimationFrame(() => {
      scrollRaf = null;
      if (!containerRef) return;
      scrollTop = containerRef.scrollTop;
    });
  }

  // Scroll to the active session when it changes (e.g. from
  // the command palette). Expands collapsed agent groups and
  // scrolls the item into view. Only fires on selection
  // changes, not on displayItems rebuilds, so collapsing a
  // group containing the active session stays collapsed.
  let prevRevealedId: string | null = null;
  $effect(() => {
    const activeId = sessions.activeSessionId;
    if (!activeId) {
      prevRevealedId = null;
      return;
    }
    if (activeId === prevRevealedId) return;
    if (!containerRef) return;
    // Read displayItems inside the effect so Svelte tracks
    // it — needed to re-run after a group expansion.
    const items = displayItems;
    // Try to find the exact child row first (when expanded).
    let item = items.find(
      (it) =>
        it.type === "session" &&
        it.isChild &&
        it.session?.id === activeId,
    );
    // Fall back to the parent row only if the active session
    // IS the primary (visible as the root row). If it's a
    // child hidden in a collapsed subgroup, fall through to
    // the auto-expand path below instead.
    if (!item) {
      item = items.find(
        (it) =>
          it.type === "session" &&
          !it.isChild &&
          it.group?.primarySessionId === activeId,
      );
    }
    if (!item) {
      // Session may be hidden in a collapsed group section.
      // Expand it — the effect will re-run when displayItems
      // updates, and prevRevealedId is still unset so the
      // second pass will proceed to scroll.
      if (groupMode !== "none") {
        for (const section of groupSections) {
          const owns = section.groups.some((g) =>
            g.sessions.some((s) => s.id === activeId),
          );
          if (owns && collapsed.has(section.label)) {
            toggleGroup(section.label);
            return;
          }
        }
      }
      // Session may be inside a collapsed continuation chain.
      // Auto-expand the parent group and relevant sub-groups.
      for (const g of groups) {
        const match = g.sessions.find((s) => s.id === activeId);
        if (!match) continue;
        if (match.id === g.primarySessionId) break; // already primary
        const next = new Set(expandedGroups);
        if (!next.has(g.key)) next.add(g.key);
        // Auto-expand the correct sub-group.
        next.add(`subagent:${g.key}`);
        next.add(`team:${g.key}`);
        expandedGroups = next;
        return;
      }
      return;
    }
    // Item found — mark as revealed so subsequent
    // displayItems rebuilds don't re-trigger.
    prevRevealedId = activeId;
    const itemBottom = item.top + item.height;
    const viewTop = containerRef.scrollTop;
    const viewBottom = viewTop + containerRef.clientHeight;
    if (item.top >= viewTop && itemBottom <= viewBottom) return;
    containerRef.scrollTop = Math.max(
      0,
      item.top - containerRef.clientHeight / 2 + item.height / 2,
    );
  });

  onDestroy(() => {
    if (scrollRaf !== null) {
      cancelAnimationFrame(scrollRaf);
      scrollRaf = null;
    }
  });
</script>

<div class="session-list-header">
  <span class="session-count">
    {formatNumber(totalCount)} sessions
  </span>
  <div class="header-actions">
    {#if sessions.loading}
      <span class="loading-indicator">loading</span>
    {/if}
    <SessionFilterControl
      {groupMode}
      onToggleGroupByAgent={toggleGroupByAgent}
      onToggleGroupByProject={toggleGroupByProject}
      onClearGroupMode={() => setGroupMode("none")}
      extraActive={sessions.filters.termination !== ""}
      onClearExtra={() => sessions.setTerminationFilter("")}
      extraSections={statusFilterSection}
    />
    {#snippet statusFilterSection()}
      <div class="filter-section">
        <div class="filter-section-label">Status</div>
        <div class="pill-buttons">
          <button
            class="pill-btn pill-btn--status-active"
            class:active={sessions.hasTerminationStatus("active")}
            onclick={() => sessions.toggleTerminationStatus("active")}
            title="Last activity within 10 minutes"
          >
            Active
          </button>
          <button
            class="pill-btn pill-btn--status-stale"
            class:active={sessions.hasTerminationStatus("stale")}
            onclick={() => sessions.toggleTerminationStatus("stale")}
            title="Flagged session, idle 10 minutes to 1 hour"
          >
            Stale
          </button>
          <button
            class="pill-btn pill-btn--status-unclean"
            class:active={sessions.hasTerminationStatus("unclean")}
            onclick={() => sessions.toggleTerminationStatus("unclean")}
            title="Terminated mid tool call (over 1 hour idle)"
          >
            Unclean
          </button>
        </div>
      </div>
    {/snippet}
  </div>
</div>

<div
  class="session-list-scroll"
  bind:this={containerRef}
  onscroll={handleScroll}
>
  <div
    style="height: {totalSize}px; width: 100%; position: relative;"
  >
    {#each visibleItems as item (item.id)}
      <div
        style="position: absolute; top: 0; left: 0; width: 100%; height: {item.height}px; transform: translateY({item.top}px);"
      >
        {#if item.type === "header"}
          <button
            class="group-header"
            onclick={() => toggleGroup(item.label)}
          >
            <svg
              class="chevron"
              class:expanded={!collapsed.has(item.label)}
              width="10"
              height="10"
              viewBox="0 0 16 16"
              fill="currentColor"
            >
              <path d="M6.22 3.22a.75.75 0 011.06 0l4.25 4.25a.75.75 0 010 1.06l-4.25 4.25a.75.75 0 01-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 010-1.06z"/>
            </svg>
            {#if groupMode === "agent"}
              <span
                class="group-dot"
                style:background={agentColor(item.label)}
              ></span>
            {:else}
              <svg
                class="project-icon"
                width="11"
                height="11"
                viewBox="0 0 16 16"
                fill="currentColor"
              >
                <path d="M1.75 1A1.75 1.75 0 000 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0016 13.25v-8.5A1.75 1.75 0 0014.25 3H7.5a.25.25 0 01-.2-.1l-.9-1.2c-.33-.44-.85-.7-1.4-.7z"/>
              </svg>
            {/if}
            <span class="group-name">{item.label}</span>
            <span class="group-count">{item.count}</span>
          </button>
        {:else if item.type === "subagent-group" && item.group}
          {@const subKey = `subagent:${item.group.key}`}
          {@const subExpanded = expandedGroups.has(subKey)}
          <button
            class="sub-group-header"
            style:padding-left="{8 + (item.depth ?? 1) * 16}px"
            onclick={() => toggleChainExpand(subKey)}
          >
            <svg class="sub-group-arrow" class:expanded={subExpanded} width="10" height="10" viewBox="0 0 16 16" fill="currentColor"><path d="M6.22 3.22a.75.75 0 011.06 0l4.25 4.25a.75.75 0 010 1.06l-4.25 4.25a.75.75 0 01-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 010-1.06z"/></svg>
            <svg class="sub-group-icon" width="10" height="10" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
              <path d="M10.56 7.01A3.5 3.5 0 108 0a3.5 3.5 0 002.56 7.01zM8 8.5c-2.7 0-5 1.7-5 4v.75c0 .41.34.75.75.75h8.5c.41 0 .75-.34.75-.75v-.75c0-2.3-2.3-4-5-4z"/>
            </svg>
            <span class="sub-group-label">Subagents</span>
            <span class="sub-group-count">({item.count})</span>
          </button>
        {:else if item.type === "team-group" && item.group}
          {@const teamKey = `team:${item.group.key}`}
          {@const teamExpanded = expandedGroups.has(teamKey)}
          <button
            class="sub-group-header"
            style:padding-left="{8 + (item.depth ?? 1) * 16}px"
            onclick={() => toggleChainExpand(teamKey)}
          >
            <svg class="sub-group-arrow" class:expanded={teamExpanded} width="10" height="10" viewBox="0 0 16 16" fill="currentColor"><path d="M6.22 3.22a.75.75 0 011.06 0l4.25 4.25a.75.75 0 010 1.06l-4.25 4.25a.75.75 0 01-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 010-1.06z"/></svg>
            <svg class="sub-group-icon" width="12" height="10" viewBox="0 0 20 16" fill="currentColor" aria-hidden="true">
              <path d="M7.56 7.01A3.5 3.5 0 105 0a3.5 3.5 0 002.56 7.01zM5 8.5c-2.7 0-5 1.7-5 4v.75c0 .41.34.75.75.75h8.5c.41 0 .75-.34.75-.75v-.75c0-2.3-2.3-4-5-4z"/>
              <path d="M17.56 7.01A3.5 3.5 0 1015 0a3.5 3.5 0 002.56 7.01zM15 8.5c-2.7 0-5 1.7-5 4v.75c0 .41.34.75.75.75h8.5c.41 0 .75-.34.75-.75v-.75c0-2.3-2.3-4-5-4z" opacity="0.6"/>
            </svg>
            <span class="sub-group-label">Team</span>
            <span class="sub-group-count">({item.count})</span>
          </button>
        {:else if item.isChild && item.session}
          <SessionItem
            session={item.session}
            continuationCount={1}
            hideAgent={groupMode === "agent"}
            hideProject={groupMode === "project"}
            compact
            depth={item.depth ?? 1}
            isLastChild={item.isLastChild ?? false}
          />
        {:else if item.group}
          {@const primary = item.group.sessions.find(
            (s) => s.id === item.group!.primarySessionId,
          ) ?? item.group.sessions[0]}
          {@const children = item.group.sessions.filter((s) => s.id !== item.group!.primarySessionId)}
          {@const groupHasSubagents = children.some((s) => isSubagentDescendant(s, item.group!.sessions))}
          {@const groupHasTeammates = children.some((s) => s.first_message?.includes("<teammate-message") ?? false)}
          {#if primary}
            <SessionItem
              session={primary}
              continuationCount={item.group.sessions.length}
              groupSessionIds={item.group.sessions.length > 1
                ? item.group.sessions.map((s) => s.id)
                : undefined}
              groupSessions={item.group.sessions.length > 1
                ? item.group.sessions
                : undefined}
              hideAgent={groupMode === "agent"}
              hideProject={groupMode === "project"}
              expanded={expandedGroups.has(item.group.key)}
              onToggleExpand={item.group.sessions.length > 1
                ? () => toggleChainExpand(item.group!.key)
                : undefined}
              depth={0}
              hasSubagents={groupHasSubagents}
              hasTeammates={groupHasTeammates}
            />
          {/if}
        {/if}
      </div>
    {/each}
  </div>
</div>

<style>
  .session-list-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 14px;
    font-size: 10px;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    letter-spacing: 0.02em;
    text-transform: uppercase;
  }

  .session-count {
    font-weight: 600;
  }

  .header-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    position: relative;
  }

  .loading-indicator {
    color: var(--accent-green);
  }

  /* Snippet rendered inside SessionFilterControl carries this
     component's CSS scope, so it doesn't see SessionFilterControl's
     own .filter-section / .pill-* base styles. Re-declare them
     here so the Status section's buttons render as pills with
     proper section spacing. */
  .filter-section {
    padding: 4px 0;
    border-top: 1px solid var(--border-muted);
    margin-top: 4px;
    padding-top: 8px;
  }

  .filter-section-label {
    font-size: 9px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    margin-bottom: 6px;
  }

  .pill-buttons {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }

  .pill-btn {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 2px 8px;
    font-size: 10px;
    color: var(--text-secondary);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    transition:
      background 0.1s,
      border-color 0.1s,
      color 0.1s;
  }

  .pill-btn:hover {
    background: var(--bg-surface-hover);
    border-color: var(--border-default);
  }

  /* Status filter pills mirror the dot indicators: green = active,
     amber = stale, red = unclean. Inactive state shows a faint
     colored border so the colors are recognizable without selection. */
  .pill-btn--status-active {
    color: color-mix(in srgb, var(--accent-green) 75%, var(--text-secondary));
    border-color: color-mix(in srgb, var(--accent-green) 35%, transparent);
  }
  .pill-btn--status-active:hover {
    border-color: color-mix(in srgb, var(--accent-green) 65%, transparent);
  }
  .pill-btn--status-active.active {
    background: color-mix(in srgb, var(--accent-green) 12%, transparent);
    border-color: var(--accent-green);
    color: var(--accent-green);
  }

  .pill-btn--status-stale {
    color: color-mix(in srgb, var(--accent-amber) 75%, var(--text-secondary));
    border-color: color-mix(in srgb, var(--accent-amber) 35%, transparent);
  }
  .pill-btn--status-stale:hover {
    border-color: color-mix(in srgb, var(--accent-amber) 65%, transparent);
  }
  .pill-btn--status-stale.active {
    background: color-mix(in srgb, var(--accent-amber) 12%, transparent);
    border-color: var(--accent-amber);
    color: var(--accent-amber);
  }

  .pill-btn--status-unclean {
    color: color-mix(in srgb, var(--accent-red) 75%, var(--text-secondary));
    border-color: color-mix(in srgb, var(--accent-red) 35%, transparent);
  }
  .pill-btn--status-unclean:hover {
    border-color: color-mix(in srgb, var(--accent-red) 65%, transparent);
  }
  .pill-btn--status-unclean.active {
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
    border-color: var(--accent-red);
    color: var(--accent-red);
  }

  .session-list-scroll {
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
  }

  /* Group headers (agent and project) */
  .group-header {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    height: 28px;
    padding: 0 10px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: none;
    letter-spacing: 0.02em;
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-muted);
    cursor: pointer;
    transition: color 0.1s, background 0.1s;
    user-select: none;
  }

  .group-header:hover {
    color: var(--text-secondary);
    background: var(--bg-surface-hover);
  }

  .chevron {
    flex-shrink: 0;
    transition: transform 0.15s ease;
  }

  .chevron.expanded {
    transform: rotate(90deg);
  }

  .group-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .project-icon {
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .group-name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .group-count {
    flex-shrink: 0;
    font-size: 9px;
    font-weight: 500;
    color: var(--text-muted);
    background: var(--bg-surface);
    padding: 0 5px;
    border-radius: 8px;
    line-height: 16px;
  }

  /* Sub-group headers (Subagents, Team) at depth 1 */
  .sub-group-header {
    display: flex;
    align-items: center;
    gap: 5px;
    width: 100%;
    height: 28px;
    font-size: 11px;
    color: var(--text-muted);
    cursor: pointer;
    user-select: none;
    background: transparent;
    border: none;
    transition: background 0.1s;
  }

  .sub-group-header:hover {
    background: var(--bg-surface-hover);
  }

  .sub-group-arrow {
    flex-shrink: 0;
    transition: transform 150ms ease;
    color: var(--text-muted);
    opacity: 0.5;
  }

  .sub-group-arrow.expanded {
    transform: rotate(90deg);
  }

  .sub-group-icon {
    flex-shrink: 0;
    color: var(--text-muted);
    opacity: 0.6;
  }

  .sub-group-label {
    font-weight: 600;
    font-size: 10px;
    letter-spacing: 0.02em;
    text-transform: uppercase;
  }

  .sub-group-count {
    font-size: 9px;
    color: var(--text-muted);
    font-weight: 500;
  }

</style>
