// Recent edits column widths the user set by dragging or fitting a column
// edge. Only those columns are kept: a column nobody resized keeps sizing
// itself to the window (the file column takes what the others leave).

export const COLUMN_WIDTHS_STORAGE_KEY = "agentsview-recent-edits-columns";

export type ColumnWidths = Record<string, number>;

export function loadColumnWidths(): ColumnWidths {
  try {
    const raw = localStorage.getItem(COLUMN_WIDTHS_STORAGE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return {};
    const widths: ColumnWidths = {};
    for (const [key, value] of Object.entries(parsed)) {
      if (typeof value === "number" && Number.isFinite(value) && value > 0) {
        widths[key] = Math.round(value);
      }
    }
    return widths;
  } catch {
    // Corrupted or unavailable storage: fall back to automatic widths.
    return {};
  }
}

export function saveColumnWidths(widths: ColumnWidths): void {
  try {
    localStorage.setItem(COLUMN_WIDTHS_STORAGE_KEY, JSON.stringify(widths));
  } catch {
    // Storage full or unavailable: widths last for this visit only.
  }
}
