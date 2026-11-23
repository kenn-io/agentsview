export type TreemapScale = "linear" | "sqrt" | "log";

export const TREEMAP_SCALES: readonly TreemapScale[] = ["linear", "sqrt", "log"];

const MIN_LABEL_PX = 11;
const MAX_LABEL_PX = 18;
// The meta line's baseline sits this far below the label size, so a label
// no larger than the tile height minus this keeps every line inside the tile.
const LINES_BELOW_PX = 31;

export function isTreemapScale(value: unknown): value is TreemapScale {
  return value === "linear" || value === "sqrt" || value === "log";
}

// Tile areas for each value. Linear areas are proportional to the values.
// Sqrt and log compress the range so small items stay visible next to a
// dominant one. Log measures each value against the smallest positive value,
// so the result does not depend on the unit (microdollars or tokens): the
// smallest tile gets area 1 and a value ten times larger gets 1 + ln(10).
// Zero and negative values get no area.
export function treemapAreas(values: readonly number[], scale: TreemapScale): number[] {
  if (scale === "linear") return values.map((value) => Math.max(0, value));
  if (scale === "sqrt") return values.map((value) => (value > 0 ? Math.sqrt(value) : 0));
  const positive = values.filter((value) => value > 0);
  if (positive.length === 0) return values.map(() => 0);
  const smallest = Math.min(...positive);
  return values.map((value) => (value > 0 ? 1 + Math.log(value / smallest) : 0));
}

// Labels on large tiles grow with the tile's share of the drawn area, from
// the 11px floor up to 18px, so the biggest tiles read first. The size follows
// the square root of the share because a label is a length and the share is
// an area. The size never grows too tall for the lines under it.
export function treemapLabelSize(area: number, totalArea: number, tileHeight: number): number {
  if (totalArea <= 0) return MIN_LABEL_PX;
  const weight = Math.sqrt(Math.max(0, area) / totalArea);
  const size = Math.round(MIN_LABEL_PX + (MAX_LABEL_PX - MIN_LABEL_PX) * weight);
  return Math.max(MIN_LABEL_PX, Math.min(MAX_LABEL_PX, size, tileHeight - LINES_BELOW_PX));
}
