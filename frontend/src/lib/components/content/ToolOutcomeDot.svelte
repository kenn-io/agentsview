<script lang="ts">
  import { OUTCOME_TONES, type ToolOutcome } from "../../utils/tool-outcomes.js";

  let { outcome }: { outcome: ToolOutcome } = $props();
</script>

<i class="dot" class:hollow={outcome === "unknown"} data-kit-tone={OUTCOME_TONES[outcome]} aria-hidden="true"></i>

<style>
  .dot {
    display: inline-block;
    flex-shrink: 0;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--kit-tone, var(--text-muted));
  }

  .dot.hollow {
    width: 7px;
    height: 7px;
    border: 1.5px solid var(--text-muted);
    background: transparent;
  }
</style>
