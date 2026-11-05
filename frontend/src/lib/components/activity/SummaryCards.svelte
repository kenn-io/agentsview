<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import StatCard from "../analytics/StatCard.svelte";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import type { Report } from "../../api/types.js";
  import { formatMoney } from "../../money.js";

  let { report }: { report: Report } = $props();

  function fmtMinutes(v: number): string {
    return Math.round(v).toLocaleString(getLocale());
  }

  // Subagents are separate from interactive and automated conversation counts.
  function sessionsSub(t: Report["totals"]): string {
    const parts: string[] = [];
    if (t.subagent_sessions > 0) {
      parts.push(
        m.activity_session_kind_split({
          interactive: formatCount(t.interactive_sessions),
          subagents: t.subagent_sessions,
          subagentsLabel: formatCount(t.subagent_sessions),
          automated: formatCount(t.automated_sessions),
        }),
      );
    } else if (t.automated_sessions > 0) {
      parts.push(
        m.activity_interactive_automated_split({ interactive: formatCount(t.interactive_sessions), automated: formatCount(t.automated_sessions) }),
      );
    }
    if (t.untimed_sessions > 0) {
      parts.push(m.activity_untimed_count({ count: formatCount(t.untimed_sessions) }));
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

  // Keep the clock label in the same timezone as the report and timeline.
  function fmtClock(ts: string | null): string {
    if (!ts) return "";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "";
    return formatDateTime(d, {
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
      timeZone: report.timezone,
    });
  }

  const peakAt = $derived(fmtClock(report.interactive_peak.at));

  interface SummaryCard {
    label: string;
    value: string;
    sub?: string;
    featured?: boolean;
    context?: boolean;
  }

  const cards = $derived.by((): SummaryCard[] => {
    const t = report.totals;
    return [
      {
        label: m.activity_interactive_peak(),
        value: formatCount(report.interactive_peak.agents),
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
        value: fmtMinutes(t.agent_minutes),
      },
      {
        label: m.activity_sessions(),
        value: formatCount(t.sessions),
        sub: sessionsSub(t),
      },
      {
        label: m.activity_projects(),
        value: formatCount(t.distinct_projects),
        context: true,
      },
      {
        label: m.activity_models(),
        value: formatCount(t.distinct_models),
        context: true,
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
    <StatCard
      value={card.value}
      label={card.label}
      sub={card.sub ?? ""}
      tier={card.featured ? "lead" : card.context ? "context" : "standard"}
      featured={card.featured}
    />
  {/each}
</div>

<style>
  .summary-cards {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
</style>
