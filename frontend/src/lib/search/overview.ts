/** Height-aware overview geometry; independent of browser rendering. */
import type { SearchBlock } from "./block-text.js";
import type { Match } from "./session-index.js";

export interface OverviewRow {
  offset: number;
  size: number;
  blocks: readonly SearchBlock[];
}
export interface OverviewLocation {
  offset: number;
  match: Match;
}
export interface OverviewTick {
  y: number;
  height: number;
}

/** Approximate block positions using rendered-text length within each row. */
export function overviewLocations(
  rows: readonly OverviewRow[],
  matches: readonly Match[],
): OverviewLocation[] {
  const byBlock = new Map<string, Match[]>();
  for (const match of matches) {
    const list = byBlock.get(match.blockKey);
    if (list) list.push(match);
    else byBlock.set(match.blockKey, [match]);
  }
  const locations: OverviewLocation[] = [];
  for (const row of rows) {
    const length = row.blocks.reduce((sum, block) => sum + Math.max(1, block.text.length), 0);
    let preceding = 0;
    for (const block of row.blocks) {
      for (const match of byBlock.get(block.key) ?? []) {
        const fraction =
          (preceding + Math.min(match.start, block.text.length)) / Math.max(1, length);
        locations.push({ offset: row.offset + fraction * row.size, match });
      }
      preceding += Math.max(1, block.text.length);
    }
  }
  return locations.sort((a, b) => a.offset - b.offset);
}

export function overviewY(offset: number, totalSize: number, height: number): number {
  if (!(totalSize > 0) || !(height > 0) || !Number.isFinite(offset)) return 0;
  return Math.max(0, Math.min(height - 1, (offset / totalSize) * height));
}

/** At least four pixels per tick; near ticks merge into one density region. */
export function overviewTicks(
  locations: readonly OverviewLocation[],
  totalSize: number,
  height: number,
): OverviewTick[] {
  if (!(height > 0) || !(totalSize > 0)) return [];
  let pixels = locations.map(({ offset }) => overviewY(offset, totalSize, height));
  if (pixels.length > 1000) pixels = [...new Set(pixels.map(Math.floor))];
  const ticks: OverviewTick[] = [];
  for (const pixel of pixels.sort((a, b) => a - b)) {
    const y = Math.max(0, Math.min(height - Math.min(4, height), pixel - 2));
    const end = Math.min(height, y + 4);
    const last = ticks[ticks.length - 1];
    if (last && y <= last.y + last.height + 2)
      last.height = Math.max(last.y + last.height, end) - last.y;
    else ticks.push({ y, height: end - y });
  }
  return ticks;
}

/** Dense drawing never changes hit-testing: choose the nearest real occurrence. */
export function nearestOverviewMatch(
  locations: readonly OverviewLocation[],
  offset: number,
): Match | undefined {
  if (!locations.length) return undefined;
  let lo = 0,
    hi = locations.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (locations[mid]!.offset < offset) lo = mid + 1;
    else hi = mid;
  }
  const right = locations[Math.min(lo, locations.length - 1)]!;
  const left = locations[Math.max(0, lo - 1)]!;
  return offset - left.offset <= right.offset - offset ? left.match : right.match;
}
