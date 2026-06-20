<script lang="ts">
  import type { Snippet } from "svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { hasSessionRouteDateIntent } from "../../stores/sessionRouteParams.js";
  import { starred } from "../../stores/starred.svelte.js";
  import {
    agentColor,
    agentLabel,
  } from "../../utils/agents.js";
  import type { GroupMode } from "../sidebar/session-list-utils.js";
  import { CheckIcon, FunnelIcon } from "../../icons.js";

  interface Props {
    groupMode?: GroupMode;
    showDisplay?: boolean;
    showStarred?: boolean;
    align?: "left" | "right";
    onToggleGroupByAgent?: () => void;
    onToggleGroupByProject?: () => void;
    onClearGroupMode?: () => void;
    extraActive?: boolean;
    onClearExtra?: () => void;
    extraSections?: Snippet;
  }

  let {
    groupMode = "none",
    showDisplay = true,
    showStarred = true,
    align = "right",
    onToggleGroupByAgent,
    onToggleGroupByProject,
    onClearGroupMode,
    extraActive = false,
    onClearExtra,
    extraSections,
  }: Props = $props();

  let open = $state(false);
  let filterBtnRef: HTMLButtonElement | undefined =
    $state(undefined);
  let dropdownRef: HTMLDivElement | undefined =
    $state(undefined);
  let agentSearch = $state("");
  let machineSearch = $state("");

  const sortedAgents = $derived.by(() => {
    const agents = [...sessions.agents].sort(
      (a, b) => b.session_count - a.session_count,
    );
    if (!agentSearch) return agents;
    const q = agentSearch.toLowerCase();
    return agents.filter((a) =>
      agentLabel(a.name).toLowerCase().includes(q),
    );
  });

  const sortedMachines = $derived.by(() => {
    const machines = [...sessions.machines].sort();
    if (!machineSearch) return machines;
    const q = machineSearch.toLowerCase();
    return machines.filter((m) => m.toLowerCase().includes(q));
  });

  $effect(() => {
    if (open) {
      sessions.loadAgents();
      sessions.loadMachines();
      agentSearch = "";
      machineSearch = "";
    }
  });

  let hasFilters = $derived(
    sessions.hasActiveFilters ||
      (showStarred && starred.filterOnly) ||
      extraActive,
  );
  let isRecentlyActiveOn = $derived(
    sessions.filters.recentlyActive,
  );
  let isHideUnknownOn = $derived(
    sessions.filters.hideUnknownProject,
  );
  let isHideSingleTurnOn = $derived(
    !sessions.filters.includeOneShot,
  );
  let isIncludeAutomatedOn = $derived(
    sessions.filters.includeAutomated,
  );

  $effect(() => {
    if (!open) return;
    function onClickOutside(e: MouseEvent) {
      const target = e.target as Node;
      if (
        filterBtnRef?.contains(target) ||
        dropdownRef?.contains(target)
      )
        return;
      open = false;
    }
    document.addEventListener("click", onClickOutside, true);
    return () =>
      document.removeEventListener(
        "click",
        onClickOutside,
        true,
      );
  });

  function clearFilters() {
    onClearGroupMode?.();
    onClearExtra?.();
    const clearDateYoke = hasSessionRouteDateIntent(
      router.route,
      router.params,
    );
    if (sessions.hasActiveFilters && starred.filterOnly) {
      if (showStarred) starred.filterOnly = false;
      sessions.clearSessionFilters({ clearDateYoke });
    } else if (sessions.hasActiveFilters) {
      sessions.clearSessionFilters({ clearDateYoke });
    } else if (showStarred && starred.filterOnly) {
      starred.filterOnly = false;
      sessions.load();
    }
  }

  function toggleStarredOnly() {
    starred.filterOnly = !starred.filterOnly;
    sessions.load();
  }
</script>

<button
  class="filter-btn"
  bind:this={filterBtnRef}
  onclick={() => (open = !open)}
  title="Filter sessions"
  aria-label="Filters"
  aria-expanded={open}
>
  <FunnelIcon size="14" strokeWidth="2" aria-hidden="true" />
  {#if hasFilters || (showDisplay && groupMode !== "none")}
    <span class="filter-indicator"></span>
  {/if}
</button>

{#if open}
  <div
    class="filter-dropdown"
    class:left={align === "left"}
    bind:this={dropdownRef}
  >
    {#if showDisplay}
      <div class="filter-section">
        <div class="filter-section-label">Display</div>
        <button
          class="filter-toggle"
          class:active={groupMode === "agent"}
          onclick={onToggleGroupByAgent}
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
          onclick={onToggleGroupByProject}
        >
          <span
            class="toggle-check"
            class:on={groupMode === "project"}
          ></span>
          Group by project
        </button>
      </div>
    {/if}
    {#if showStarred}
      <div class="filter-section">
        <div class="filter-section-label">Starred</div>
        <button
          class="filter-toggle"
          class:active={starred.filterOnly}
          onclick={toggleStarredOnly}
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
    {/if}
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
        class:active={isHideSingleTurnOn}
        onclick={() =>
          sessions.setIncludeOneShotFilter(
            isHideSingleTurnOn,
          )}
      >
        <span
          class="toggle-check"
          class:on={isHideSingleTurnOn}
        ></span>
        Hide single-turn
      </button>
      <button
        class="filter-toggle"
        class:active={isIncludeAutomatedOn}
        onclick={() =>
          sessions.setIncludeAutomatedFilter(
            !isIncludeAutomatedOn,
          )}
      >
        <span
          class="toggle-check"
          class:on={isIncludeAutomatedOn}
        ></span>
        Include automated sessions
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
        <button
          class="agent-select-row"
          class:selected={!sessions.filters.agent}
          style:--agent-color={"var(--accent-blue)"}
          onclick={() => sessions.setAgentFilter("")}
        >
          <span
            class="agent-check"
            class:on={!sessions.filters.agent}
          >
            {#if !sessions.filters.agent}
              <CheckIcon size="8" strokeWidth="2.4" aria-hidden="true" />
            {/if}
          </span>
          <span class="agent-select-name">All agents</span>
        </button>
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
            >
              {#if selected}
                <CheckIcon size="8" strokeWidth="2.4" aria-hidden="true" />
              {/if}
            </span>
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
              sessions.isMachineSelected(machine)}
            <button
              class="agent-select-row"
              class:selected
              style:--agent-color={"var(--accent-blue)"}
              onclick={() =>
                sessions.toggleMachineFilter(machine)}
            >
              <span
                class="agent-check"
                class:on={selected}
              >
                {#if selected}
                  <CheckIcon size="8" strokeWidth="2.4" aria-hidden="true" />
                {/if}
              </span>
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

    {@render extraSections?.()}

    {#if hasFilters || (showDisplay && groupMode !== "none")}
      <button
        class="clear-filters-btn"
        onclick={clearFilters}
      >
        Clear filters
      </button>
    {/if}
  </div>
{/if}

<style>
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
    max-height: min(560px, calc(100vh - 128px));
    overflow-y: auto;
    overflow-x: hidden;
    overscroll-behavior: contain;
    scrollbar-gutter: stable;
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

  .filter-dropdown.left {
    left: 0;
    right: auto;
    transform-origin: top left;
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
    color: var(--agent-color, var(--accent-blue));
    font-weight: 500;
    background: color-mix(
      in srgb,
      var(--agent-color, var(--accent-blue)) 10%,
      transparent
    );
  }

  .agent-check {
    width: 10px;
    height: 10px;
    border-radius: 2px;
    border: 1.5px solid var(--border-default);
    flex-shrink: 0;
    transition: background 0.1s, border-color 0.1s;
    color: white;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .agent-check.on {
    background: var(--agent-color, var(--accent-blue));
    border-color: var(--agent-color, var(--accent-blue));
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
</style>
