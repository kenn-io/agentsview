<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import {
    CalendarIcon,
    ClockIcon,
    CodeIcon,
    FolderIcon,
    MessageSquareTextIcon,
    MonitorIcon,
    XIcon,
  } from "../../icons.js";
  import { agentColor, agentLabel } from "../../utils/agents.js";
  import { m } from "../../i18n/index.js";
  import {
    branchTokenLabel,
    BRANCH_LIST_SEP,
  } from "../../branchFilters.js";

  const selectedAgents = $derived(
    analytics.agent
      ? analytics.agent.split(",")
      : [],
  );
  const selectedModels = $derived(
    analytics.model
      ? analytics.model.split(",").filter((model) => model.length > 0)
      : [],
  );
  const selectedMachines = $derived(
    analytics.machine
      ? analytics.machine.split(",")
      : [],
  );
  const selectedBranches = $derived(
    analytics.branch
      ? analytics.branch.split(BRANCH_LIST_SEP).map((token) => ({
          token,
          label: branchTokenLabel(token, m.shared_no_branch()),
        }))
      : [],
  );

  const selectedStatuses = $derived(
    analytics.termination
      ? analytics.termination.split(",").filter((s) => s.length > 0)
      : [],
  );

  function statusLabel(status: string): string {
    switch (status) {
      case "active":
        return m.analytics_status_active();
      case "stale":
        return m.analytics_status_stale();
      case "unclean":
        return m.analytics_status_unclean();
      default:
        return status;
    }
  }

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
    selectedBranches.length +
    selectedAgents.length +
    selectedModels.length +
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
    <span class="filters-label">{m.shared_active_filters_label()}</span>

    {#if analytics.selectedDate}
      <button
        class="filter-chip"
        onclick={() => analytics.clearDate()}
        title={m.analytics_filters_clear_date()}
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
        title={m.shared_active_filters_clear_project()}
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
        title={m.shared_active_filters_remove_machine({ machine })}
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

    {#each selectedBranches as branch (branch.token)}
      <button
        class="filter-chip"
        onclick={() => analytics.removeBranch(branch.token)}
        title={m.shared_active_filters_remove_branch({ branch: branch.label })}
      >
        {branch.label}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#each selectedAgents as agent (agent)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleAgent(agent)}
        title={m.shared_active_filters_remove_agent({ agent: agentLabel(agent) })}
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

    {#each selectedModels as model (model)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleModel(model)}
        title="Remove {model} filter"
      >
        <span class="chip-icon">
          <CodeIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {model}
        <span class="chip-x">&times;</span>
      </button>
    {/each}

    {#if analytics.minUserMessages > 0}
      <button
        class="filter-chip"
        onclick={() => analytics.clearMinUserMessages()}
        title={m.shared_active_filters_clear_min_prompts()}
      >
        <span class="chip-icon">
          <MessageSquareTextIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {m.shared_active_filters_min_prompts({ count: analytics.minUserMessages })}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if analytics.recentlyActive}
      <button
        class="filter-chip"
        onclick={() => analytics.clearRecentlyActive()}
        title={m.shared_active_filters_clear_recently_active()}
      >
        <span class="chip-icon">
          <ClockIcon size="10" strokeWidth="1.8" aria-hidden="true" />
        </span>
        {m.shared_active_filters_active24h()}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each selectedStatuses as status (status)}
      <button
        class="filter-chip"
        onclick={() => analytics.toggleTerminationStatus(status)}
        title={m.analytics_filters_remove_status({ status: statusLabel(status) })}
      >
        {m.analytics_filters_status({ status: statusLabel(status) })}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#if !analytics.includeOneShot}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeOneShot()}
        title={m.shared_active_filters_clear_single_turn()}
      >
        {m.shared_active_filters_single_turn_hidden()}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if analytics.automatedScope !== "human"}
      <button
        class="filter-chip"
        onclick={() => analytics.clearIncludeAutomated()}
        title={m.shared_active_filters_clear_automated()}
      >
        {analytics.automatedScope === "automated"
          ? m.analytics_filters_only_automated()
          : m.shared_active_filters_automated_included()}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if hasTime}
      <button
        class="filter-chip"
        onclick={() => analytics.clearTimeFilter()}
        title={m.analytics_filters_clear_time()}
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
        title={m.shared_active_filters_clear_all()}
      >
        {m.shared_active_filters_clear_all_label()}
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
