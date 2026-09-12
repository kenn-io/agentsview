<script lang="ts">
  import type { FixtureProps } from "./props.svelte.js";
  import { searchBlock } from "../search-block.svelte.js";
  let props: FixtureProps = $props();
</script>

<div {@attach searchBlock(props.blockKey, () => props)}>{@html props.html}</div>
