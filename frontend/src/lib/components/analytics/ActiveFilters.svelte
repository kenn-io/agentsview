<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import { agentColor, agentLabel } from "../../utils/agents.js";

  const selectedAgents = $derived(
    analytics.agent
      ? analytics.agent.split(",")
      : [],
  );
  const selectedMachines = $derived(
    analytics.machine
      ? analytics.machine.split(",")
      : [],
  );

  const selectedStatuses = $derived(
    analytics.termination
      ? analytics.termination.split(",").filter((s) => s.length > 0)
      : [],
  );

  const STATUS_LABEL: Record<string, string> = {
    active: "Active",
    stale: "Stale",
    unclean: "Unclean",
  };

  const DAY_LABELS = [
    "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun",
  ];

  const dateLabel = $derived.by(() => {
    if (!analytics.selectedDate) return "";
    const d = new Date(analytics.selectedDate + "T00:00:00");
    return d.toLocaleDateString("en", {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
  });

  const timeLabel = $derived.by(() => {
    const dow = analytics.selectedDow;
    const hour = analytics.selectedHour;
    if (dow !== null && hour !== null) {
      return `${DAY_LABELS[dow]} ${String(hour).padStart(2, "0")}:00`;
    }
    if (dow !== null) return DAY_LABELS[dow]!;
    if (hour !== null) {
      return `${String(hour).padStart(2, "0")}:00`;
    }
    return "";
  });

  const hasTime = $derived(
    analytics.selectedDow !== null ||
    analytics.selectedHour !== null,
  );

  const filterCount = $derived(
    (analytics.selectedDate !== null ? 1 : 0) +
    (analytics.project !== "" ? 1 : 0) +
    selectedMachines.length +
    selectedAgents.length +
    selectedStatuses.length +
    (analytics.minUserMessages > 0 ? 1 : 0) +
    (!analytics.includeOneShot ? 1 : 0) +
    (analytics.includeAutomated ? 1 : 0) +
    (analytics.recentlyActive ? 1 : 0) +
    (hasTime ? 1 : 0)
  );
</script>

{#if analytics.hasActiveFilters}
  <div class="active-filters">
    <span class="filters-label">Filters:</span>

    {#if analytics.selectedDate}
      <button
        class="filter-chip"
        onclick={() => analytics.clearDate()}
        title="Clear date filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M4.5 1a.5.5 0 01.5.5V2h6v-.5a.5.5
              0 011 0V2h1a2 2 0 012 2v9a2 2 0 01-2
              2H3a2 2 0 01-2-2V4a2 2 0 012-2h1v-.5a.5.5
              0 01.5-.5zM3 6v7a1 1 0 001 1h8a1 1 0
              001-1V6H3z"/>
          </svg>
        </span>
        {dateLabel}
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#if analytics.project}
      <button
        class="filter-chip"
        onclick={() => analytics.clearProject()}
        title="Clear project filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M1 3.5A1.5 1.5 0 012.5 2h2.764a1.5
              1.5 0 011.025.404l.961.878A.5.5 0
              007.59 3.5H13.5A1.5 1.5 0 0115 5v7.5a1.5
              1.5 0 01-1.5 1.5h-11A1.5 1.5 0 011
              12.5v-9z"/>
          </svg>
        </span>
        {analytics.project}
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#each selectedMachines as machine (machine)}
      <button
        class="filter-chip"
        onclick={() => analytics.removeMachine(machine)}
        title="Remove {machine} filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M2 3.5A1.5 1.5 0 013.5 2h9A1.5 1.5 0
              0114 3.5v5A1.5 1.5 0 0112.5 10H9v2h2.5a.5.5
              0 010 1h-7a.5.5 0 010-1H7v-2H3.5A1.5 1.5
              0 012 8.5v-5zM3.5 3a.5.5 0 00-.5.5v5a.5.5
              0 00.5.5h9a.5.5 0 00.5-.5v-5a.5.5 0
              00-.5-.5h-9z"/>
          </svg>
        </span>
        {machine}
        <span class="chip-x">&times;</span>
      </button>
    {/each}

    {#each selectedAgents as agent (agent)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleAgent(agent)}
        title="Remove {agentLabel(agent)} filter"
      >
        <span
          class="agent-chip-dot"
          style:background={agentColor(agent)}
        ></span>
        {agentLabel(agent)}
        <span class="chip-x">&times;</span>
      </button>
    {/each}

    {#if analytics.minUserMessages > 0}
      <button
        class="filter-chip"
        onclick={() => analytics.clearMinUserMessages()}
        title="Clear min prompts filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M5 8a1 1 0 11-2 0 1 1 0 012
              0zm4 0a1 1 0 11-2 0 1 1 0 012
              0zm3 1a1 1 0 100-2 1 1 0 000 2z"/>
            <path d="M2.165 15.803l.02-.004c1.83-.363
              2.948-.842 3.468-1.105A9.06 9.06 0
              008 15c4.418 0 8-3.134 8-7s-3.582-7-8-7-8
              3.134-8 7c0 1.76.743 3.37 1.97
              4.6a10.437 10.437 0
              01-.524 2.318l-.003.011a10.722 10.722 0
              01-.244.637c-.079.186.074.394.272.362z"/>
          </svg>
        </span>
        &ge;{analytics.minUserMessages} prompts
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#if analytics.recentlyActive}
      <button
        class="filter-chip"
        onclick={() => analytics.clearRecentlyActive()}
        title="Clear recently active filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M8 1a7 7 0 100 14A7 7 0 008 1zm.5
              3a.5.5 0 00-1 0v4a.5.5 0
              00.146.354l2 2a.5.5 0 00.708-.708L8.5
              7.793V4z"/>
          </svg>
        </span>
        Active 24h
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#each selectedStatuses as status (status)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleTerminationStatus(status)}
        title="Remove {STATUS_LABEL[status] ?? status} from status filter"
      >
        Status: {STATUS_LABEL[status] ?? status}
        <span class="chip-x">&times;</span>
      </button>
    {/each}

    {#if !analytics.includeOneShot}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeOneShot()}
        title="Clear single-turn filter"
      >
        Single-turn hidden
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#if analytics.includeAutomated}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeAutomated()}
        title="Clear automated filter"
      >
        Automated included
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#if hasTime}
      <button
        class="filter-chip"
        onclick={() => analytics.clearTimeFilter()}
        title="Clear time filter"
      >
        <span class="chip-icon">
          <svg width="10" height="10" viewBox="0 0 16 16"
            fill="currentColor">
            <path d="M8 1a7 7 0 100 14A7 7 0 008 1zm.5
              3a.5.5 0 00-1 0v4a.5.5 0
              00.146.354l2 2a.5.5 0 00.708-.708L8.5
              7.793V4z"/>
          </svg>
        </span>
        {timeLabel}
        <span class="chip-x">&times;</span>
      </button>
    {/if}

    {#if filterCount > 1}
      <button
        class="clear-all"
        onclick={() => analytics.clearAllFilters()}
        title="Clear all filters"
      >
        Clear all
      </button>
    {/if}
  </div>
{/if}

<style>
  .active-filters {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 16px 6px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    flex-wrap: wrap;
  }

  .filters-label {
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }

  .filter-chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 22px;
    padding: 0 6px;
    font-size: 11px;
    font-weight: 500;
    color: var(--accent-blue);
    background: color-mix(
      in srgb, var(--accent-blue) 10%, transparent
    );
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s;
  }

  .filter-chip:hover {
    background: color-mix(
      in srgb, var(--accent-blue) 18%, transparent
    );
  }

  .chip-icon {
    display: flex;
    align-items: center;
    opacity: 0.7;
  }

  .agent-chip-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .chip-x {
    font-size: 13px;
    line-height: 1;
    margin-left: 2px;
    opacity: 0.6;
  }

  .filter-chip:hover .chip-x {
    opacity: 1;
  }

  .clear-all {
    height: 22px;
    padding: 0 8px;
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .clear-all:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }
</style>
