<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import { formatMoney } from "../../money.js";

  let { report }: { report: Report } = $props();

  function fmtInt(v: number): string {
    return v.toLocaleString();
  }

  // Sessions card detail line: surface the automation split only when there
  // are automated sessions, so the common all-interactive view stays clean,
  // and keep the untimed count. interactive + automated == sessions.
  function sessionsSub(t: Report["totals"]): string {
    const parts: string[] = [];
    if (t.automated_sessions > 0) {
      parts.push(
        m.activity_interactive_automated_split({ interactive: fmtInt(t.interactive_sessions), automated: fmtInt(t.automated_sessions) }),
      );
    }
    if (t.untimed_sessions > 0) {
      parts.push(m.activity_untimed_count({ count: fmtInt(t.untimed_sessions) }));
    }
    return parts.join(", ");
  }

  // minutes -> "Hh Mm" (e.g. 75 -> "1h 15m"). Sub-hour durations
  // drop the hour segment; whole minutes only.
  function fmtDuration(mins: number): string {
    const total = Math.max(Math.round(mins), 0);
    const h = Math.floor(total / 60);
    const m = total % 60;
    if (h === 0) return `${m}m`;
    return `${h}h ${m}m`;
  }

  // RFC3339 -> "HH:MM" in the viewer's local zone. The report's
  // day window is already local-timezone-aligned server-side, so
  // local formatting keeps the clock label consistent with it.
  function fmtClock(ts: string | null): string {
    if (!ts) return "";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "";
    return formatDateTime(d, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
  }

  const peakAt = $derived(fmtClock(report.peak.at));
  const asOf = $derived(fmtClock(report.as_of));

  interface SummaryCard {
    label: string;
    value: string;
    sub?: string;
    featured?: boolean;
  }

  const cards = $derived.by((): SummaryCard[] => {
    const t = report.totals;
    return [
      {
        label: m.activity_peak_concurrency(),
        value: String(report.peak.agents),
        sub: peakAt ? m.activity_at_time({ time: peakAt }) : "",
        featured: true,
      },
      {
        label: m.activity_active(),
        value: fmtDuration(t.active_minutes),
        sub: m.activity_idle_duration({ duration: fmtDuration(t.idle_minutes) }),
      },
      {
        label: m.activity_agent_minutes(),
        // Round to a whole minute so the card shows "134", not "134.226";
        // matches the Breakdowns rounding of the same metric.
        value: fmtInt(Math.round(t.agent_minutes)),
      },
      {
        label: m.activity_sessions(),
        value: fmtInt(t.sessions),
        sub: sessionsSub(t),
      },
      {
        label: m.activity_projects(),
        value: fmtInt(t.distinct_projects),
      },
      {
        label: m.activity_models(),
        value: fmtInt(t.distinct_models),
      },
      {
        label: m.activity_total_cost(),
        value: formatMoney(t.cost),
      },
    ];
  });
</script>

<div class="summary-cards">
  {#each cards as card}
    <Card
      level="default"
      padding="none"
      class={card.featured ? "card featured" : "card"}
    >
      <span class="card-value">{card.value}</span>
      <span class="card-label">{card.label}</span>
      {#if card.sub}
        <span class="card-sub">{card.sub}</span>
      {/if}
    </Card>
  {/each}
</div>

{#if report.partial && asOf}
  <div class="partial-note">{m.activity_in_progress_as_of({ time: asOf })}</div>
{/if}

<style>
  .summary-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .summary-cards :global(.card) {
    flex: 1;
    min-width: 110px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .summary-cards :global(.card > .kit-card__body) {
    display: contents;
  }

  .summary-cards :global(.card.featured) {
    border-width: 2px;
    border-color: var(--accent-blue);
  }

  .card-value {
    font-size: 20px;
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.2;
  }

  .card-label {
    font-size: 11px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .card-sub {
    font-size: 10px;
    color: var(--text-muted);
    margin-top: 2px;
  }

  .partial-note {
    margin-top: 8px;
    font-size: 11px;
    color: var(--accent-amber);
  }
</style>
