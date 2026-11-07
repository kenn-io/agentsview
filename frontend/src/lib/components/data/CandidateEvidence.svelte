<script lang="ts">
  import { Disclosure } from "@kenn-io/kit-ui";
  import type { DbWorktreeReclassificationCandidate } from "../../api/generated/index";
  import { m } from "../../i18n/index.js";

  let { candidate }: { candidate: DbWorktreeReclassificationCandidate } = $props();
  const paths = $derived([...new Set(candidate.examples.map((example) => example.cwd).filter(Boolean))]);
</script>

{#if paths.length > 0}
  <Disclosure variant="plain" title={m.data_candidate_observed_paths({ count: candidate.distinct_cwds })}>
    <ul>
      {#each paths as path}<li>{path}</li>{/each}
    </ul>
    {#if candidate.distinct_cwds > paths.length}
      <p>{m.data_candidate_paths_sample()}</p>
    {/if}
  </Disclosure>
{/if}

<style>
  ul, p { color: var(--text-muted); font-size: var(--font-size-xs); }
  ul { margin: var(--space-2) 0; padding-left: var(--space-7); }
  li { font-family: var(--font-mono); overflow-wrap: anywhere; }
  p { margin: var(--space-2) 0; }
</style>
