<script lang="ts">
  import { onMount, untrack } from "svelte";
  import {
    createRefreshScheduler,
    DEFAULT_REFRESH_INTERVAL_MS,
    formatRefreshAge,
  } from "../../utils/refresh.js";
  import { RefreshCwIcon } from "../../icons.js";

  // Re-evaluate the relative age label this often so it advances without a data
  // fetch. Shared by every dashboard that shows an "Updated Xm ago" status.
  const REFRESH_LABEL_INTERVAL_MS = 60 * 1000;

  let {
    lastUpdatedAt,
    busy = false,
    onRefresh,
    label,
    title,
    intervalMs = DEFAULT_REFRESH_INTERVAL_MS,
  }: {
    /** Epoch ms of the last successful fetch, or null before the first load. */
    lastUpdatedAt: number | null;
    /** Spins the icon and disables the button while a refresh is in flight. */
    busy?: boolean;
    /** Refetches the dashboard data; invoked on the interval and on click. */
    onRefresh: () => void;
    /** Accessible name for the button (aria-label). */
    label: string;
    /** Tooltip text; defaults to `label` when omitted. */
    title?: string;
    /** Auto-refresh cadence in ms; defaults to the shared 5-minute interval. */
    intervalMs?: number;
  } = $props();

  // The page owns the initial load -- it alone knows when its URL/filter state
  // is hydrated -- so this control only keeps the data fresh afterward. Arm the
  // interval without an immediate fetch (scheduleNext, not start) so the first
  // auto-refresh lands one interval out instead of racing the page's mount; a
  // manual click refreshes now and resets that timer. intervalMs is read once
  // at setup (untrack); a live cadence change would need a fresh scheduler.
  const scheduler = createRefreshScheduler(
    () => onRefresh(),
    untrack(() => intervalMs),
  );

  // Local clock that ticks once a minute so the age label re-derives without a
  // data fetch. Seeded once at mount.
  let tick = $state(Date.now());
  const ageLabel = $derived(formatRefreshAge(lastUpdatedAt, tick));

  onMount(() => {
    scheduler.scheduleNext();
    let labelTimer: ReturnType<typeof setTimeout> | undefined;
    function scheduleLabelTick() {
      labelTimer = setTimeout(() => {
        tick = Date.now();
        scheduleLabelTick();
      }, REFRESH_LABEL_INTERVAL_MS);
    }
    scheduleLabelTick();
    return () => {
      scheduler.stop();
      if (labelTimer !== undefined) clearTimeout(labelTimer);
    };
  });
</script>

<div class="refresh-control">
  <button
    class="refresh-btn"
    class:querying={busy}
    onclick={() => scheduler.refreshNow()}
    disabled={busy}
    title={title ?? label}
    aria-label={label}
  >
    <RefreshCwIcon size="14" strokeWidth="2" aria-hidden="true" />
  </button>
  <div class="refresh-status">
    <span
      title={lastUpdatedAt === null
        ? undefined
        : new Date(lastUpdatedAt).toLocaleString()}
    >
      {ageLabel}
    </span>
  </div>
</div>

<style>
  .refresh-control {
    min-height: 28px;
    display: inline-flex;
    align-items: center;
    gap: 8px;
  }

  .refresh-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s, opacity 0.1s;
  }

  .refresh-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .refresh-btn:disabled {
    cursor: default;
    opacity: 0.75;
  }

  .refresh-btn.querying :global(svg) {
    animation: spin 0.8s linear infinite;
  }

  .refresh-status {
    min-height: 24px;
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--text-muted);
    font-size: 11px;
    white-space: nowrap;
  }

  @keyframes spin {
    from {
      transform: rotate(0deg);
    }
    to {
      transform: rotate(360deg);
    }
  }
</style>
