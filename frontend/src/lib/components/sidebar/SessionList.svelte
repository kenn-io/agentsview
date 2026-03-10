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
    ITEM_HEIGHT,
    OVERSCAN,
    STORAGE_KEY,
    buildAgentSections,
    buildDisplayItems,
    computeTotalSize,
    findStart,
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

  // Ensure agents are loaded when dropdown opens.
  $effect(() => {
    if (showFilterDropdown) {
      sessions.loadAgents();
      agentSearch = "";
    }
  });

  let groupByAgent = $state(
    typeof localStorage !== "undefined" &&
      localStorage.getItem(STORAGE_KEY) === "true",
  );
  let manualExpanded: Set<string> = $state(new Set());
  // Start all collapsed when grouping is first enabled.
  let collapseAll = $state(true);

  $effect(() => {
    if (typeof localStorage !== "undefined") {
      localStorage.setItem(STORAGE_KEY, String(groupByAgent));
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
      .map((g) => ({
        ...g,
        sessions: g.sessions.filter((s) =>
          starred.isStarred(s.id),
        ),
      }))
      .filter((g) => g.sessions.length > 0);
  });

  // Build agent-grouped structure when groupByAgent is on.
  let agentSections = $derived.by(() =>
    buildAgentSections(groups, groupByAgent),
  );

  // Derive effective collapsed set synchronously so the first
  // render is already collapsed (no flicker).
  let collapsedAgents = $derived.by(() => {
    if (!groupByAgent) return new Set<string>();
    if (collapseAll) {
      return new Set(agentSections.map((s) => s.agent));
    }
    // Invert: all agents minus the manually expanded ones.
    const all = new Set(agentSections.map((s) => s.agent));
    for (const a of manualExpanded) all.delete(a);
    return all;
  });

  // Build flat display items for virtual scrolling.
  let displayItems = $derived.by(() =>
    buildDisplayItems(groups, agentSections, groupByAgent, collapsedAgents),
  );

  let totalCount = $derived(
    starred.filterOnly
      ? groups.reduce((n, g) => n + g.sessions.length, 0)
      : sessions.total,
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

  function toggleGroupByAgent() {
    groupByAgent = !groupByAgent;
    if (groupByAgent) {
      collapseAll = true;
      manualExpanded = new Set();
    }
  }

  function toggleAgent(agent: string) {
    if (collapseAll) {
      // First toggle after fresh group-enable: switch to
      // manual mode, expanding only the clicked agent.
      collapseAll = false;
      manualExpanded = new Set([agent]);
    } else {
      const next = new Set(manualExpanded);
      if (next.has(agent)) {
        next.delete(agent);
      } else {
        next.add(agent);
      }
      manualExpanded = next;
    }
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
      {#if hasFilters || groupByAgent}
        <span class="filter-indicator"></span>
      {/if}
    </button>
    {#if showFilterDropdown}
      <div class="filter-dropdown" bind:this={dropdownRef}>
        <div class="filter-section">
          <div class="filter-section-label">Display</div>
          <button
            class="filter-toggle"
            class:active={groupByAgent}
            onclick={toggleGroupByAgent}
          >
            <span
              class="toggle-check"
              class:on={groupByAgent}
            ></span>
            Group by agent
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
        {#if hasFilters}
          <button
            class="clear-filters-btn"
            onclick={() => {
              if (sessions.hasActiveFilters && starred.filterOnly) {
                starred.filterOnly = false;
                sessions.clearSessionFilters();
              } else if (sessions.hasActiveFilters) {
                sessions.clearSessionFilters();
              } else {
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
            class="agent-group-header"
            onclick={() => toggleAgent(item.agent)}
          >
            <svg
              class="chevron"
              class:expanded={!collapsedAgents.has(item.agent)}
              width="10"
              height="10"
              viewBox="0 0 16 16"
              fill="currentColor"
            >
              <path d="M6.22 3.22a.75.75 0 011.06 0l4.25 4.25a.75.75 0 010 1.06l-4.25 4.25a.75.75 0 01-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 010-1.06z"/>
            </svg>
            <span
              class="agent-group-dot"
              style:background={agentColor(item.agent)}
            ></span>
            <span class="agent-group-name">{item.agent}</span>
            <span class="agent-group-count">{item.count}</span>
          </button>
        {:else if item.group}
          {@const primary = item.group.sessions.find(
            (s) => s.id === item.group!.primarySessionId,
          ) ?? item.group.sessions[0]}
          {#if primary}
            <SessionItem
              session={primary}
              continuationCount={item.group.sessions.length}
              groupSessionIds={item.group.sessions.length > 1
                ? item.group.sessions.map((s) => s.id)
                : undefined}
              hideAgent={groupByAgent}
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

  /* Agent group headers */
  .agent-group-header {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    height: 28px;
    padding: 0 10px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: capitalize;
    letter-spacing: 0.02em;
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-muted);
    cursor: pointer;
    transition: color 0.1s, background 0.1s;
    user-select: none;
  }

  .agent-group-header:hover {
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

  .agent-group-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .agent-group-name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .agent-group-count {
    flex-shrink: 0;
    font-size: 9px;
    font-weight: 500;
    color: var(--text-muted);
    background: var(--bg-surface);
    padding: 0 5px;
    border-radius: 8px;
    line-height: 16px;
  }
</style>
