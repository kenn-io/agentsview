<script lang="ts">
  import { _ } from "svelte-i18n";
  import type { Report } from "../../api/types.js";

  let { report }: { report: Report } = $props();

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

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
        $_("activity.interactiveAutomatedSplit", { values: { interactive: fmtInt(t.interactive_sessions), automated: fmtInt(t.automated_sessions) } }),
      );
    }
    if (t.untimed_sessions > 0) {
      parts.push($_("activity.untimedCount", { values: { count: fmtInt(t.untimed_sessions) } }));
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
    return d.toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
  }

  const peakAt = $derived(fmtClock(report.peak.at));
  const asOf = $derived(fmtClock(report.as_of));

  interface Card {
    label: string;
    value: string;
    sub?: string;
    featured?: boolean;
  }

  const cards = $derived.by((): Card[] => {
    const t = report.totals;
    return [
      {
        label: $_("activity.peakConcurrency"),
        value: String(report.peak.agents),
        sub: peakAt ? $_("activity.atTime", { values: { time: peakAt } }) : "",
        featured: true,
      },
      {
        label: $_("activity.active"),
        value: fmtDuration(t.active_minutes),
        sub: $_("activity.idleDuration", { values: { duration: fmtDuration(t.idle_minutes) } }),
      },
      {
        label: $_("activity.agentMinutes"),
        // Round to a whole minute so the card shows "134", not "134.226";
        // matches the Breakdowns rounding of the same metric.
        value: fmtInt(Math.round(t.agent_minutes)),
      },
      {
        label: $_("activity.sessions"),
        value: fmtInt(t.sessions),
        sub: sessionsSub(t),
      },
      {
        label: $_("activity.projects"),
        value: fmtInt(t.distinct_projects),
      },
      {
        label: $_("activity.models"),
        value: fmtInt(t.distinct_models),
      },
      {
        label: $_("activity.totalCost"),
        value: fmtCost(t.cost),
      },
    ];
  });
</script>

<div class="summary-cards">
  {#each cards as card}
    <div class="card" class:featured={card.featured}>
      <span class="card-value">{card.value}</span>
      <span class="card-label">{card.label}</span>
      {#if card.sub}
        <span class="card-sub">{card.sub}</span>
      {/if}
    </div>
  {/each}
</div>

{#if report.partial && asOf}
  <div class="partial-note">{$_("activity.inProgressAsOf", { values: { time: asOf } })}</div>
{/if}

<style>
  .summary-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .card {
    flex: 1;
    min-width: 110px;
    padding: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .card.featured {
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
