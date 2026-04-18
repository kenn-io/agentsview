<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import type { ProjectAnalytics } from "../../api/types.js";

  const MAX_PROJECTS = 15;
  const BAR_HEIGHT = 20;
  const ROW_GAP = 4;
  const LABEL_WIDTH = 140;

  const rows = $derived.by(() => {
    const projects = analytics.projects?.projects;
    if (!projects || projects.length === 0) return [];

    const sorted = [...projects].sort(
      (a, b) => b.messages - a.messages,
    );

    if (sorted.length <= MAX_PROJECTS) return sorted;

    const top = sorted.slice(0, MAX_PROJECTS);
    const rest = sorted.slice(MAX_PROJECTS);
    const other: ProjectAnalytics = {
      name: `Other (${rest.length})`,
      sessions: rest.reduce((s, p) => s + p.sessions, 0),
      messages: rest.reduce((s, p) => s + p.messages, 0),
      first_session: "",
      last_session: "",
      avg_messages: 0,
      median_messages: 0,
      agents: {},
      daily_trend: 0,
    };
    return [...top, other];
  });

  const maxMessages = $derived(
    rows.length > 0
      ? Math.max(...rows.map((r) => r.messages), 1)
      : 1,
  );

  function barWidth(messages: number): number {
    return (messages / maxMessages) * 100;
  }

  function truncateName(name: string, max: number): string {
    if (name.length <= max) return name;
    return name.slice(0, max - 1) + "\u2026";
  }

  function handleClick(project: ProjectAnalytics) {
    if (project.name.startsWith("Other (")) return;
    analytics.setProject(project.name);
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function handleHover(
    e: MouseEvent,
    project: ProjectAnalytics,
  ) {
    const rect = (
      e.currentTarget as HTMLElement
    ).getBoundingClientRect();
    const agents = Object.entries(project.agents)
      .sort(([, a], [, b]) => b - a)
      .map(([name, count]) => `${name}: ${count}`)
      .join(", ");
    const parts = [
      `${project.messages.toLocaleString()} messages`,
      `${project.sessions} sessions`,
    ];
    if (agents) parts.push(agents);
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: parts.join(" | "),
    };
  }

  function handleLeave() {
    tooltip = null;
  }
</script>

<div class="breakdown-container">
  <div class="breakdown-header">
    <h3 class="chart-title">Projects</h3>
    {#if rows.length > 0}
      <span class="count">{analytics.projects?.projects.length ?? 0} total</span>
    {/if}
  </div>

  {#if analytics.errors.projects}
    <div class="error">
      {analytics.errors.projects}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchProjects()}
      >
        Retry
      </button>
    </div>
  {:else if rows.length > 0}
    <div class="bar-list">
      {#each rows as project, i}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="bar-row"
          class:clickable={!project.name.startsWith("Other (")}
          class:selected={analytics.project === project.name}
          class:dimmed={analytics.project !== "" && analytics.project !== project.name}
          onclick={() => handleClick(project)}
          onmouseenter={(e) => handleHover(e, project)}
          onmouseleave={handleLeave}
        >
          <span class="project-name" title={project.name}>
            {truncateName(project.name, 24)}
          </span>
          <div class="bar-track">
            <div
              class="bar-fill"
              style="width: {barWidth(project.messages)}%"
            ></div>
          </div>
          <span class="bar-value">
            {project.messages.toLocaleString()}
          </span>
        </div>
      {/each}
    </div>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        {tooltip.text}
      </div>
    {/if}
  {:else}
    <div class="empty">No project data</div>
  {/if}
</div>

<style>
  .breakdown-container {
    position: relative;
    flex: 1;
  }

  .breakdown-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .count {
    font-size: 10px;
    color: var(--text-muted);
  }

  .bar-list {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .bar-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    transition: background 0.1s;
  }

  .bar-row.clickable {
    cursor: pointer;
  }

  .bar-row.clickable:hover {
    background: var(--bg-surface-hover);
  }

  .bar-row.selected {
    background: color-mix(
      in srgb, var(--accent-blue) 12%, transparent
    );
  }

  .bar-row.dimmed {
    opacity: 0.35;
  }

  .bar-row.dimmed:hover {
    opacity: 0.7;
  }

  .project-name {
    flex-shrink: 0;
    width: 140px;
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .bar-track {
    flex: 1;
    height: 14px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .bar-fill {
    height: 100%;
    background: var(--accent-blue);
    border-radius: 2px;
    min-width: 2px;
  }

  .bar-value {
    flex-shrink: 0;
    width: 52px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 4px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 10px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: 100;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: inherit;
    cursor: pointer;
  }
</style>
