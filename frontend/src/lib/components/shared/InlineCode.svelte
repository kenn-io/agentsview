<!-- A command, URL, or identifier set inside running text: mono on a
     subtle inset chip, so `agentsview db migrate --images` reads as
     something to type rather than as part of the sentence. Matches the
     inline code in import hints and rendered markdown. -->
<script lang="ts">
  import type { Snippet } from "svelte";

  let { children }: { children: Snippet } = $props();
</script>

<code class="inline-code">{@render children()}</code>

<style>
  /* kit-ui-check-ignore: an inline <code> chip inside running text; kit-ui Card is a block <div> */
  .inline-code {
    font-family: var(--font-mono, monospace);
    font-size: 0.95em;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    padding: 0 0.35em;
    overflow-wrap: anywhere;
    -webkit-box-decoration-break: clone;
    box-decoration-break: clone;
  }
</style>
