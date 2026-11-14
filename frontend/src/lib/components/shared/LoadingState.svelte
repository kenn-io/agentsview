<script lang="ts">
  import { EmptyState, Spinner } from "@kenn-io/kit-ui";

  interface Props {
    /** What is loading, as a short phrase ("Loading pinned messages..."). */
    label: string;
    /** Matches EmptyState: `top` for a page or a panel under its header. */
    align?: "top" | "center";
    class?: string;
  }

  let { label, align = "top", class: className = "" }: Props = $props();
</script>

<!-- The first-load state for a page or panel. It takes the place and
     shape of the EmptyState that may replace it, so the page does not jump
     when the read lands. Pages with a layout-shaped skeleton keep it. -->
<EmptyState {align} role="status" title={label} class="loading-state {className}">
  {#snippet icon()}
    <Spinner size={20} label={label} />
  {/snippet}
</EmptyState>
