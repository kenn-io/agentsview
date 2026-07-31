import { tick } from "svelte";
import { ui } from "../stores/ui.svelte.js";

type SidebarControlPlacement = "sidebar" | "content";

function focusedPlacement(
  element: Element | null,
): SidebarControlPlacement | null {
  if (!element) return null;

  const sidebar = document.getElementById("session-sidebar");
  if (
    sidebar?.contains(element) ||
    element.closest(".sidebar-panel-control--sidebar")
  ) {
    return "sidebar";
  }
  if (
    element.closest('[data-sidebar-focus-region="content"]') ||
    element.closest(".sidebar-panel-control--content")
  ) {
    return "content";
  }
  return null;
}

export async function toggleSidebarWithFocus(): Promise<void> {
  const placementBeingHidden: SidebarControlPlacement = ui.sidebarOpen
    ? "sidebar"
    : "content";
  const shouldMoveFocus =
    focusedPlacement(document.activeElement) === placementBeingHidden;
  const nextPlacement = ui.sidebarOpen ? "content" : "sidebar";

  ui.toggleSidebar();

  if (!shouldMoveFocus) return;

  await tick();
  document
    .querySelector<HTMLButtonElement>(
      `.sidebar-panel-control--${nextPlacement}`,
    )
    ?.focus();
}
