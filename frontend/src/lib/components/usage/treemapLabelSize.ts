export type TreemapLabelScale = "linear" | "sqrt" | "log";

export const TREEMAP_LABEL_SCALES: readonly TreemapLabelScale[] = ["linear", "sqrt", "log"];

const MIN_LABEL_PX = 11;
const MAX_LABEL_PX = 18;
// The meta line's baseline sits this far below the label size, so a label
// no larger than the tile height minus this keeps every line inside the tile.
const LINES_BELOW_PX = 31;

export function isTreemapLabelScale(value: unknown): value is TreemapLabelScale {
  return value === "linear" || value === "sqrt" || value === "log";
}

// Labels on large tiles grow with the tile's weight, from the 11px floor up to
// 18px, so the biggest categories read first. sqrt suits most data because tile
// area already grows linearly with share; log separates values that differ by
// orders of magnitude. The size never grows too tall for the lines under it.
export function treemapLabelSize(
  value: number,
  total: number,
  tileHeight: number,
  scale: TreemapLabelScale,
): number {
  if (total <= 0) return MIN_LABEL_PX;
  let weight: number;
  if (scale === "linear") weight = value / total;
  else if (scale === "sqrt") weight = Math.sqrt(value / total);
  // +2 keeps both logarithms positive when a value is zero.
  else weight = Math.log(value + 2) / Math.log(total + 2);
  const size = Math.round(MIN_LABEL_PX + (MAX_LABEL_PX - MIN_LABEL_PX) * weight);
  return Math.max(MIN_LABEL_PX, Math.min(MAX_LABEL_PX, size, tileHeight - LINES_BELOW_PX));
}
