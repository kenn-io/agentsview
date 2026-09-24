<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import type { FrictionSummary } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { formatUSD, personaRows, sortedCostEntries } from "./frictionView.js";

  interface Props {
    summary: FrictionSummary;
  }

  let { summary }: Props = $props();

  const personas = $derived(personaRows(summary.personas));
  const spend = $derived(summary.spend ?? null);
  const archive = $derived(summary.archive_spend ?? null);
</script>

{#snippet costList(title: string, costs: Record<string, string> | null | undefined)}
  {@const entries = sortedCostEntries(costs)}
  {#if entries.length > 0}
    <h3>{title}</h3>
    <ul class="cost-list">
      {#each entries as [key, value] (key)}
        <li><code>{key}</code> {formatUSD(value)}</li>
      {/each}
    </ul>
  {/if}
{/snippet}

{#if personas.length > 0}
  <section class="friction-personas" aria-labelledby="friction-personas-title">
    <h2 id="friction-personas-title">{m.friction_section_personas()}</h2>
    <div class="persona-list">
      {#each personas as row (row.key)}
        <Card level="inset" padding="none" class="persona-row">
          <div class="persona-content">
            <code>{row.key}</code>
            <span>{m.friction_pattern_sessions({ count: row.value.sessions })}</span>
            <span>{m.friction_section_corrections()} <strong>{row.value.corrections}</strong></span>
            <span>{m.friction_section_errors()} <strong>{row.value.errors}</strong></span>
            <span>{m.friction_section_workarounds()} <strong>{row.value.workarounds}</strong></span>
            <span>{m.friction_section_deferrals()} <strong>{row.value.deferrals}</strong></span>
            <span>{m.friction_section_patterns()} <strong>{row.value.patterns}</strong></span>
            {#if row.value.input_tokens > 0 || row.value.output_tokens > 0}
              <span>
                {m.friction_spend_tokens({ input: row.value.input_tokens, output: row.value.output_tokens })}
              </span>
            {/if}
            {#if formatUSD(row.value.cost_usd)}
              <span>{formatUSD(row.value.cost_usd)}</span>
            {/if}
          </div>
        </Card>
      {/each}
    </div>
  </section>
{/if}

{#if spend || archive}
  <section class="friction-spend" aria-labelledby="friction-spend-title">
    <h2 id="friction-spend-title">{m.friction_section_spend()}</h2>
    {#if spend}
      <p>
        {m.friction_spend_total()}:
        <strong>{formatUSD(spend.total_usd) ?? m.friction_spend_no_cost()}</strong>
        · {m.friction_spend_coverage({ count: spend.sessions_with_stats, withCost: spend.sessions_with_cost })}
      </p>
      <p>{m.friction_spend_tokens({ input: spend.input_tokens, output: spend.output_tokens })}</p>
      {@render costList(m.friction_spend_by_role(), spend.role_costs_usd)}
      {@render costList(m.friction_spend_by_model(), spend.model_costs_usd)}
    {/if}
    {#if archive}
      <h3>{m.friction_archive_title()}</h3>
      <p>
        {m.friction_archive_yesterday({ date: archive.week_to })}:
        {archive.yesterday ? formatUSD(archive.yesterday.total_usd) : m.friction_archive_no_rows()}
      </p>
      <p>
        {m.friction_archive_week({ from: archive.week_from, to: archive.week_to })}:
        {formatUSD(archive.week.total_usd)} · {archive.timezone}
      </p>
      {@render costList(m.friction_spend_by_model(), archive.week.models_usd)}
    {/if}
  </section>
{/if}

<style>
  .friction-personas,
  .friction-spend {
    display: flex;
    flex-direction: column;
    gap: 8px;
    font-size: 12px;
  }
  h2 {
    margin: 0;
    font-size: 14px;
  }
  h3 {
    margin: 4px 0 0;
    font-size: 13px;
  }
  p {
    margin: 0;
  }
  .persona-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .persona-content {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-5);
    padding: 8px 10px;
    align-items: baseline;
  }
  .cost-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
</style>
