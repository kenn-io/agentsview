<script lang="ts">
  import { IconButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    PanelLeftCloseIcon,
    PanelLeftOpenIcon,
  } from "../../icons.js";
  import { ui } from "../../stores/ui.svelte.js";

  const label = $derived(
    ui.sidebarOpen
      ? m.nav_close_sidebar()
      : m.nav_open_sidebar(),
  );
</script>

<IconButton
  size="sm"
  onclick={() => ui.toggleSidebar()}
  title={m.nav_toggle_sidebar_shortcut()}
  ariaLabel={label}
>
  {#if ui.sidebarOpen}
    <PanelLeftCloseIcon size="14" strokeWidth="2" aria-hidden="true" />
  {:else}
    <PanelLeftOpenIcon size="14" strokeWidth="2" aria-hidden="true" />
  {/if}
</IconButton>
