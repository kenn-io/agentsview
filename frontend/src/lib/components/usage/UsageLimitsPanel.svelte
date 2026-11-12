<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { UsageService, type DbRateLimitSnapshot } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { usage } from "../../stores/usage.svelte.js";
  import { rateLimitGroups } from "../../utils/rateLimits.js";

  let snapshots: DbRateLimitSnapshot[] = $state([]);

  // Reload with the rest of the page: on mount and on every usage refresh.
  $effect(() => {
    void usage.lastUpdatedAt;
    const controller = new AbortController();
    UsageService.getApiV1UsageLimits({ signal: controller.signal }).then(
      (response) => {
        snapshots = response.limits ?? [];
      },
      (err: unknown) => {
        // The panel is a supplement to the page; a server without the route
        // or a failed read leaves it hidden rather than adding an error.
        if (!isAbortError(err)) snapshots = [];
      },
    );
    return () => controller.abort();
  });

  const groups = $derived(rateLimitGroups(snapshots));
</script>

{#if groups.length > 0}
  <Card level="default" padding="none" class="chart-panel wide">
    <section class="limits-panel" aria-labelledby="usage-limits-title">
      <h3 class="chart-title" id="usage-limits-title">{m.usage_limits_title()}</h3>
      <div class="limit-groups">
        {#each groups as group (group.key)}
          <div class="limit-group">
            <div class="group-head">
              <span class="group-title">{group.title}</span>
              <span class="group-reported">{group.reported}</span>
            </div>
            {#each group.windows as row (row.key)}
              <div class="window-row" class:reset-since={row.resetSince}>
                <span class="window-label">{row.label}</span>
                <div
                  class="window-track"
                  role="meter"
                  aria-valuemin="0"
                  aria-valuemax="100"
                  aria-valuenow={row.percent}
                  aria-label={m.usage_limits_meter_label({ window: row.label, used: row.usedText })}
                >
                  <div class="window-fill" style="width: {row.percent}%;"></div>
                </div>
                <span class="window-used">{row.usedText}</span>
                <span class="window-reset">{row.resetText}</span>
              </div>
            {/each}
          </div>
        {/each}
      </div>
    </section>
  </Card>
{/if}

<style>
  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .limit-groups {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
    gap: 12px 24px;
  }

  .limit-group {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
  }

  .group-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    font-size: 11px;
  }

  .group-title {
    color: var(--text-secondary);
    font-weight: 600;
  }

  .group-reported {
    color: var(--text-muted);
    white-space: nowrap;
  }

  .window-row {
    display: grid;
    grid-template-columns: 96px minmax(60px, 1fr) auto;
    grid-template-areas:
      "label track used"
      ". reset reset";
    align-items: center;
    column-gap: 8px;
    font-size: 11px;
  }

  .window-label {
    grid-area: label;
    color: var(--text-secondary);
  }

  .window-track {
    grid-area: track;
    height: 8px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .window-fill {
    height: 100%;
    background: var(--accent-blue);
    border-radius: var(--radius-sm);
  }

  .window-used {
    grid-area: used;
    min-width: 64px;
    text-align: right;
    font-family: var(--font-mono);
    color: var(--text-primary);
  }

  .window-reset {
    grid-area: reset;
    color: var(--text-muted);
  }

  .window-reset:empty {
    display: none;
  }

  .reset-since .window-fill {
    background: var(--text-muted);
  }

  .reset-since .window-used {
    color: var(--text-muted);
  }
</style>
