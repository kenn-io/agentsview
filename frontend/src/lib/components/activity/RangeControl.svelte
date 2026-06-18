<script lang="ts">
  import { activity } from "../../stores/activity.svelte.js";

  type Preset = "day" | "week" | "month" | "custom";

  const segments: { value: Preset; label: string }[] = [
    { value: "day", label: "Day" },
    { value: "week", label: "Week" },
    { value: "month", label: "Month" },
    { value: "custom", label: "Custom" },
  ];

  function select(value: Preset) {
    activity.setPreset(value);
    activity.load();
  }
</script>

<div class="range-control" role="group" aria-label="Report range">
  {#each segments as segment (segment.value)}
    <button
      class="segment-btn"
      class:active={activity.preset === segment.value}
      onclick={() => select(segment.value)}
    >
      {segment.label}
    </button>
  {/each}
</div>

<style>
  .range-control {
    display: flex;
    gap: 2px;
  }

  .segment-btn {
    height: 26px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .segment-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .segment-btn.active {
    background: var(--accent-blue);
    color: #fff;
  }
</style>
