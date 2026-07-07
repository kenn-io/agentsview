<script lang="ts">
  import {
    initMarkdownMermaidRendering,
    mermaidCodeFence,
    type MarkdownMermaidOptions,
  } from "@kenn-io/kit-ui/utils/markdown-mermaid";

  interface Props {
    content: string;
    /** Test seam: forwarded to kit-ui's renderer (injectable mermaid loader). */
    mermaidOptions?: MarkdownMermaidOptions;
  }

  let { content, mermaidOptions }: Props = $props();

  let container: HTMLDivElement | undefined = $state();

  $effect(() => {
    if (!container) return;
    const controller = initMarkdownMermaidRendering(container, mermaidOptions);
    return () => controller.disconnect();
  });
</script>

<!-- kit-ui's post-processor renders the pre.mermaid block into a pan/zoom
     viewer with copy and lightbox controls; on render failure the escaped
     source stays visible (and searchable) as-is. -->
<div class="mermaid-block" bind:this={container}>
  {@html mermaidCodeFence(content, "mermaid")}
</div>

<style>
  .mermaid-block {
    margin: 4px 0;
  }
</style>
