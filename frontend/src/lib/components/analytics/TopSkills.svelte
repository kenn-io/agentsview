<script lang="ts">
  import { Chip } from "@kenn-io/kit-ui";
  import { analytics } from "../../stores/analytics.svelte.js";
  import type {
    SkillAgentBreakdown,
    SkillProjectBreakdown,
    SkillUsage,
  } from "../../api/types.js";
  import { m } from "../../i18n/index.js";

  const skills = $derived(analytics.skills?.by_skill ?? []);

  const maxCount = $derived(
    skills.length > 0
      ? Math.max(...skills.map((s) => s.call_count), 1)
      : 1,
  );

  function barWidth(count: number): number {
    return (count / maxCount) * 100;
  }

  function formatLastUsed(value: string): string {
    if (!value) return m.analytics_top_skills_never();
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return value;
    return d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  }

  function projectBreakdownLabel(
    items: SkillProjectBreakdown[] | null,
  ): string {
    const top = (items ?? []).slice(0, 2);
    if (top.length === 0) return m.shared_none();
    return top
      .map((item) => {
        return `${item.project}: ${item.count}`;
      })
      .join(", ");
  }

  function agentPct(item: SkillAgentBreakdown, total: number): string {
    if (total <= 0) return "0%";
    const pct = (item.count / total) * 100;
    return `${pct >= 10 ? pct.toFixed(0) : pct.toFixed(1)}%`;
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function showTooltip(e: MouseEvent, text: string) {
    const rect = (
      e.currentTarget as HTMLElement
    ).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text,
    };
  }

  function handleSkillHover(e: MouseEvent, skill: SkillUsage) {
    showTooltip(
      e,
      m.analytics_top_skills_tooltip({
        skill: skill.skill_name,
        count: skill.call_count,
        countLabel: skill.call_count.toLocaleString(),
        pct: skill.pct,
      }),
    );
  }

  function handleLeave() {
    tooltip = null;
  }
</script>

<div class="skills-container">
  <div class="skills-header">
    <h3 class="chart-title">{m.analytics_top_skills_title()}</h3>
    {#if analytics.skills}
      <span class="count">
        {m.analytics_top_skills_count({
          calls: analytics.skills.total_skill_calls,
          callsLabel: analytics.skills.total_skill_calls.toLocaleString(),
          skills: analytics.skills.distinct_skills,
          skillsLabel: analytics.skills.distinct_skills.toLocaleString(),
        })}
      </span>
    {/if}
  </div>

  {#if analytics.errors.skills}
    <div class="error">
      {analytics.errors.skills}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchSkills()}
      >
        {m.shared_retry()}
      </button>
    </div>
  {:else if skills.length > 0}
    <div class="skill-list">
      {#each skills.slice(0, 8) as skill}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="skill-row"
          onmouseenter={(e) => handleSkillHover(e, skill)}
          onmouseleave={handleLeave}
        >
          <span class="skill-name">{skill.skill_name}</span>
          <span class="bar-track">
            <span
              class="bar-fill"
              style="width: {barWidth(skill.call_count)}%"
            ></span>
          </span>
          <span class="bar-value">
            {skill.call_count.toLocaleString()}
          </span>
          <span class="session-value">
            {m.analytics_session_shape_session_count({
              count: skill.session_count,
              countLabel: skill.session_count.toLocaleString(),
            })}
          </span>
          <span class="last-used">
            {formatLastUsed(skill.last_used_at)}
          </span>
        </div>
        <div class="breakdowns">
          <div class="agent-breakdown" aria-label={m.analytics_top_skills_agent_breakdown()}>
            <span class="breakdown-label">{m.analytics_top_skills_agents()}</span>
            {#if skill.agent_breakdown?.length}
              {#each skill.agent_breakdown as agent}
                <Chip size="xs" uppercase={false} class="agent-chip">
                  <span class="agent-name">{agent.agent}</span>
                  <span class="agent-count">{agent.count.toLocaleString()}</span>
                  <span class="agent-pct">
                    {agentPct(agent, skill.call_count)}
                  </span>
                </Chip>
              {/each}
            {:else}
              <span class="muted">{m.shared_none()}</span>
            {/if}
          </div>
          <span class="project-breakdown">
            {m.analytics_top_skills_projects({
              projects: projectBreakdownLabel(skill.project_breakdown),
            })}
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
    <div class="empty">{m.analytics_top_skills_empty()}</div>
  {/if}
</div>

<style>
  .skills-container {
    position: relative;
    flex: 1;
  }

  .skills-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
    gap: 12px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .count {
    font-size: 10px;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .skill-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .skill-row {
    display: grid;
    grid-template-columns: minmax(84px, 1.4fr) minmax(48px, 1fr) 52px 72px 44px;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 3px 4px;
    border-radius: var(--radius-sm);
    background: transparent;
    color: inherit;
    text-align: left;
    transition: background 0.1s;
  }

  .skill-row:hover {
    background: var(--bg-surface-hover);
  }

  .skill-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .bar-track {
    height: 12px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .bar-fill {
    display: block;
    height: 100%;
    border-radius: 2px;
    min-width: 2px;
    background: var(--accent-green, #10b981);
  }

  .bar-value,
  .session-value,
  .last-used {
    font-size: 10px;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .bar-value {
    text-align: right;
    font-family: var(--font-mono);
  }

  .breakdowns {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-5);
    min-width: 0;
    padding: 0 4px 6px;
    color: var(--text-muted);
    font-size: 9px;
  }

  .agent-breakdown {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 4px;
    min-width: 0;
  }

  .breakdown-label {
    font-weight: 600;
    color: var(--text-secondary);
  }

  :global(.agent-chip.kit-chip) {
    max-width: 160px;
    color: var(--text-secondary);
  }

  .agent-name,
  .project-breakdown {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .agent-count {
    font-family: var(--font-mono);
    color: var(--text-primary);
  }

  .agent-pct,
  .muted {
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
    z-index: var(--z-tooltip);
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
