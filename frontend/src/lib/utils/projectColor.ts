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
