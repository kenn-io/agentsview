<script lang="ts">
  import { Table, TableHeaderCell } from "@kenn-io/kit-ui";
  import { TriangleAlertIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { formatCost, formatCount } from "../../utils/format.js";
  import { keepInsidePane } from "./breadcrumbLayout.js";
  import type { SessionUsage, SessionUsageBreakdownEntry } from "./sessionUsage.svelte.js";

  // The usage figures in the session header: the per-step breakdown, token
  // totals, cost, and plan usage. The header owns the group around them,
  // which it shows only when `usage.hasStats` is true.
  interface Props {
    usage: SessionUsage;
  }

  let { usage }: Props = $props();

  function formatBreakdownContext(entry: SessionUsageBreakdownEntry): string {
    const context =
      entry.input_tokens +
      entry.cache_creation_input_tokens +
      entry.cache_read_input_tokens;
    return formatCount(Math.max(0, context));
  }

  function formatBreakdownTitle(entry: SessionUsageBreakdownEntry): string {
    const parts = [
      entry.model || entry.source,
      `${formatBreakdownContext(entry)} ctx`,
      `${formatCount(Math.max(0, entry.output_tokens))} out`,
    ];
    if (entry.has_cost) {
      parts.push(formatCost(entry.cost));
    }
    return parts.filter(Boolean).join(" · ");
  }
</script>

{#if usage.breakdownCount > 0}
  <!-- kit-ui-check-ignore: usage statistics open in a floating panel; kit-ui Disclosure expands inline and has no click popover equivalent. -->
  <details class="usage-breakdown" bind:open={usage.breakdownOpen}>
    <summary
      class="usage-breakdown-trigger"
      title={m.session_breadcrumb_usage_breakdown_title()}
    >
      {m.session_breadcrumb_usage_breakdown_steps({
        count: usage.breakdownCount,
        countLabel: formatCount(usage.breakdownCount),
      })}
    </summary>
    {#if usage.breakdownOpen}
      <div class="usage-breakdown-menu" {@attach keepInsidePane}>
        {#if usage.breakdownLoading}
          <div class="usage-breakdown-status">
            {m.session_breadcrumb_usage_breakdown_loading()}
          </div>
        {:else if usage.breakdown.length === 0}
          <div class="usage-breakdown-status">
            {m.session_breadcrumb_failed()}
          </div>
        {:else}
        {#each usage.breakdown as row (row.ordinal)}
          <div class="usage-breakdown-row" title={formatBreakdownTitle(row)}>
            <span class="usage-breakdown-label">
              {row.label}
            </span>
            <span class="usage-breakdown-model">
              {row.model || row.source}
            </span>
            <span class="usage-breakdown-tokens">
              {formatBreakdownContext(row)} ctx
              <span aria-hidden="true">/</span>
              {formatCount(Math.max(0, row.output_tokens))} out
            </span>
            {#if row.has_cost}
              <span class="usage-breakdown-cost">
                {formatCost(row.cost)}
              </span>
            {/if}
          </div>
        {/each}
        {/if}
      </div>
    {/if}
  </details>
{/if}
{#if usage.tokenSummary}
  <span class="token-badge token-badge--desktop">
    {usage.tokenSummary}
  </span>
  <span
    class="token-badge token-badge--mobile"
    title={usage.tokenSummary}
  >
    {usage.tokenSummary}
  </span>
{/if}
{#if usage.costLabel}
  <span class="cost-badge" title={usage.costTitle}>
    {#if usage.costIsRollup}
      {m.session_breadcrumb_total_cost()}: {usage.costLabel}
    {:else}
      {usage.costLabel}
    {/if}
  </span>
{/if}
{#if usage.planUsageDisplay}
  <!-- kit-ui-check-ignore: plan statistics open in a floating panel with a table and session link; kit-ui Disclosure expands inline and has no click popover equivalent. -->
  <details class="plan-usage" bind:open={usage.planUsageOpen}>
    <summary
      class="usage-breakdown-trigger plan-usage-trigger"
      class:plan-usage-trigger--empty={!usage.planUsageDisplay.hasFigures}
      title={usage.planUsageTitle}
    >
      {#if usage.planUsageWarnings.length > 0}
        <TriangleAlertIcon size="10" strokeWidth="2.2" aria-hidden="true" />
      {/if}
      {usage.planUsageDisplay.trigger}
      {#if usage.planUsageSrText}
        <span class="kit-sr-only">, {usage.planUsageSrText}</span>
      {/if}
    </summary>
    {#if usage.planUsageOpen}
      {@const rootId = usage.planUsageDisplay.countedUnder}
      {@const showFiveHour = usage.planUsageDisplay.groups.some((g) => g.fiveHour)}
      <div class="usage-breakdown-menu plan-usage-menu" {@attach keepInsidePane}>
        {#if rootId}
          <a
            class="plan-usage-headline plan-usage-root"
            href={router.buildSessionHref(rootId)}
            onclick={(event) => {
              event.preventDefault();
              usage.planUsageOpen = false;
              router.navigateToSession(rootId);
            }}
          >
            {usage.countedUnderLabel}
          </a>
        {:else}
          <div class="plan-usage-headline">{usage.planUsageDisplay.headline}</div>
        {/if}
        {#if usage.planUsageDisplay.staleNote}
          <div class="plan-usage-stale">{usage.planUsageDisplay.staleNote}</div>
        {/if}
        {#if usage.planUsageDisplay.totals.length > 0}
          <ul class="plan-usage-totals">
            {#each usage.planUsageDisplay.totals as line (line)}
              <li>{line}</li>
            {/each}
          </ul>
        {/if}
        {#if usage.planUsageDisplay.groups.length > 0}
          <Table unsorted
            class="plan-usage-groups"
            stickyHeader={false}
            ariaLabel={m.plan_usage_title()}
          >
            {#snippet header()}
              <TableHeaderCell label={m.plan_usage_col_split()} />
              <TableHeaderCell label={m.plan_usage_col_weekly()} numeric />
              {#if showFiveHour}
                <TableHeaderCell label={m.plan_usage_col_five_hour()} numeric />
              {/if}
              <TableHeaderCell label={m.plan_usage_col_credits()} numeric />
            {/snippet}
            {#each usage.planUsageDisplay.groups as group (group.key)}
              <tr>
                <td>{group.label}</td>
                <td class="plan-usage-num">{group.weekly}</td>
                {#if showFiveHour}
                  <td class="plan-usage-num">{group.fiveHour}</td>
                {/if}
                <td class="plan-usage-num">{group.credits}</td>
              </tr>
            {/each}
          </Table>
        {/if}
        {#if usage.planUsageDisplay.hasFigures}
          <p class="plan-usage-note">{m.plan_usage_credits_note()}</p>
        {/if}
        {#if usage.captureCoverageLines.length > 0}
          <div class="plan-usage-capture">
            {#each usage.captureCoverageLines as line (line)}
              <div>{line}</div>
            {/each}
          </div>
        {/if}
      </div>
    {/if}
  </details>
{/if}

<style>
  /* Figures: one 12px muted size with tabular numerals and no fill. The
     side padding only keeps their spacing. The header gives its own
     figures (date and model) the same look. */
  .token-badge,
  .cost-badge,
  .usage-breakdown-trigger {
    font-size: 12px;
    font-weight: 400;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    background: transparent;
    padding: 0 4px;
    white-space: nowrap;
  }

  .token-badge {
    flex-shrink: 0;
  }

  .token-badge--mobile {
    display: none;
    white-space: nowrap;
  }

  .cost-badge {
    flex-shrink: 0;
  }

  .usage-breakdown,
  .plan-usage {
    position: relative;
    flex-shrink: 0;
  }

  .plan-usage-trigger {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }

  .plan-usage-trigger--empty {
    font-style: italic;
  }

  .plan-usage-menu {
    width: min(420px, calc(100vw - 24px));
    max-height: 360px;
    padding: 8px 10px;
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .plan-usage-headline {
    font-weight: 600;
    color: var(--text-primary);
  }

  .plan-usage-root {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .plan-usage-root:hover {
    text-decoration: underline;
  }

  .plan-usage-stale {
    color: var(--accent-amber);
  }

  .plan-usage-totals {
    margin: 0;
    padding: 0;
    list-style: none;
    font-variant-numeric: tabular-nums;
  }

  .plan-usage-menu :global(.plan-usage-groups) {
    flex: none;
    border: 1px solid var(--border-muted);
    border-radius: 4px;
  }

  .plan-usage-num {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .plan-usage-note,
  .plan-usage-capture {
    margin: 0;
    color: var(--text-muted);
    line-height: 1.4;
  }

  .plan-usage-capture {
    padding-top: 6px;
    border-top: 1px solid var(--border-muted);
  }

  /* Usage menus open from plain figures; a dotted underline marks them as
     triggers without adding a chip. */
  .usage-breakdown-trigger {
    list-style: none;
    cursor: pointer;
    text-decoration: underline dotted;
    text-decoration-color: color-mix(in srgb, currentColor 45%, transparent);
    text-underline-offset: 3px;
  }

  .usage-breakdown-trigger::-webkit-details-marker {
    display: none;
  }

  .usage-breakdown[open] .usage-breakdown-trigger,
  .plan-usage[open] .usage-breakdown-trigger,
  .usage-breakdown-trigger:hover {
    color: var(--text-secondary);
    text-decoration-color: currentColor;
  }

  .usage-breakdown-menu {
    position: absolute;
    top: 100%;
    right: 0;
    margin-top: 4px;
    width: min(460px, calc(100vw - 24px));
    max-height: 260px;
    overflow: auto;
    padding: 6px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    box-shadow: var(--shadow-lg);
    z-index: var(--z-popover);
  }

  .usage-breakdown-status {
    padding: 5px 6px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .usage-breakdown-row {
    display: grid;
    grid-template-columns: minmax(62px, 0.85fr) minmax(90px, 1fr) max-content max-content;
    gap: 8px;
    align-items: center;
    padding: 5px 6px;
    border-radius: 4px;
    font-size: 11px;
    line-height: 1.25;
    color: var(--text-secondary);
  }

  .usage-breakdown-row:hover {
    background: var(--bg-surface-hover);
  }

  .usage-breakdown-label,
  .usage-breakdown-model {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .usage-breakdown-label {
    color: var(--text-primary);
    font-weight: 500;
  }

  .usage-breakdown-model {
    color: var(--text-muted);
  }

  .usage-breakdown-tokens,
  .usage-breakdown-cost {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .usage-breakdown-cost {
    color: var(--text-muted);
  }


  @media (min-width: 641px) {
    /* The metadata row's figures share the chips' 16px line, so the
       second row costs 18px instead of 20px. */
    .token-badge,
    .cost-badge,
    .usage-breakdown-trigger {
      line-height: 16px;
    }
  }

  @media (max-width: 900px) {
    .token-badge--desktop {
      display: none;
    }

    .token-badge--mobile {
      display: inline-flex;
      font-size: 9px;
      padding: 1px 4px;
      max-width: 110px;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .usage-breakdown-trigger {
      font-size: 9px;
      padding: 1px 4px;
    }

    .usage-breakdown-menu {
      right: -54px;
      width: min(360px, calc(100vw - 16px));
    }

    .usage-breakdown-row {
      grid-template-columns: minmax(56px, 0.8fr) minmax(68px, 1fr) max-content;
      gap: 6px;
      font-size: 10px;
    }

    .usage-breakdown-cost {
      display: none;
    }

    .usage-breakdown {
      display: none;
    }

    /* The meta row scrolls sideways here, so a menu anchored to its
       trigger can open past the screen edge; span the viewport instead. */
    .plan-usage-menu {
      position: fixed;
      top: auto;
      left: 8px;
      right: 8px;
      width: auto;
    }
  }
</style>
