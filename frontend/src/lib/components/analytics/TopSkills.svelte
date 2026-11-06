<script lang="ts">
  import { SvelteSet } from "svelte/reactivity";
  import { formatCount } from "../../utils/format.js";
  import { Chip, Table, TableHeaderCell } from "@kenn-io/kit-ui";
  import { ChevronRightIcon } from "../../icons.js";
  import { analytics } from "../../stores/analytics.svelte.js";
  import type { DbSkillAgentBreakdown as SkillAgentBreakdown, DbSkillUsage as SkillUsage } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";

  const uid = $props.id();
  const skills = $derived(analytics.skills?.by_skill ?? []);

  // Skills whose agent and project breakdown row is open, by name, so a
  // refresh that reorders the list keeps the same skills open.
  const expanded = new SvelteSet<string>();

  function toggle(name: string) {
    if (expanded.has(name)) expanded.delete(name);
    else expanded.add(name);
  }

  // Share is the skill's part of all skill calls, so the full track is
  // 100% and the bar agrees with its tooltip.
  function barWidth(pct: number): number {
    return Math.min(100, Math.max(0, pct));
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
        countLabel: formatCount(skill.call_count),
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
          callsLabel: formatCount(analytics.skills.total_skill_calls),
          skills: analytics.skills.distinct_skills,
          skillsLabel: formatCount(analytics.skills.distinct_skills),
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
    <Table bare unsorted class="skills-table" ariaLabel={m.analytics_top_skills_title()}>
      <thead>
        <tr>
          <TableHeaderCell label={m.analytics_top_skills_col_skill()} class="col-skill" />
          <TableHeaderCell label={m.analytics_top_skills_col_share()} class="col-share" />
          <TableHeaderCell label={m.analytics_top_skills_col_calls()} numeric />
          <TableHeaderCell label={m.analytics_top_skills_col_sessions()} numeric />
          <TableHeaderCell label={m.analytics_top_skills_col_last_used()} numeric />
        </tr>
      </thead>
      <tbody>
        {#each skills.slice(0, 8) as skill, index (skill.skill_name)}
          {@const open = expanded.has(skill.skill_name)}
          {@const detailId = `${uid}-skill-${index}`}
          <tr class="skill-row" class:open>
            <th scope="row" class="skill-cell">
              <button
                type="button"
                class="disclosure"
                aria-expanded={open}
                aria-controls={detailId}
                onclick={() => toggle(skill.skill_name)}
              >
                <ChevronRightIcon size={12} class="chevron" aria-hidden="true" />
                <span class="skill-name">{skill.skill_name}</span>
              </button>
            </th>
            <td class="share-cell">
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <span
                class="bar-track"
                onmouseenter={(e) => handleSkillHover(e, skill)}
                onmouseleave={handleLeave}
              >
                <span
                  class="bar-fill"
                  style="width: {barWidth(skill.pct)}%"
                ></span>
              </span>
            </td>
            <td class="num">{formatCount(skill.call_count)}</td>
            <td class="num">{formatCount(skill.session_count)}</td>
            <td class="num last-used">{formatLastUsed(skill.last_used_at)}</td>
          </tr>
          <tr id={detailId} class="detail-row" hidden={!open}>
            <td colspan="5">
              <div class="breakdowns">
                <div class="breakdown">
                  <span class="breakdown-label" id="{detailId}-agents">
                    {m.analytics_top_skills_agents()}
                  </span>
                  {#if skill.agent_breakdown?.length}
                    <ul class="breakdown-items" aria-labelledby="{detailId}-agents">
                      {#each skill.agent_breakdown as agent (agent.agent)}
                        <li>
                          <Chip size="xs" uppercase={false} class="breakdown-chip" title={agent.agent}>
                            {agent.agent}
                            {#snippet trailing()}
                              <span class="item-count">{formatCount(agent.count)}</span>
                              <span class="item-pct">
                                {agentPct(agent, skill.call_count)}
                              </span>
                            {/snippet}
                          </Chip>
                        </li>
                      {/each}
                    </ul>
                  {:else}
                    <span class="muted">{m.shared_none()}</span>
                  {/if}
                </div>
                <div class="breakdown">
                  <span class="breakdown-label" id="{detailId}-projects">
                    {m.analytics_top_skills_projects_label()}
                  </span>
                  {#if skill.project_breakdown?.length}
                    <ul class="breakdown-items" aria-labelledby="{detailId}-projects">
                      {#each skill.project_breakdown as project (project.project)}
                        <li>
                          <Chip size="xs" uppercase={false} class="breakdown-chip" title={project.project}>
                            {project.project}
                            {#snippet trailing()}
                              <span class="item-count">{formatCount(project.count)}</span>
                            {/snippet}
                          </Chip>
                        </li>
                      {/each}
                    </ul>
                  {:else}
                    <span class="muted">{m.shared_none()}</span>
                  {/if}
                </div>
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </Table>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        <span>{tooltip.text}</span>
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
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .skills-container :global(.skills-table) {
    width: 100%;
    border-collapse: collapse;
    table-layout: fixed;
    font-size: 11px;
  }

  /* Header row: kit-ui header cells on the card surface, with a hairline
     under them like the panel's other mini tables. */
  .skills-container :global(.skills-table .kit-th) {
    padding: 4px 6px;
    background: transparent;
    box-shadow: inset 0 -1px 0 var(--border-muted);
    font-weight: 500;
    /* Longer translated labels ("Dernière utilisation") wrap within their
       column instead of running into the next header. */
    white-space: normal;
    vertical-align: bottom;
  }

  .skills-container :global(.skills-table .kit-th.col-skill) {
    width: 30%;
    /* Lines the header up with the name after the disclosure chevron. */
    padding-inline-start: 26px;
  }

  .skills-container :global(.skills-table .kit-th.col-share) {
    width: auto;
  }

  .skills-container :global(.skills-table .kit-th.kit-th--numeric) {
    width: 76px;
  }

  .skill-row > th,
  .skill-row > td {
    padding: 3px 6px;
    border-bottom: 1px solid var(--border-muted);
    vertical-align: middle;
  }

  .skill-row.open > th,
  .skill-row.open > td {
    border-bottom-color: transparent;
  }

  .skill-row:hover {
    background: var(--bg-surface-hover);
  }

  .skill-cell {
    padding-inline-start: 2px;
    font-weight: 400;
    text-align: left;
  }

  .disclosure {
    display: flex;
    align-items: center;
    gap: 4px;
    width: 100%;
    min-width: 0;
    padding: 2px 4px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }

  .disclosure:hover {
    color: var(--text-primary);
  }

  .disclosure :global(.chevron) {
    flex-shrink: 0;
    color: var(--text-muted);
    transition: transform 0.12s ease;
  }

  .disclosure[aria-expanded="true"] :global(.chevron) {
    transform: rotate(90deg);
  }

  .skill-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .bar-track {
    display: block;
    height: 10px;
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

  .num {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    text-align: right;
    white-space: nowrap;
  }

  .detail-row > td {
    padding: 2px 6px 8px 26px;
    border-bottom: 1px solid var(--border-muted);
  }

  .breakdowns {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
    color: var(--text-secondary);
  }

  .breakdown {
    display: flex;
    align-items: baseline;
    gap: 8px;
    min-width: 0;
  }

  .breakdown-label {
    flex: 0 0 64px;
    color: var(--text-muted);
  }

  .breakdown-items {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    min-width: 0;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  :global(.breakdown-chip.kit-chip) {
    max-width: 200px;
    color: var(--text-secondary);
  }

  /* Count and share sit in the chip's trailing slot, outside its
     truncating label, so a long name never hides them. */
  .item-pct {
    margin-inline-start: 4px;
  }

  .item-count {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .item-pct,
  .muted {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
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
