import type { Message } from "../api/types.js";

/**
 * Compute the most frequently used model across assistant messages.
 * Returns empty string if no model data is present.
 * Tie-break: alphabetically first model wins.
 */
export function computeMainModel(messages: Message[]): string {
  const counts = new Map<string, number>();
  for (const m of messages) {
    if (m.role === "assistant" && m.model && m.model !== "<synthetic>") {
      counts.set(m.model, (counts.get(m.model) ?? 0) + 1);
    }
  }
  let best = "";
  let bestN = 0;
  for (const [model, n] of counts) {
    if (n > bestN || (n === bestN && model < best)) {
      best = model;
      bestN = n;
    }
  }
  return best;
}
