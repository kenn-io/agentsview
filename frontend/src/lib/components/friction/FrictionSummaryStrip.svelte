<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import type { FrictionKind, SignalSection } from "./frictionView.js";

  interface Props {
    sections: SignalSection[];
  }

  let { sections }: Props = $props();

  const LABELS: Record<FrictionKind, () => string> = {
    correction: m.friction_section_corrections,
    error: m.friction_section_errors,
    workaround: m.friction_section_workarounds,
    deferral: m.friction_section_deferrals,
    pattern: m.friction_section_patterns,
    frustration: m.friction_section_frustration,
    interruption: m.friction_section_interruptions,
    diagnostic: m.friction_section_diagnostics,
  };
</script>

<div class="summary-strip">
  {#each sections as section (section.kind)}
    <Card
      level="default"
      padding="none"
      class="tile"
    >
      <span class="value" class:muted={section.signals.length === 0}>{section.signals.length}</span>
      <span class="label">{LABELS[section.kind]()}</span>
    </Card>
  {/each}
</div>

<style>
  .summary-strip {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(104px, 1fr));
    gap: 8px;
  }
  .summary-strip :global(.tile) {
    padding: 10px 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .summary-strip :global(.tile > .kit-card__body) {
    display: contents;
  }
  .value {
    font-size: 20px;
    font-weight: 600;
    line-height: 1.2;
    color: var(--text-primary);
  }
  .label {
    font-size: 11px;
    color: var(--text-muted);
  }
  .value.muted {
    color: var(--text-muted);
    font-weight: 500;
  }
</style>
