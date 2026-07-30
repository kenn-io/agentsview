<script lang="ts">
  import { IconButton } from "@kenn-io/kit-ui";
  import { tick } from "svelte";
  import { m } from "../../i18n/index.js";
  import {
    PanelLeftCloseIcon,
    PanelLeftOpenIcon,
  } from "../../icons.js";
  import { ui } from "../../stores/ui.svelte.js";

  interface Props {
    placement: "sidebar" | "content";
  }

  let { placement }: Props = $props();

  const label = $derived(
    ui.sidebarOpen
      ? m.nav_close_sidebar()
      : m.nav_open_sidebar(),
  );

  async function toggleSidebar(event: MouseEvent) {
    const shouldMoveFocus = document.activeElement === event.currentTarget;
    ui.toggleSidebar();

    if (!shouldMoveFocus) return;

    await tick();
    const nextPlacement = placement === "sidebar" ? "content" : "sidebar";
    document
      .querySelector<HTMLButtonElement>(
        `.sidebar-panel-control--${nextPlacement}`,
      )
      ?.focus();
  }
</script>

<IconButton
  class={`sidebar-panel-control sidebar-panel-control--${placement}`}
  size="sm"
  onclick={toggleSidebar}
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
