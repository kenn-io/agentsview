import type { UsageMode } from "../../stores/usage.svelte.js";

export function usageModeFromParams(params: Record<string, string>): UsageMode {
  return params.view === "tokens" ? "token" : "cost";
}

export function withUsageMode(
  params: Record<string, string>,
  mode: UsageMode,
): Record<string, string> {
  const next = { ...params };
  if (mode === "token") {
    next.view = "tokens";
  } else {
    delete next.view;
  }
  return next;
}
