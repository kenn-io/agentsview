<script lang="ts">
  import { onDestroy } from "svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { starred } from "../../stores/starred.svelte.js";
  import SessionItem from "./SessionItem.svelte";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import {
    ChevronDownIcon,
    ChevronRightIcon,
    FolderIcon,
    UserRoundIcon,
    UsersRoundIcon,
  } from "../../icons.js";
  import { formatNumber } from "../../utils/format.js";
  import { agentColor } from "../../utils/agents.js";
  import {
    type DisplayItem,
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
  let initialHydratedVersion: number | null = $state(null);
  let initialHydratingVersion: number | null = $state(null);
  let paintedDisplayItems: DisplayItem[] = $state([]);
  let paintedTotalSize = $state(0);

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
  let renderDisplayItems = $derived(
    initialHydratedVersion === sessions.sidebarIndexVersion
      ? displayItems
      : paintedDisplayItems,
  );
  let renderTotalSize = $derived(
    initialHydratedVersion === sessions.sidebarIndexVersion
      ? totalSize
      : paintedTotalSize,
  );

  let visibleItems = $derived.by(() => {
    if (renderDisplayItems.length === 0) return [];
    const start = findStart(renderDisplayItems, scrollTop);
    const end = scrollTop + viewportHeight + OVERSCAN * ITEM_HEIGHT;
    const result: typeof renderDisplayItems = [];
    for (let i = start; i < renderDisplayItems.length; i++) {
      const item = renderDisplayItems[i]!;
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

  function effectiveViewportHeight(): number {
    // ResizeObserver/clientHeight normally reports before hydration starts.
    // If jsdom or a hidden container reports 0, use a fixed conservative
    // viewport so hydration remains deliberate instead of accidental.
    return viewportHeight > 0 ? viewportHeight : ITEM_HEIGHT * 8;
  }

  function sessionForItem(item: DisplayItem) {
    if (item.type !== "session") return undefined;
    if (item.isChild) return item.session;
    return item.group?.sessions.find(
      (s) => s.id === item.group!.primarySessionId,
    ) ?? item.group?.sessions[0];
  }

  function needsVisibleHydration(item: DisplayItem): boolean {
    const session = sessionForItem(item);
    if (!session?.is_index_only) return false;
    return !session.display_name;
  }

  function hydrationIdsForItems(items: DisplayItem[]): string[] {
    const ids: string[] = [];
    const seen = new Set<string>();
    for (const item of items) {
      if (!needsVisibleHydration(item)) continue;
      const id = sessionForItem(item)?.id;
      if (!id || seen.has(id)) continue;
      seen.add(id);
      ids.push(id);
    }
    return ids;
  }

  function initialHydrationIds(items: DisplayItem[]): string[] {
    const target =
      Math.ceil(effectiveViewportHeight() / ITEM_HEIGHT) + OVERSCAN;
    const sessionItems = items
      .filter((item) => item.type === "session")
      .slice(0, target);
    return hydrationIdsForItems(sessionItems);
  }

  function requestHydration(ids: string[], version: number) {
    if (ids.length === 0) return;
    void sessions.hydrateVisibleSessions(ids, version);
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

  $effect(() => {
    const version = sessions.sidebarIndexVersion;
    const ids = initialHydrationIds(displayItems);

    if (initialHydratedVersion === version) {
      paintedDisplayItems = displayItems;
      paintedTotalSize = totalSize;
      return;
    }
    if (initialHydratingVersion === version) return;

    if (ids.length === 0) {
      initialHydratedVersion = version;
      paintedDisplayItems = displayItems;
      paintedTotalSize = totalSize;
      return;
    }

    initialHydratingVersion = version;
    void (async () => {
      await sessions.hydrateVisibleSessions(ids, version);
      if (sessions.sidebarIndexVersion !== version) return;
      initialHydratedVersion = version;
      initialHydratingVersion = null;
      paintedDisplayItems = displayItems;
      paintedTotalSize = totalSize;
    })();
  });

  $effect(() => {
    const version = sessions.sidebarIndexVersion;
    if (initialHydratedVersion !== version) return;
    requestHydration(hydrationIdsForItems(visibleItems), version);
  });

  // Clamp stale scrollTop when count shrinks.
  $effect(() => {
    if (!containerRef) return;
    const maxTop = Math.max(
      0,
      renderTotalSize - containerRef.clientHeight,
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
    const items = renderDisplayItems;
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
    style="height: {renderTotalSize}px; width: 100%; position: relative;"
  >
    {#each visibleItems as item (item.id)}
      <div
        style="position: absolute; top: 0; left: 0; width: 100%; height: {item.height}px; transform: translateY({item.top}px);"
      >
        {#if item.type === "header"}
          <button
            class="group-header"
            onclick={() => toggleGroup(item.label)}
            title="{collapsed.has(item.label) ? 'Expand' : 'Collapse'} {item.label} group"
            aria-label="{collapsed.has(item.label) ? 'Expand' : 'Collapse'} {item.label} group"
          >
            {#if collapsed.has(item.label)}
              <ChevronRightIcon class="chevron" size="10" strokeWidth="2.5" aria-hidden="true" />
            {:else}
              <ChevronDownIcon class="chevron" size="10" strokeWidth="2.5" aria-hidden="true" />
            {/if}
            {#if groupMode === "agent"}
              <span
                class="group-dot"
                style:background={agentColor(item.label)}
              ></span>
            {:else}
              <FolderIcon class="project-icon" size="11" strokeWidth="1.8" aria-hidden="true" />
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
            title="{subExpanded ? 'Collapse' : 'Expand'} Subagents group"
            aria-label="{subExpanded ? 'Collapse' : 'Expand'} Subagents group"
          >
            {#if subExpanded}
              <ChevronDownIcon class="sub-group-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
            {:else}
              <ChevronRightIcon class="sub-group-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
            {/if}
            <UserRoundIcon class="sub-group-icon" size="10" strokeWidth="2" aria-hidden="true" />
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
            title="{teamExpanded ? 'Collapse' : 'Expand'} Team group"
            aria-label="{teamExpanded ? 'Collapse' : 'Expand'} Team group"
          >
            {#if teamExpanded}
              <ChevronDownIcon class="sub-group-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
            {:else}
              <ChevronRightIcon class="sub-group-arrow" size="10" strokeWidth="2.5" aria-hidden="true" />
            {/if}
            <UsersRoundIcon class="sub-group-icon" size="12" strokeWidth="2" aria-hidden="true" />
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
          {@const groupHasTeammates = children.some((s) => s.is_teammate ?? s.first_message?.includes("<teammate-message") ?? false)}
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

  :global(.chevron) {
    flex-shrink: 0;
  }

  .group-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  :global(.project-icon) {
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

  :global(.sub-group-arrow) {
    flex-shrink: 0;
    color: var(--text-muted);
    opacity: 0.5;
  }

  :global(.sub-group-icon) {
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
