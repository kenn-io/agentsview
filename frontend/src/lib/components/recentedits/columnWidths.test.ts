// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { COLUMN_WIDTHS_STORAGE_KEY, loadColumnWidths, saveColumnWidths } from "./columnWidths.js";

describe("recent edits column widths", () => {
  afterEach(() => localStorage.clear());

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
