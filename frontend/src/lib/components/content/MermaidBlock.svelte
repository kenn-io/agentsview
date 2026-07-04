<script lang="ts">
  import { onDestroy } from "svelte";
  import { renderMermaid, type MermaidRenderError } from "../../utils/mermaid.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    content: string;
  }

  let { content }: Props = $props();
  let renderState: "pending" | "ready" | "error" = $state("pending");
  let svg: string = $state("");
  let error: MermaidRenderError | null = $state(null);

  $effect(() => {
    const source = content;
    let cancelled = false;
    renderState = "pending";
    svg = "";
    error = null;

    renderMermaid(source).then((result) => {
      if (cancelled) return;
      if (result.ok) {
        svg = result.svg;
        renderState = "ready";
        return;
      }
      error = result.error;
      renderState = "error";
    });

    return () => {
      cancelled = true;
    };
  });

  onDestroy(() => {
    error = null;
  });
</script>

<div class="mermaid-block" aria-busy={renderState === "pending"}>
  {#if renderState === "ready"}
    <div class="mermaid-diagram">
      {@html svg}
    </div>
  {:else if renderState === "error"}
    <div class="mermaid-fallback">
      <div class="mermaid-fallback-label">
        {m.mermaid_render_failed()}
      </div>
      {#if error?.message}
        <div class="mermaid-fallback-error">{error.message}</div>
      {/if}
      <pre class="mermaid-source">{content}</pre>
    </div>
  {:else}
    <div class="mermaid-pending" role="status" aria-live="polite">
      {m.mermaid_render_pending()}
    </div>
  {/if}
</div>

<style>
  .mermaid-block {
    background: var(--code-bg);
    border-radius: var(--radius-md);
    margin: 4px 0;
    overflow: hidden;
  }

  .mermaid-diagram {
    padding: 12px 16px;
    overflow: auto;
  }

  .mermaid-diagram :global(svg) {
    max-width: 100%;
    height: auto;
  }

  .mermaid-pending,
  .mermaid-fallback {
    padding: 12px 16px;
  }

  .mermaid-pending {
    min-height: 96px;
    display: flex;
    align-items: center;
    color: var(--text-muted);
    font-size: 13px;
    line-height: 1.5;
  }

  .mermaid-fallback {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .mermaid-fallback-label {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .mermaid-fallback-error {
    font-size: 12px;
    color: var(--text-muted);
  }

  .mermaid-source {
    margin: 0;
    padding: 12px 16px;
    border-top: 1px solid color-mix(in srgb, var(--code-text) 8%, transparent);
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.55;
    color: var(--code-text);
    overflow-x: auto;
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
