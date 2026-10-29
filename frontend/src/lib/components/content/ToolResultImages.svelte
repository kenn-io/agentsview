<!-- ABOUTME: Shows the images a tool result holds as transcript-sized previews. -->
<!-- ABOUTME: Each preview opens the image at full size in a dialog. -->
<script lang="ts">
  import { onDestroy } from "svelte";
  import { Modal } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { assetImageSource, loadAssetImages } from "../../utils/markdown.js";
  import { ui } from "../../stores/ui.svelte.js";

  interface Props {
    /** Inline data URIs or asset:// references, in result order. */
    sources: string[];
    alt: string;
  }

  let { sources, alt }: Props = $props();
  let openIndex: number | null = $state(null);
  // Offloaded images arrive as asset:// references, which only the loader can fetch.
  let imageSources = $derived(sources.map(assetImageSource));
  let openSource = $derived(openIndex === null ? null : (imageSources[openIndex] ?? null));

  // The app-wide modal slot lets Escape close this dialog without also
  // leaving the session, and pauses transcript shortcuts while it is open.
  $effect(() => {
    if (ui.activeModal !== "toolImage") openIndex = null;
  });

  function close() {
    openIndex = null;
    if (ui.activeModal === "toolImage") ui.activeModal = null;
  }

  // Leaving the session with the dialog open unmounts this component, and a
  // slot left behind would keep blocking transcript shortcuts.
  onDestroy(() => {
    if (openIndex !== null) close();
  });

  // Transcript rows are positioned with transforms, which would confine a
  // fixed dialog to its row and route its clicks through the row's handlers.
  function attachToBody(node: HTMLElement) {
    document.body.appendChild(node);
    return () => node.remove();
  }
</script>

<div class="tool-images" use:loadAssetImages={imageSources.join("\n")}>
  {#each imageSources as src, index (index)}
    <button
      type="button"
      class="tool-image"
      aria-label={m.tool_block_image_open()}
      title={m.tool_block_image_open()}
      onclick={(event) => {
        event.stopPropagation();
        openIndex = index;
        ui.activeModal = "toolImage";
      }}
    >
      <img {src} {alt} loading="lazy" decoding="async" />
    </button>
  {/each}
</div>
{#if openSource !== null}
  <div {@attach attachToBody}>
    <Modal ariaLabel={alt} closable={false} maxWidth="calc(100vw - 32px)" onclose={close}>
      <button
        type="button"
        class="tool-image-full"
        aria-label={m.tool_block_image_close()}
        use:loadAssetImages={openSource}
        onclick={close}
      >
        <img src={openSource} {alt} />
      </button>
    </Modal>
  </div>
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
    background: none;
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
    width: 100%;
    padding: 0;
    border: none;
    background: none;
    cursor: zoom-out;
  }
  /* Natural size, shrunk only to fit the viewport. */
  .tool-image-full img {
    display: block;
    max-width: 100%;
    max-height: calc(100svh - 140px);
    object-fit: contain;
  }
</style>
