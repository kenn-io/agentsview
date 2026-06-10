import { describe, it, expect } from "vite-plus/test";
import { squarify, type TreemapTile } from "./treemap.js";

describe("squarify", () => {
  it("returns empty array for empty input", () => {
    expect(squarify([], 400, 300)).toEqual([]);
  });

  it("returns single tile filling the full rect", () => {
    const tiles = squarify(
      [{ id: "a", value: 10 }], 400, 300,
    );
    expect(tiles).toHaveLength(1);
    expect(tiles[0]!.x).toBe(0);
    expect(tiles[0]!.y).toBe(0);
    expect(tiles[0]!.width).toBe(400);
    expect(tiles[0]!.height).toBe(300);
  });

  it("tiles sum to full area within tolerance", () => {
    const tiles = squarify(
      [
        { id: "a", value: 40 },
        { id: "b", value: 30 },
        { id: "c", value: 20 },
        { id: "d", value: 10 },
      ],
      400,
      300,
    );
    const totalArea = tiles.reduce(
      (s, t) => s + t.width * t.height, 0,
    );
    expect(totalArea).toBeCloseTo(400 * 300, -1);
  });

  it("tiles do not overlap", () => {
    const tiles = squarify(
      [
        { id: "a", value: 40 },
        { id: "b", value: 30 },
        { id: "c", value: 20 },
        { id: "d", value: 10 },
      ],
      400,
      300,
    );
    for (let i = 0; i < tiles.length; i++) {
      for (let j = i + 1; j < tiles.length; j++) {
        expect(tilesOverlap(tiles[i]!, tiles[j]!)).toBe(false);
      }
    }
  });

  it("filters out zero-value items", () => {
    const tiles = squarify(
      [
        { id: "a", value: 10 },
        { id: "b", value: 0 },
        { id: "c", value: 5 },
      ],
      400,
      300,
    );
    expect(tiles).toHaveLength(2);
    const ids = tiles.map((t) => t.id);
    expect(ids).toContain("a");
    expect(ids).toContain("c");
    expect(ids).not.toContain("b");
  });
});

function tilesOverlap(a: TreemapTile, b: TreemapTile): boolean {
  const EPS = 0.01;
  const aRight = a.x + a.width;
  const aBottom = a.y + a.height;
  const bRight = b.x + b.width;
  const bBottom = b.y + b.height;
  return (
    a.x < bRight - EPS &&
    aRight > b.x + EPS &&
    a.y < bBottom - EPS &&
    aBottom > b.y + EPS
  );
}
