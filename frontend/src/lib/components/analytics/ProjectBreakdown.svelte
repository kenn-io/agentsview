<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import { analytics } from "../../stores/analytics.svelte.js";
  import PanelStatus from "./PanelStatus.svelte";
  import type { DbProjectAnalytics as ProjectAnalytics } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";

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
    if (isOther(project)) return;
    analytics.setProject(project.name);
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function isOther(project: ProjectAnalytics): boolean {
    return project.name.startsWith("Other (");
  }

  function detailParts(project: ProjectAnalytics): string[] {
    const agents = Object.entries(project.agents)
      .sort(([, a], [, b]) => b - a)
      .map(([name, count]) => `${name}: ${count}`)
      .join(", ");
    const parts: string[] = [
      m.analytics_project_messages({
        count: project.messages,
        countLabel: formatCount(project.messages),
      }),
      m.analytics_project_sessions({
        count: project.sessions,
        countLabel: formatCount(project.sessions),
      }),
    ];
    if (agents) parts.push(agents);
    return parts;
  }

  // Hover and keyboard focus show the same details; screen readers get
  // them from the row text.
  function showTip(el: EventTarget | null, project: ProjectAnalytics) {
    const rect = (el as HTMLElement).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: detailParts(project).join(" | "),
    };
  }

  function handleLeave() {
    tooltip = null;
  }
</script>

{#snippet rowContent(project: ProjectAnalytics)}
  <span class="project-name" title={project.name} aria-hidden="true">
    {truncateName(project.name, 24)}
  </span>
  <span class="bar-track" aria-hidden="true">
    <span
      class="bar-fill"
      style="width: {barWidth(project.messages)}%"
    ></span>
  </span>
  <span class="bar-value" aria-hidden="true">
    {formatCount(project.messages)}
  </span>
  <span class="kit-sr-only">{[project.name, ...detailParts(project)].join(", ")}</span>
{/snippet}

<div class="breakdown-container">
  <div class="breakdown-header">
    <h3 class="chart-title">{m.analytics_projects_title()}</h3>
    {#if rows.length > 0}
      <span class="count">
        {m.analytics_total_count({
          countLabel: formatCount(analytics.projects?.projects.length ?? 0),
        })}
      </span>
    {/if}
  </div>

  <PanelStatus
    hasData={analytics.projects !== null}
    pending={analytics.querying.projects}
    error={analytics.errors.projects}
    onRetry={() => analytics.fetchProjects()}
  />
  {#if rows.length > 0}
    <div class="bar-list" role="list">
      {#each rows as project}
        <div
          role="listitem"
          onmouseenter={(e) => showTip(e.currentTarget, project)}
          onmouseleave={handleLeave}
        >
          {#if isOther(project)}
            <div
              class="bar-row"
              class:dimmed={analytics.project !== ""}
            >
              {@render rowContent(project)}
            </div>
          {:else}
            <button
              type="button"
              class="bar-row clickable"
              class:selected={analytics.project === project.name}
              class:dimmed={analytics.project !== "" && analytics.project !== project.name}
              aria-pressed={analytics.project === project.name}
              onclick={() => handleClick(project)}
              onfocus={(e) => showTip(e.currentTarget, project)}
              onblur={handleLeave}
            >
              {@render rowContent(project)}
            </button>
          {/if}
        </div>
      {/each}
    </div>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        <span>{tooltip.text}</span>
      </div>
    {/if}
  {:else if analytics.projects}
    <div class="empty">{m.analytics_projects_empty()}</div>
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
    font-size: 11px;
    color: var(--text-muted);
  }

  .bar-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .bar-row {
    display: flex;
    width: 100%;
    text-align: left;
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
    display: block;
    flex: 1;
    height: 14px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .bar-fill {
    display: block;
    height: 100%;
    background: var(--accent-blue);
    border-radius: 2px;
    min-width: 2px;
  }

  .bar-value {
    flex-shrink: 0;
    width: 52px;
    text-align: right;
    font-size: 11px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 4px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 11px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: var(--z-tooltip);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
