// ABOUTME: Resolves the message layout class, suspending skim during search.
import type { MessageLayout } from "../stores/ui.svelte.js";

/**
 * Resolve the layout actually applied to the transcript. Skim hides
 * auto-expanded search matches, so while a highlight query is active it
 * falls back to the full "default" layout; every other layout is returned
 * unchanged.
 */
export function resolveMessageLayout(
  layout: MessageLayout,
  highlightActive: boolean,
): MessageLayout {
  if (layout === "skim" && highlightActive) return "default";
  return layout;
}
