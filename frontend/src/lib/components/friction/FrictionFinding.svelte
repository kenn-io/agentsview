<script lang="ts">
  import { Chip } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { codeSegments, withoutSubjectPrefix } from "./frictionView.js";

  interface Finding {
    kind: string;
    label: string;
    tool_name: string;
    text: string;
    evidence: string;
  }

  interface Props {
    finding: Finding;
    // Diagnostic producers repeat their identity at the start of the text.
    subjectId?: string;
    // Interruptions and diagnostics render grouped rows with this count.
    count?: number;
    // A diagnostics heading already gives the total when only one group exists.
    showCount?: boolean;
  }

  let { finding, subjectId = "", count = 1, showCount = true }: Props = $props();

  const PATTERN_NAMES: Record<string, () => string> = {
    retry_loop: m.friction_pattern_retry_loop,
    runaway_loop: m.friction_pattern_runaway_loop,
    edit_churn: m.friction_pattern_edit_churn,
    mid_task_compaction: m.friction_pattern_mid_task_compaction,
    context_pressure: m.friction_pattern_context_pressure,
    iteration_runaway: m.friction_pattern_iteration_runaway,
  };

  function patternName(label: string): string {
    const name = PATTERN_NAMES[label];
    return name ? name() : label.replaceAll("_", " ");
  }
</script>

<span class="finding" data-kind={finding.kind}>
  {#if finding.kind === "correction" || finding.kind === "frustration"}
    <q class="quote">{finding.text}</q>
  {:else if finding.kind === "error"}
    {#if finding.tool_name}<code class="tool">{finding.tool_name}</code>{/if}
    <span class="text">{withoutSubjectPrefix(finding.text, subjectId)}</span>
  {:else if finding.kind === "workaround"}
    {#if finding.label}<Chip size="xs" tone="warning">{finding.label}</Chip>{/if}
    <span class="text">{finding.text}</span>
  {:else if finding.kind === "deferral"}
    <span class="text">{m.friction_deferral_said({ phrase: finding.label })}</span>
  {:else if finding.kind === "pattern"}
    <strong class="name">{patternName(finding.label)}</strong>
    <span class="text">
      {#each codeSegments(finding.evidence) as segment}
        {#if segment.code}<code>{segment.text}</code>{:else}{segment.text}{/if}
      {/each}
    </span>
  {:else if finding.kind === "diagnostic"}
    <code class="source">{finding.label}</code>
    <span class="text">{finding.text}</span>
    {#if showCount}
      <span class="count">{m.friction_diagnostic_count({ count })}</span>
    {/if}
  {:else if finding.kind === "interruption"}
    <span class="text">{m.friction_interruption_count({ count })}</span>
  {:else}
    <span class="text">{finding.text}</span>
  {/if}
</span>

<style>
  .finding {
    display: inline;
    color: var(--text-primary);
    word-break: break-word;
  }
  .finding > :global(* + *) {
    margin-left: 6px;
  }
  .quote {
    quotes: "\201C" "\201D";
  }
  .name {
    font-weight: 600;
  }
  .count {
    color: var(--text-muted);
  }
  code {
    font-family: var(--font-mono);
    font-size: 0.92em;
    padding: 0 4px;
    border-radius: 3px;
    background: color-mix(in srgb, var(--text-primary) 8%, transparent);
  }
</style>
