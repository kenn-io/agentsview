<script lang="ts">
  import type { DbWorktreeReclassificationCandidate } from "../../api/generated/index";
  import { m } from "../../i18n/index.js";

  let { candidate }: { candidate: DbWorktreeReclassificationCandidate } = $props();
  const paths = $derived([...new Set(candidate.examples.map((example) => example.cwd).filter(Boolean))]);
</script>

{#if paths.length > 0}
  <details>
    <summary>{m.data_candidate_observed_paths({ count: candidate.distinct_cwds })}</summary>
    <ul>
      {#each paths as path}<li>{path}</li>{/each}
    </ul>
    {#if candidate.distinct_cwds > paths.length}
      <p>{m.data_candidate_paths_sample()}</p>
    {/if}
  </details>
{/if}

<style>
  details { color: var(--text-muted); font-size: var(--font-size-xs); }
  summary { cursor: pointer; padding-block: var(--space-2); }
  ul { margin: var(--space-2) 0; padding-left: var(--space-7); }
  li { font-family: var(--font-mono); overflow-wrap: anywhere; }
  p { margin: var(--space-2) 0; }
</style>
