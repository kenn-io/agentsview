<script lang="ts">
  import { IconButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    PanelLeftCloseIcon,
    PanelLeftOpenIcon,
  } from "../../icons.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { toggleSidebarWithFocus } from "../../utils/sidebar-toggle.js";

  interface Props {
    placement: "sidebar" | "content";
  }

  let { placement }: Props = $props();

  const label = $derived(
    ui.sidebarOpen
      ? m.nav_close_sidebar()
      : m.nav_open_sidebar(),
  );
</script>

<IconButton
  class={`sidebar-panel-control sidebar-panel-control--${placement}`}
  size="sm"
  onclick={() => void toggleSidebarWithFocus()}
  title={m.nav_toggle_sidebar_shortcut()}
  ariaLabel={label}
  ariaExpanded={ui.sidebarOpen}
  ariaControls="session-sidebar"
>
  {#if ui.sidebarOpen}
    <PanelLeftCloseIcon size="14" strokeWidth="2" aria-hidden="true" />
  {:else}
    <PanelLeftOpenIcon size="14" strokeWidth="2" aria-hidden="true" />
  {/if}
</IconButton>
