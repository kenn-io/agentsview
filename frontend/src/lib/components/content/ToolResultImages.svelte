<!-- ABOUTME: Shows the images a tool result holds as transcript-sized previews. -->
<!-- ABOUTME: Each preview opens the image at full size in a dialog. -->
<script lang="ts">
  import { Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { loadAssetImages } from "../../utils/markdown.js";

  interface Props {
    /** Inline data URIs or asset:// references, in result order. */
    sources: string[];
    alt: string;
  }

  let { sources, alt }: Props = $props();
  let openIndex: number | null = $state(null);
  let openSource = $derived(openIndex === null ? null : (sources[openIndex] ?? null));
</script>

<div class="tool-images" use:loadAssetImages={sources.join("\n")}>
  {#each sources as src, index (index)}
    <button
      type="button"
      class="tool-image"
      aria-label={m.tool_block_image_open()}
      title={m.tool_block_image_open()}
      onclick={(event) => {
        event.stopPropagation();
        openIndex = index;
      }}
    >
      <img {src} {alt} loading="lazy" decoding="async" />
    </button>
  {/each}
</div>
{#if openSource !== null}
  <Modal
    title={m.tool_block_image_dialog()}
    closeLabel={m.tool_block_image_close()}
    maxWidth="calc(100vw - 32px)"
    onclose={() => (openIndex = null)}
  >
    <div class="tool-image-full" use:loadAssetImages={openSource}>
      <img src={openSource} {alt} />
    </div>
  </Modal>
{/if}

<style>
  .tool-images {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 8px 14px 10px;
    border-top: 1px solid var(--border-muted);
  }
  .tool-image {
    display: block;
    max-width: 100%;
    padding: 0;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    cursor: zoom-in;
    overflow: hidden;
  }
  .tool-image:hover {
    border-color: var(--border-default);
  }
  .tool-image:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }
  .tool-image img {
    display: block;
    max-width: 100%;
    max-height: 320px;
    width: auto;
    height: auto;
    object-fit: contain;
  }
  .tool-image-full {
    display: flex;
    justify-content: center;
  }
  /* Natural size, shrunk only to fit the viewport. */
  .tool-image-full img {
    display: block;
    max-width: 100%;
    max-height: calc(100svh - 140px);
    object-fit: contain;
  }
</style>
