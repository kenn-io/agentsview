<script lang="ts">
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { hasSessionRouteDateIntent } from "../../stores/sessionRouteParams.js";
  import {
    agentColor,
    agentLabel,
  } from "../../utils/agents.js";
  import { XIcon } from "../../icons.js";

  interface Props {
    projectFilters?: string[];
    modelFilters?: string[];
    onRemoveProject?: (project: string) => void;
    onClearProjects?: () => void;
    onRemoveModel?: (model: string) => void;
    onClearModels?: () => void;
  }

  let {
    projectFilters = [],
    modelFilters = [],
    onRemoveProject,
    onClearProjects,
    onRemoveModel,
    onClearModels,
  }: Props = $props();

  const selectedAgents = $derived(
    sessions.filters.agent
      ? sessions.filters.agent.split(",")
      : [],
  );
  const selectedMachines = $derived(
    sessions.filters.machine
      ? sessions.filters.machine.split(",")
      : [],
  );

  const hasFilters = $derived(
    !!sessions.filters.project ||
      sessions.hasActiveFilters ||
      projectFilters.length > 0 ||
      modelFilters.length > 0,
  );

  function clearProject() {
    sessions.filters.project = "";
    sessions.activeSessionId = null;
    sessions.load();
  }

  function removeMachine(machine: string) {
    sessions.toggleMachineFilter(machine);
  }

  function removeAgent(agent: string) {
    sessions.toggleAgentFilter(agent);
  }

  function clearAll() {
    sessions.filters.project = "";
    sessions.clearSessionFilters({
      clearDateYoke: hasSessionRouteDateIntent(
        router.route,
        router.params,
      ),
    });
    onClearProjects?.();
    onClearModels?.();
  }
</script>

{#if hasFilters}
  <div class="active-filters">
    <span class="filters-label">Filters:</span>

    {#if sessions.filters.project}
      <button
        class="filter-chip"
        onclick={clearProject}
        title="Clear project filter"
      >
        {sessions.filters.project}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each selectedMachines as machine (machine)}
      <button
        class="filter-chip"
        onclick={() => removeMachine(machine)}
        title="Remove {machine} filter"
      >
        {machine}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#each selectedAgents as agent (agent)}
      <button
        class="filter-chip"
        onclick={() => removeAgent(agent)}
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

    {#if sessions.filters.minUserMessages > 0}
      <button
        class="filter-chip"
        onclick={() => sessions.setMinUserMessagesFilter(0)}
        title="Clear min prompts filter"
      >
        &ge;{sessions.filters.minUserMessages} prompts
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if sessions.filters.recentlyActive}
      <button
        class="filter-chip"
        onclick={() => sessions.setRecentlyActiveFilter(false)}
        title="Clear recently active filter"
      >
        Active 24h
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if sessions.filters.hideUnknownProject}
      <button
        class="filter-chip"
        onclick={() => sessions.setHideUnknownProjectFilter(false)}
        title="Clear hidden unknown project filter"
      >
        Unknown hidden
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each projectFilters as project (project)}
      <button
        class="filter-chip"
        onclick={() => onRemoveProject?.(project)}
        title="Remove {project} project filter"
      >
        {project}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    {#if !sessions.filters.includeOneShot}
      <button
        class="filter-chip"
        onclick={() => sessions.setIncludeOneShotFilter(true)}
        title="Clear single-turn filter"
      >
        Single-turn hidden
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#if sessions.filters.includeAutomated}
      <button
        class="filter-chip"
        onclick={() => sessions.setIncludeAutomatedFilter(false)}
        title="Clear automated filter"
      >
        Automated included
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/if}

    {#each modelFilters as model (model)}
      <button
        class="filter-chip"
        onclick={() => onRemoveModel?.(model)}
        title="Remove {model} model filter"
      >
        {model}
        <span class="chip-x">
          <XIcon size="11" strokeWidth="2.4" aria-hidden="true" />
        </span>
      </button>
    {/each}

    <button
      class="clear-all"
      onclick={clearAll}
      title="Clear all filters"
    >
      Clear all
    </button>
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

  .agent-chip-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .chip-x {
    display: inline-flex;
    align-items: center;
    opacity: 0.65;
    flex-shrink: 0;
  }

  .clear-all {
    font-size: 11px;
    color: var(--text-muted);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .clear-all:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }
</style>
