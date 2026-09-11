<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { formatDateTime, m } from "../../i18n/index.js";
  import {
    rateLimits,
    type RateLimitWindow,
  } from "../../stores/ratelimits.svelte.js";
  import { formatResetCountdown, formatWindowLength } from "../../utils/rateLimitFormat.js";
  import RateLimitHistoryChart from "./RateLimitHistoryChart.svelte";

  interface Props {
    window: RateLimitWindow;
    since: string;
    until: string;
  }

  let { window: snapshot, since, until }: Props = $props();

  let now = $state(Date.now());
  let timer: ReturnType<typeof setInterval> | undefined;

  onMount(() => {
    timer = setInterval(() => {
      now = Date.now();
    }, 60_000);
  });
  onDestroy(() => {
    if (timer !== undefined) clearInterval(timer);
  });

  const identity = $derived({
    vendor: snapshot.vendor,
    accountId: snapshot.accountId ?? "",
    machine: snapshot.machine ?? "",
    limitId: snapshot.limitId ?? "",
    windowKind: snapshot.windowKind,
  });

  $effect(() => {
    // Re-fetch this card's history when its full identity (machine,
    // limit id, window kind) or the selected date range changes,
    // and also whenever current snapshots refresh (mount, the Usage
    // page's manual refresh, or its periodic auto-refresh) so the chart
    // picks up newly synced observations instead of only the card's own
    // used-percent/credits fields updating.
    const id = identity;
    void rateLimits.refreshToken;
    void since;
    void until;
    rateLimits.fetchHistory(id, since, until);
  });

  const usedPercent = $derived(Math.max(0, Math.min(100, snapshot.usedPercent)));

  const barColor = $derived(
    usedPercent >= 90
      ? "var(--accent-red)"
      : usedPercent >= 70
        ? "var(--accent-amber)"
        : "var(--accent-blue)",
  );

  // resetsAt is omitted from the API response (rather than sent as 0)
  // when the vendor did not report a reset time for this window, so a
  // missing value is genuinely unknown; any finite number, including 0,
  // is a real reset instant (a past one renders as "Resets now").
  const hasKnownReset = $derived(
    typeof snapshot.resetsAt === "number" && Number.isFinite(snapshot.resetsAt),
  );
  const resetCountdown = $derived(
    hasKnownReset ? formatResetCountdown(snapshot.resetsAt!, now) : null,
  );
  const resetAbsolute = $derived(
    hasKnownReset
      ? formatDateTime(snapshot.resetsAt! * 1000, {
          dateStyle: "medium",
          timeStyle: "short",
        })
      : "",
  );

  const history = $derived(rateLimits.historyFor(identity));

  // Window label composed via Paraglide like the other rate-limit labels
  // (rate_limits_resets_in, etc.): a 7-day (10080-minute) window reads as
  // "Weekly limit" the way Codex's own UI describes it; any other known
  // length falls back to a generic "{duration} limit". windowMinutes is
  // omitted from the API response when Codex never reported a duration
  // for this window (see ServiceRateLimitWindow.windowMinutes) -- that
  // case falls back to a window-kind label ("Session limit" for
  // "primary", "Weekly limit" for "secondary") instead of a generic
  // "{duration} limit" with a placeholder "—" duration. limit_name is the
  // vendor-reported, human-readable name (e.g. "GPT-5.3-Codex-Spark");
  // limit_id (a slot name like "codex") is the fallback for older/partial
  // snapshots that never carried a name.
  const windowLabel = $derived(
    snapshot.windowMinutes === undefined
      ? snapshot.windowKind === "primary"
        ? m.rate_limits_window_session()
        : m.rate_limits_window_weekly()
      : snapshot.windowMinutes === 10080
        ? m.rate_limits_window_weekly()
        : m.rate_limits_window_generic({ duration: formatWindowLength(snapshot.windowMinutes) }),
  );
  const cardHeader = $derived(
    m.rate_limits_card_header({
      window: windowLabel,
      name: snapshot.limitName || snapshot.limitId,
    }),
  );
</script>

<div class="rate-limit-card">
  <div class="card-header">
    <span class="limit-id">{cardHeader}</span>
    <span class="window-length">{formatWindowLength(snapshot.windowMinutes)}</span>
  </div>

  <div
    class="progress-track"
    role="progressbar"
    aria-label={m.rate_limits_used_percent_aria()}
    aria-valuemin={0}
    aria-valuemax={100}
    aria-valuenow={Math.round(usedPercent)}
  >
    <div
      class="progress-fill"
      style:width={`${usedPercent}%`}
      style:background={barColor}
    ></div>
  </div>
  <div class="progress-stats">
    <span class="used-percent" style:color={barColor}>
      {usedPercent.toLocaleString(undefined, { maximumFractionDigits: 1 })}%
    </span>
    <span class="resets" title={resetAbsolute}>
      {#if !hasKnownReset}
        {m.rate_limits_resets_unknown()}
      {:else if resetCountdown !== null}
        {m.rate_limits_resets_in({ value: resetCountdown })}
      {:else}
        {m.rate_limits_resets_now()}
      {/if}
    </span>
  </div>

  <div class="meta-row">
    {#if snapshot.planType}
      <span class="meta-item">
        <span class="meta-label">{m.rate_limits_plan_label()}</span>
        <span class="meta-value">{snapshot.planType}</span>
      </span>
    {/if}
    {#if snapshot.creditsHas}
      <span class="meta-item">
        <span class="meta-label">{m.rate_limits_credits_label()}</span>
        <span class="meta-value">
          {snapshot.creditsUnlimited
            ? m.rate_limits_credits_unlimited()
            : snapshot.creditsBalance}
        </span>
      </span>
    {/if}
  </div>

  {#if history.length > 1}
    <div class="history">
      <RateLimitHistoryChart snapshots={history} color={barColor} />
    </div>
  {/if}
</div>

<style>
  .rate-limit-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md, 8px);
    background: var(--bg-surface);
    min-width: 220px;
  }

  .card-header {
    display: flex;
    align-items: baseline;
    gap: 6px;
    font-size: 12px;
  }

  .limit-id {
    font-weight: 600;
    color: var(--text-primary);
    font-family: var(--font-mono, monospace);
  }

  .window-length {
    color: var(--text-secondary);
  }

  .progress-track {
    height: 6px;
    border-radius: 3px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    overflow: hidden;
  }

  .progress-fill {
    height: 100%;
    transition: width 0.4s ease;
  }

  .progress-stats {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .used-percent {
    font-weight: 600;
  }

  .meta-row {
    display: flex;
    gap: 16px;
    font-size: 11px;
  }

  .meta-item {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .meta-label {
    color: var(--text-muted);
  }

  .meta-value {
    color: var(--text-primary);
    font-family: var(--font-mono, monospace);
  }

  .history {
    margin-top: 4px;
  }
</style>
