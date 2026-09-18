<script lang="ts">
  import { onMount } from "svelte";
  import { Card } from "@kenn-io/kit-ui";
  import { Chart, Layer, Spline } from "layerchart";
  import { scaleUtc } from "d3-scale";
  import { UsageService, type DbRateLimitSeries } from "../../api/generated/index";
  import { ApiError } from "../../api/runtime.js";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { addDays, parseLocalDate } from "../../utils/dates.js";

  const CLOCK_INTERVAL_MS = 60_000;
  let { machine, from, to, refreshKey }: {
    machine: string; from: string; to: string; refreshKey: number;
  } = $props();
  let series: DbRateLimitSeries[] = $state([]);
  let error = $state(false);
  let now = $state(Date.now());
  let requestScope = "";
  const since = $derived((parseLocalDate(from)?.getTime() ?? 0) / 1000);
  const until = $derived((parseLocalDate(addDays(to, 1))?.getTime() ?? 0) / 1000);
  const percent = $derived(new Intl.NumberFormat(getLocale(), {
    style: "percent", maximumFractionDigits: 1,
  }));
  const cards = $derived(series.flatMap((item) => {
    const current = item.current;
    const windows = (["primary", "secondary"] as const).filter((kind) => current[kind]);
    return (windows.length ? windows : current.credits ? [null] : []).map((kind) => ({ item, current, kind }));
  }));
  const machines = $derived([...new Set(cards.map(({ item }) => item.machine))]);

  onMount(() => {
    const timer = setInterval(() => { now = Date.now(); }, CLOCK_INTERVAL_MS);
    return () => clearInterval(timer);
  });

  function duration(minutes: number): string {
    const parts: string[] = [];
    for (const [unit, size] of [["day", 1440], ["hour", 60], ["minute", 1]] as const) {
      const count = Math.floor(minutes / size);
      if (count) parts.push(new Intl.NumberFormat(getLocale(), {
        style: "unit", unit, unitDisplay: "narrow",
      }).format(count));
      minutes %= size;
      if (parts.length === 2) break;
    }
    return parts.join(" ");
  }

  function resetLabel(at: number | null): string {
    if (at == null) return m.rate_limits_resets({ time: m.shared_unknown() });
    if (at * 1000 <= now) return m.rate_limits_resets_now();
    return m.rate_limits_resets_in({ time: duration(Math.ceil((at * 1000 - now) / 60_000)) });
  }

  $effect(() => {
    void refreshKey;
    const controller = new AbortController();
    const scope = JSON.stringify([machine, since, until]);
    if (scope !== requestScope) {
      requestScope = scope;
      series = [];
    }
    error = false;
    UsageService.getApiV1RateLimits(
      { machine, since, until }, { signal: controller.signal },
    ).then((result) => {
      if (!controller.signal.aborted) series = result;
    }).catch((cause) => {
      const unsupported = cause instanceof ApiError && cause.status === 501;
      if (!controller.signal.aborted && !unsupported) error = true;
    });
    return () => controller.abort();
  });

</script>

{#if error}
  <p role="alert">{m.rate_limits_error()}</p>
{:else if cards.length}
  <section aria-label={m.rate_limits_title()}>
    <Card title={m.rate_limits_title()} padding="sm">
      {#each machines as machineName (machineName)}
        <h4>{sessions.machineLabel(machineName)}</h4>
        <div class="limits-grid">
          {#each cards.filter(({ item }) => item.machine === machineName) as { item, current, kind } (JSON.stringify([item.limit_id, kind]))}
            {@const window = kind ? current[kind] : undefined}
            {@const label = window?.window_minutes === 300 ? m.rate_limits_session()
              : window?.window_minutes === 10080 ? m.rate_limits_weekly()
              : window?.window_minutes ? m.rate_limits_window({ duration: duration(window.window_minutes) })
              : kind === "primary" ? m.rate_limits_primary() : m.rate_limits_secondary()}
            {@const used = window?.used_percent}
            {@const color = used == null ? "var(--text-muted)" : used >= 90
              ? "var(--accent-red)" : used >= 70 ? "var(--accent-amber)" : "var(--accent-blue)"}
            <Card padding="sm" title={window ? label : m.rate_limits_credits()}
              meta={window?.window_minutes ? duration(window.window_minutes) : undefined}>
              {#if current.limit_name || (item.limit_id && item.limit_id !== "codex")}
                <small class="limit-name">{current.limit_name || item.limit_id}</small>
              {/if}
              {#if window}
                <div class="progress" role="progressbar" aria-label={label}
                  aria-valuemin="0" aria-valuemax="100"
                  aria-valuenow={used == null ? undefined : Math.max(0, Math.min(100, used))}
                  aria-valuetext={used == null ? m.shared_unknown() : percent.format(used / 100)}>
                  {#if used != null}
                    <div style:width={`${Math.max(0, Math.min(100, used))}%`} style:background={color}></div>
                  {/if}
                </div>
                <div class="stats">
                  <strong style:color>{used == null ? m.shared_unknown() : percent.format(used / 100)}</strong>
                  <span title={window.resets_at == null ? undefined : formatDateTime(window.resets_at * 1000, {
                    dateStyle: "medium", timeStyle: "short",
                  })}>{resetLabel(window.resets_at)}</span>
                </div>
                {#if item.points.length}
                  <div class="history">
                    <Chart data={item.points} x={(point) => Date.parse(point.observed_at)}
                      y={(point) => {
                        const reading = point[kind!];
                        return reading?.used_percent != null && reading.window_minutes === window.window_minutes
                          ? Math.max(0, Math.min(100, reading.used_percent)) : null;
                      }}
                      xScale={scaleUtc()} yDomain={[0, 100]}
                      padding={2} height={72}
                      role="img" aria-label={m.rate_limits_history()}>
                      <Layer>
                        <Spline fill="none" stroke={color} strokeWidth={2}
                          stroke-linecap="round" stroke-linejoin="round" />
                      </Layer>
                    </Chart>
                  </div>
                {/if}
              {/if}
              {#if current.credits && (kind === "primary" || !current.primary)}
                <p class="credits">
                  {m.rate_limits_credits()}:
                  {#if current.credits.balance != null}
                    {current.credits.balance.trim() && Number.isFinite(Number(current.credits.balance))
                      ? Number(current.credits.balance).toLocaleString(getLocale(), { maximumFractionDigits: 2 })
                      : current.credits.balance}
                  {:else if current.credits.has_credits != null}
                    {current.credits.has_credits ? m.rate_limits_available() : m.rate_limits_unavailable()}
                  {:else}
                    {m.shared_unknown()}
                  {/if}
                  {#if current.credits.unlimited === true} · {m.rate_limits_unlimited()}{/if}
                </p>
              {/if}
            </Card>
          {/each}
        </div>
      {/each}
    </Card>
  </section>
{/if}

<style>
  h4 {
    margin: 0 0 8px;
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }
  .limits-grid + h4 {
    margin-top: 16px;
  }
  .limits-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(240px, 100%), 1fr));
    gap: 12px;
    align-items: start;
  }
  .limit-name {
    display: block;
    margin-bottom: 8px;
    overflow-wrap: anywhere;
  }
  .stats {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    color: var(--text-secondary);
  }
  .stats, .credits {
    margin: 8px 0 0;
    font-size: 11px;
  }
  .progress {
    height: 6px;
    border-radius: 3px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    overflow: hidden;
  }
  .progress > div {
    height: 100%;
  }
  .history {
    margin-top: 12px;
  }
  small {
    color: var(--text-muted);
  }
</style>
