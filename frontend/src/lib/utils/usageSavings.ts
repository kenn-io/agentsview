export type SavingsState = "saved" | "costlier" | "none";

// Values whose absolute magnitude rounds to $0.00 at two
// decimals are treated as "none" so the UI doesn't render
// a misleading "$0.00 more than uncached" badge for a
// sub-cent delta.
const DISPLAY_EPSILON_MICRODOLLARS = 5_000;

// savingsState classifies a cache-savings dollar delta into
// the three states the Cache Efficiency panel renders:
//   - "saved"    : positive — cache reduced total cost
//   - "costlier" : negative — cache creation premium outweighed
//                  any read discount (common in write-only
//                  workloads where cache_creation > input rate)
//   - "none"     : zero, or within half a cent of zero — no
//                  signal worth showing at 2-decimal display
export function savingsState(value: number): SavingsState {
  if (Math.abs(value) < DISPLAY_EPSILON_MICRODOLLARS) return "none";
  if (value > 0) return "saved";
  return "costlier";
}
