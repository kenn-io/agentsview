<script lang="ts">
  import { onDestroy } from "svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { starred } from "../../stores/starred.svelte.js";
  import SessionItem from "./SessionItem.svelte";
  import { formatNumber } from "../../utils/format.js";
  import {
    agentColor,
    agentLabel,
  } from "../../utils/agents.js";
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
  let showFilterDropdown = $state(false);
  let filterBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let dropdownRef: HTMLDivElement | undefined =
    $state(undefined);
  let agentSearch = $state("");

  // Agents sorted by frequency, filtered by search.
  let sortedAgents = $derived.by(() => {
    const agents = [...sessions.agents].sort(
      (a, b) => b.session_count - a.session_count,
    );
    if (!agentSearch) return agents;
    const q = agentSearch.toLowerCase();
    return agents.filter((a) =>
      agentLabel(a.name).toLowerCase().includes(q),
    );
  });

  let machineSearch = $state("");
  let sortedMachines = $derived.by(() => {
    const machines = [...sessions.machines].sort();
    if (!machineSearch) return machines;
    const q = machineSearch.toLowerCase();
    return machines.filter((m) => m.toLowerCase().includes(q));
  });

  // Ensure agents and machines are loaded when dropdown opens.
  $effect(() => {
    if (showFilterDropdown) {
      sessions.loadAgents();
      sessions.loadMachines();
      agentSearch = "";
      machineSearch = "";
    }
  });

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

  let hasFilters = $derived(
    sessions.hasActiveFilters || starred.filterOnly,
  );
  let isRecentlyActiveOn = $derived(
    sessions.filters.recentlyActive,
  );
  let isHideUnknownOn = $derived(
    sessions.filters.hideUnknownProject,
  );
  let isIncludeOneShotOn = $derived(
    sessions.filters.includeOneShot,
  );

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

  // Close filter dropdown on outside click.
  $effect(() => {
    if (!showFilterDropdown) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        filterBtnRef?.contains(target) ||
        dropdownRef?.contains(target)
      )
        return;
      showFilterDropdown = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
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
    <button
      class="filter-btn"
      bind:this={filterBtnRef}
      onclick={() =>
        (showFilterDropdown = !showFilterDropdown)}
    >
      <svg
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <polygon
          points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3"
        />
      </svg>
      {#if hasFilters || groupMode !== "none"}
        <span class="filter-indicator"></span>
      {/if}
    </button>
    {#if showFilterDropdown}
      <div class="filter-dropdown" bind:this={dropdownRef}>
        <div class="filter-section">
          <div class="filter-section-label">Display</div>
          <button
            class="filter-toggle"
            class:active={groupMode === "agent"}
            onclick={toggleGroupByAgent}
          >
            <span
              class="toggle-check"
              class:on={groupMode === "agent"}
            ></span>
            Group by agent
          </button>
          <button
            class="filter-toggle"
            class:active={groupMode === "project"}
            onclick={toggleGroupByProject}
          >
            <span
              class="toggle-check"
              class:on={groupMode === "project"}
            ></span>
            Group by project
          </button>
        </div>
        <div class="filter-section">
          <div class="filter-section-label">Starred</div>
          <button
            class="filter-toggle"
            class:active={starred.filterOnly}
            onclick={() => (starred.filterOnly = !starred.filterOnly)}
          >
            <span
              class="toggle-check"
              class:on={starred.filterOnly}
            ></span>
            Starred only
            {#if starred.count > 0}
              <span class="starred-count">{starred.count}</span>
            {/if}
          </button>
        </div>
        <div class="filter-section">
          <div class="filter-section-label">Activity</div>
          <button
            class="filter-toggle"
            class:active={isRecentlyActiveOn}
            onclick={() =>
              sessions.setRecentlyActiveFilter(
                !isRecentlyActiveOn,
              )}
          >
            <span
              class="toggle-check"
              class:on={isRecentlyActiveOn}
            ></span>
            Recently Active
          </button>
        </div>
        <div class="filter-section">
          <div class="filter-section-label">
            Session Type
          </div>
          <button
            class="filter-toggle"
            class:active={isIncludeOneShotOn}
            onclick={() =>
              sessions.setIncludeOneShotFilter(
                !isIncludeOneShotOn,
              )}
          >
            <span
              class="toggle-check"
              class:on={isIncludeOneShotOn}
            ></span>
            Include single-turn
          </button>
        </div>
        <div class="filter-section">
          <div class="filter-section-label">Project</div>
          <button
            class="filter-toggle"
            class:active={isHideUnknownOn}
            onclick={() =>
              sessions.setHideUnknownProjectFilter(
                !isHideUnknownOn,
              )}
          >
            <span
              class="toggle-check"
              class:on={isHideUnknownOn}
            ></span>
            Hide unknown
          </button>
        </div>
        <div class="filter-section">
          <div class="filter-section-label">Agent</div>
          {#if sessions.agents.length > 5}
            <input
              class="agent-search"
              type="text"
              placeholder="Search agents..."
              bind:value={agentSearch}
            />
          {/if}
          <div class="agent-select-list">
            {#each sortedAgents as agent (agent.name)}
              {@const selected =
                sessions.isAgentSelected(agent.name)}
              <button
                class="agent-select-row"
                class:selected
                style:--agent-color={agentColor(agent.name)}
                onclick={() =>
                  sessions.toggleAgentFilter(agent.name)}
              >
                <span
                  class="agent-check"
                  class:on={selected}
                ></span>
                <span
                  class="agent-dot-mini"
                  style:background={agentColor(agent.name)}
                ></span>
                <span class="agent-select-name">
                  {agentLabel(agent.name)}
                </span>
                <span class="agent-select-count">
                  {agent.session_count}
                </span>
              </button>
            {:else}
              <span class="agent-select-empty">
                {agentSearch ? "No match" : "No agents"}
              </span>
            {/each}
          </div>
        </div>
        {#if sessions.machines.length > 0}
          <div class="filter-section">
            <div class="filter-section-label">Machine</div>
            {#if sessions.machines.length > 5}
              <input
                class="agent-search"
                type="text"
                placeholder="Search machines..."
                bind:value={machineSearch}
              />
            {/if}
            <div class="agent-select-list">
              {#each sortedMachines as machine (machine)}
                {@const selected =
                  sessions.filters.machine === machine}
                <button
                  class="agent-select-row"
                  class:selected
                  onclick={() =>
                    sessions.setMachineFilter(machine)}
                >
                  <span
                    class="agent-check"
                    class:on={selected}
                  ></span>
                  <span class="agent-select-name">
                    {machine}
                  </span>
                </button>
              {:else}
                <span class="agent-select-empty">
                  {machineSearch ? "No match" : "No machines"}
                </span>
              {/each}
            </div>
          </div>
        {/if}
        <div class="filter-section">
          <div class="filter-section-label">Min Prompts</div>
          <div class="pill-buttons">
            {#each [2, 3, 5, 10] as n}
              <button
                class="pill-btn"
                class:active={sessions.filters.minUserMessages === n}
                onclick={() =>
                  sessions.setMinUserMessagesFilter(n)}
              >
                {n}
              </button>
            {/each}
          </div>
        </div>
        {#if hasFilters || groupMode !== "none"}
          <button
            class="clear-filters-btn"
            onclick={() => {
              if (groupMode !== "none") setGroupMode("none");
              if (sessions.hasActiveFilters && starred.filterOnly) {
                starred.filterOnly = false;
                sessions.clearSessionFilters();
              } else if (sessions.hasActiveFilters) {
                sessions.clearSessionFilters();
              } else if (starred.filterOnly) {
                starred.filterOnly = false;
              }
            }}
          >
            Clear filters
          </button>
        {/if}
      </div>
    {/if}
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

  .filter-btn {
    position: relative;
    display: flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border-radius: 4px;
    color: var(--text-muted);
    transition: color 0.1s, background 0.1s;
  }

  .filter-btn:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .filter-indicator {
    position: absolute;
    top: 2px;
    right: 2px;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent-green);
  }

  .filter-dropdown {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: 220px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-lg);
    padding: 8px;
    z-index: 100;
    text-transform: none;
    letter-spacing: normal;
    animation: dropdown-in 0.12s ease-out;
    transform-origin: top right;
  }

  @keyframes dropdown-in {
    from {
      opacity: 0;
      transform: scale(0.95) translateY(-2px);
    }
    to {
      opacity: 1;
      transform: scale(1) translateY(0);
    }
  }

  .filter-section {
    padding: 4px 0;
  }

  .filter-section + .filter-section {
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

  .filter-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 4px 8px;
    font-size: 11px;
    color: var(--text-secondary);
    text-align: left;
    border-radius: 4px;
    transition: background 0.1s, color 0.1s;
  }

  .filter-toggle:hover {
    background: var(--bg-surface-hover);
  }

  .filter-toggle.active {
    background: var(--bg-surface-hover);
    color: var(--accent-green);
    font-weight: 500;
  }

  .toggle-check {
    width: 10px;
    height: 10px;
    border-radius: 2px;
    border: 1.5px solid var(--border-default);
    flex-shrink: 0;
    transition: background 0.1s, border-color 0.1s;
  }

  .toggle-check.on {
    background: var(--accent-green);
    border-color: var(--accent-green);
  }

  .agent-search {
    width: 100%;
    height: 24px;
    padding: 0 8px;
    margin-bottom: 4px;
    font-size: 10px;
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    outline: none;
    transition: border-color 0.1s;
  }

  .agent-search::placeholder {
    color: var(--text-muted);
  }

  .agent-search:focus {
    border-color: var(--accent-blue);
  }

  .agent-select-list {
    display: flex;
    flex-direction: column;
    max-height: 180px;
    overflow-y: auto;
    overflow-x: hidden;
    gap: 1px;
  }

  .agent-select-row {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 3px 8px;
    font-size: 11px;
    color: var(--text-secondary);
    text-align: left;
    border-radius: 3px;
    transition: background 0.08s, color 0.08s;
    flex-shrink: 0;
  }

  .agent-select-row:hover {
    background: var(--bg-surface-hover);
  }

  .agent-select-row.selected {
    color: var(--agent-color);
    font-weight: 500;
  }

  .agent-check {
    width: 10px;
    height: 10px;
    border-radius: 2px;
    border: 1.5px solid var(--border-default);
    flex-shrink: 0;
    transition: background 0.1s, border-color 0.1s;
  }

  .agent-check.on {
    background: var(--agent-color);
    border-color: var(--agent-color);
  }

  .agent-dot-mini {
    width: 5px;
    height: 5px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .agent-select-name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .agent-select-count {
    flex-shrink: 0;
    font-size: 9px;
    font-weight: 500;
    color: var(--text-muted);
    min-width: 14px;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .agent-select-empty {
    display: block;
    padding: 6px 8px;
    font-size: 10px;
    color: var(--text-muted);
    text-align: center;
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

  .pill-btn.active {
    background: var(--bg-surface-hover);
    border-color: var(--accent-green);
    color: var(--accent-green);
    font-weight: 500;
  }

  .clear-filters-btn {
    display: block;
    width: 100%;
    padding: 4px 8px;
    margin-top: 8px;
    font-size: 10px;
    color: var(--text-muted);
    text-align: center;
    border-top: 1px solid var(--border-muted);
    padding-top: 8px;
    transition: color 0.1s;
  }

  .starred-count {
    margin-left: auto;
    font-size: 9px;
    font-weight: 600;
    color: var(--accent-amber);
    min-width: 14px;
    text-align: center;
  }

  .clear-filters-btn:hover {
    color: var(--text-primary);
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
