import { hashColor } from "@kenn-io/kit-ui/utils/color-hash";

export const PROJECT_PALETTE: readonly string[] = [
  "var(--accent-blue)",
  "var(--accent-purple)",
  "var(--accent-amber)",
  "var(--accent-teal)",
  "var(--accent-rose)",
  "var(--accent-green)",
  "var(--accent-indigo)",
  "var(--accent-orange)",
  "var(--accent-sky)",
  "var(--accent-pink)",
  "var(--accent-coral)",
  "var(--accent-lime)",
] as const;

// Delegates to kit-ui's hashColor but deliberately keeps the app's own
// palette instead of kit-ui's DEFAULT_HASH_PALETTE: the palettes differ in
// order and content, so switching would reshuffle every project's
// established identity color.
export function projectColor(name: string): string {
  return hashColor(name, PROJECT_PALETTE);
}

export function seriesColorMap(
  ids: readonly string[],
): ReadonlyMap<string, string> {
  const colors = new Map<string, string>();
  const occupied = new Set<number>();
  const uniqueIds = [...new Set(ids)]
    .filter((id) => id !== "" && id !== "__other__")
    .sort();

  for (const id of uniqueIds) {
    const preferred = PROJECT_PALETTE.indexOf(projectColor(id));
    if (preferred < 0) continue;

    let slot = preferred;
    for (let offset = 0; offset < PROJECT_PALETTE.length; offset++) {
      const candidate = (preferred + offset) % PROJECT_PALETTE.length;
      if (!occupied.has(candidate)) {
        slot = candidate;
        occupied.add(candidate);
        break;
      }
    }
    colors.set(id, PROJECT_PALETTE[slot]!);
  }

  for (const id of ids) {
    if (id === "" || id === "__other__") colors.set(id, "var(--text-muted)");
  }

  return colors;
}
