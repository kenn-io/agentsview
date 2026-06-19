<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import {
    CalendarIcon,
    ClockIcon,
    FolderIcon,
    MessageSquareTextIcon,
    MonitorIcon,
    XIcon,
  } from "../../icons.js";
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
    (analytics.automatedScope !== "human" ? 1 : 0) +
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
          <CalendarIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {dateLabel}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if analytics.project}
      <button
        class="filter-chip"
        onclick={() => analytics.clearProject()}
        title="Clear project filter"
      >
        <span class="chip-icon">
          <FolderIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {analytics.project}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each selectedMachines as machine (machine)}
      <button
        class="filter-chip"
        onclick={() => analytics.removeMachine(machine)}
        title="Remove {machine} filter"
      >
        <span class="chip-icon">
          <MonitorIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {machine}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
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
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#if analytics.minUserMessages > 0}
      <button
        class="filter-chip"
        onclick={() => analytics.clearMinUserMessages()}
        title="Clear min prompts filter"
      >
        <span class="chip-icon">
          <MessageSquareTextIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        &ge;{analytics.minUserMessages} prompts
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if analytics.recentlyActive}
      <button
        class="filter-chip"
        onclick={() => analytics.clearRecentlyActive()}
        title="Clear recently active filter"
      >
        <span class="chip-icon">
          <ClockIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        Active 24h
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each selectedStatuses as status (status)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleTerminationStatus(status)}
        title="Remove {STATUS_LABEL[status] ?? status} from status filter"
      >
        Status: {STATUS_LABEL[status] ?? status}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#if !analytics.includeOneShot}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeOneShot()}
        title="Clear single-turn filter"
      >
        Single-turn hidden
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if analytics.automatedScope !== "human"}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeAutomated()}
        title="Clear automated filter"
      >
        {analytics.automatedScope === "automated"
          ? "Only automated"
          : "Automated included"}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if hasTime}
      <button
        class="filter-chip"
        onclick={() => analytics.clearTimeFilter()}
        title="Clear time filter"
      >
        <span class="chip-icon">
          <ClockIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {timeLabel}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
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
    display: inline-flex;
    align-items: center;
    margin-left: 2px;
    opacity: 0.6;
    flex-shrink: 0;
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
