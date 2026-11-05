// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import {
  COLUMN_WIDTHS_STORAGE_KEY,
  loadColumnWidths,
  mergeColumnWidths,
  saveColumnWidths,
} from "./columnWidths.js";

describe("recent edits column widths", () => {
  afterEach(() => localStorage.clear());

  it("keeps only the columns the user resized", () => {
    const next = mergeColumnWidths(
      { time: 120 },
      ["project", "file", "edits", "time"],
      [312.4, 640, 72, 120],
      new Set(["project"]),
    );
    expect(next).toEqual({ project: 312, time: 120 });
  });

  it("round-trips saved widths and drops unusable values", () => {
    saveColumnWidths({ project: 300, file: 500 });
    expect(loadColumnWidths()).toEqual({ project: 300, file: 500 });

    localStorage.setItem(
      COLUMN_WIDTHS_STORAGE_KEY,
      JSON.stringify({ project: "wide", file: -4, edits: 90 }),
    );
    expect(loadColumnWidths()).toEqual({ edits: 90 });

    localStorage.setItem(COLUMN_WIDTHS_STORAGE_KEY, "{not json");
    expect(loadColumnWidths()).toEqual({});
  });
});
