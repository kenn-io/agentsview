<script lang="ts">
  import type { Snippet } from "svelte";
  import { PageHeader } from "@kenn-io/kit-ui";

  interface Props {
    title: string;
    description?: string;
    /** Beside the title, typically a neutral count. */
    trailing?: Snippet;
    /** Controls at the trailing edge of the header row. */
    actions?: Snippet;
  }

  let {
    title,
    description = undefined,
    trailing = undefined,
    actions = undefined,
  }: Props = $props();
</script>

<!-- The one page header for library pages (Recall, Pinned, Trash, Recent
     Edits, Data): an 18px semibold sentence-case title with no icon, at the
     same offset on every page. -->
<div class="library-header">
  <PageHeader {title} {description} {trailing} {actions} level={1} size="large" />
</div>

<style>
  .library-header {
    --kit-page-header-title-size: 18px;
    --kit-page-header-title-weight: 600;
    margin-bottom: 16px;
  }

  /* Top-align the row so a button beside the title never pushes the title
     down; every library title starts at the same offset. */
  .library-header :global(.kit-page-header) {
    align-items: flex-start;
  }
</style>
