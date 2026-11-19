// One colour ramp and one step rule for every activity heatmap, so the
// calendar heatmap and the day-by-hour heatmap read on the same scale.
//
// The steps mix the theme's green into the card surface, so each theme
// (and its dark variant) gets its own ramp without a separate table, and a
// theme switch repaints the cells without re-rendering the chart. The top
// step is the full accent; the lower steps stay quiet next to it.
export const HEATMAP_LEVEL_COLORS = [
  "var(--bg-inset)",
  "color-mix(in srgb, var(--accent-green) 22%, var(--bg-surface))",
  "color-mix(in srgb, var(--accent-green) 44%, var(--bg-surface))",
  "color-mix(in srgb, var(--accent-green) 68%, var(--bg-surface))",
  "var(--accent-green)",
] as const;

export function heatmapLevelColor(level: number): string {
  return HEATMAP_LEVEL_COLORS[level] ?? HEATMAP_LEVEL_COLORS[0];
}

export interface HeatmapThresholds {
  l2: number;
  l3: number;
  l4: number;
}

/** Quartile thresholds over the non-zero values, the same rule the server
 * uses for the calendar heatmap's `levels`. */
export function quartileThresholds(values: number[]): HeatmapThresholds {
  const sorted = values.filter((v) => v > 0).sort((a, b) => a - b);
  const n = sorted.length;
  if (n === 0) return { l2: 2, l3: 3, l4: 4 };
  return {
    l2: sorted[Math.floor(n / 4)]!,
    l3: sorted[Math.floor(n / 2)]!,
    l4: sorted[Math.floor((n * 3) / 4)]!,
  };
}

/** The level (0-4) of a value: 0 for none, then one step per quartile. */
export function heatmapLevel(value: number, t: HeatmapThresholds): number {
  if (value <= 0) return 0;
  if (value <= t.l2) return 1;
  if (value <= t.l3) return 2;
  if (value <= t.l4) return 3;
  return 4;
}
