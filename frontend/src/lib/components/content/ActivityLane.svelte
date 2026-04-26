<!-- ABOUTME: Compact message-density lane that fits the SessionVitals
     timeline grid (`.lane-row` / `.lane-track.activity`). Reads buckets
     from the existing sessionActivity store and renders one absolutely
     positioned `.activity-bar` per populated bucket. Click-to-scroll uses
     `ui.scrollToOrdinal`, matching ActivityMinimap's behavior. -->
<script lang="ts">
  import { sessionActivity } from "../../stores/sessionActivity.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import type { SessionActivityBucket } from "../../api/types/session-activity.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  $effect(() => {
    void sessionActivity.load(sessionId);
  });

  const chart = $derived.by(() => {
    const buckets = sessionActivity.buckets;
    if (buckets.length === 0) return null;

    const n = buckets.length;
    let maxCount = 0;
    for (const b of buckets) {
      const total = b.user_count + b.assistant_count;
      if (total > maxCount) maxCount = total;
    }
    if (maxCount === 0) maxCount = 1;

    // Width per slot in percent. Bars are slightly narrower than their slot
    // so neighboring bars never touch.
    const slotPct = 100 / n;
    const barWidthPct = Math.max(slotPct * 0.7, 0.5);

    const bars = buckets.map((bucket, i) => {
      const total = bucket.user_count + bucket.assistant_count;
      return {
        leftPct: i * slotPct,
        widthPct: barWidthPct,
        heightPct: total > 0 ? (total / maxCount) * 100 : 0,
        populated: total > 0,
        bucket,
        index: i,
      };
    });

    return { bars };
  });

  const activeIndex = $derived(sessionActivity.activeBucketIndex);

  function formatTime(iso: string): string {
    const d = new Date(iso);
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function barTitle(bucket: SessionActivityBucket): string {
    const range = `${formatTime(bucket.start_time)}–${formatTime(bucket.end_time)}`;
    const total = bucket.user_count + bucket.assistant_count;
    return `${range} · ${total} message${total === 1 ? "" : "s"}`;
  }

  function handleBarClick(bucket: SessionActivityBucket) {
    if (bucket.first_ordinal == null) return;
    if (ui.hasBlockFilters) {
      ui.showAllBlocks();
    }
    ui.scrollToOrdinal(bucket.first_ordinal);
  }
</script>

<div class="lane-row">
  <span class="lane-label">activity</span>
  <span class="lane-track activity">
    {#if chart}
      {#each chart.bars as bar (bar.index)}
        {#if bar.populated}
          <button
            class="activity-bar"
            class:active={activeIndex === bar.index}
            style="left: {bar.leftPct}%; width: {bar.widthPct}%; height: {bar.heightPct}%;"
            title={barTitle(bar.bucket)}
            onclick={() => handleBarClick(bar.bucket)}
            type="button"
            aria-label={barTitle(bar.bucket)}
          ></button>
        {/if}
      {/each}
    {/if}
  </span>
</div>

<style>
  /* These are duplicated from SessionVitals.svelte — Svelte scopes styles
     per component, so each consumer that renders a `.lane-row` needs the
     rules for it. Keeping them in sync is the cost of using one shared
     class name across two components. */
  .lane-row {
    display: grid;
    grid-template-columns: 48px 1fr;
    align-items: center;
    gap: 8px;
    margin-bottom: 4px;
  }
  .lane-label {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
  }
  .lane-track {
    height: 12px;
    background: var(--bg-inset, rgba(255, 255, 255, 0.04));
    border-radius: 2px;
    position: relative;
  }
  .lane-track.activity {
    height: 22px;
    background: var(--bg-inset, rgba(255, 255, 255, 0.03));
  }
  .activity-bar {
    position: absolute;
    bottom: 0;
    background: var(--accent-blue, #4a7ba8);
    border-radius: 1px 1px 0 0;
    border: 0;
    padding: 0;
    cursor: pointer;
    min-height: 1px;
  }
  .activity-bar:hover {
    filter: brightness(1.3);
  }
  .activity-bar.active {
    outline: 1px solid var(--text-primary);
    outline-offset: 0;
  }
</style>
