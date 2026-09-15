import { describe, expect, it } from "vite-plus/test";
import type { Match } from "./session-index.js";
import type { SearchBlock } from "./block-text.js";
import { overviewLocations, overviewTicks, overviewY, nearestOverviewMatch } from "./overview.js";
function match(ordinal: number, start = 0, occurrence = 0): Match {
  return { ordinal, blockKey: `${ordinal}:text:0`, start, end: start + 1, occurrence };
}
function block(ordinal: number, text: string): SearchBlock {
  return { key: `${ordinal}:text:0`, ordinal, kind: "text", text };
}
describe("find overview model", () => {
  it("places matches proportionally inside their block and grouped row", () => {
    const matches = [match(1, 2), match(2, 3)];
    const locations = overviewLocations(
      [{ offset: 100, size: 100, blocks: [block(1, "aaaa"), block(2, "bbbbbb")] }],
      matches,
    );
    expect(locations.map((point) => point.offset)).toEqual([120, 170]);
    expect(locations.map((point) => point.match)).toEqual(matches);
  });
  it("uses visual offsets when newest-first reverses grouped messages", () => {
    const first = match(1),
      second = match(2);
    const locations = overviewLocations(
      [
        { offset: 0, size: 120, blocks: [block(2, "new")] },
        { offset: 120, size: 100, blocks: [block(1, "old")] },
      ],
      [first, second],
    );
    expect(locations.map((point) => point.match.ordinal)).toEqual([2, 1]);
  });
  it("ignores matches whose blocks are absent and handles an empty row", () => {
    expect(overviewLocations([{ offset: 0, size: 100, blocks: [] }], [match(1)])).toEqual([]);
  });
  it("uses a minimum block length without dividing by zero", () => {
    const locations = overviewLocations(
      [{ offset: 10, size: 20, blocks: [block(1, ""), block(2, "a")] }],
      [match(2)],
    );
    expect(locations[0]?.offset).toBe(20);
  });
  it("clamps coordinates and handles an unmeasured viewport", () => {
    expect(overviewY(-10, 100, 50)).toBe(0);
    expect(overviewY(200, 100, 50)).toBe(49);
    expect(overviewY(50, 100, 50)).toBe(25);
    expect(overviewY(Number.NaN, 100, 50)).toBe(0);
    expect(overviewY(1, 0, 50)).toBe(0);
    expect(overviewTicks([], 10, 0)).toEqual([]);
  });
  it("draws at least four pixels and merges gaps of two pixels", () => {
    const locations = [10, 16, 40].map((offset, index) => ({ offset, match: match(index) }));
    expect(overviewTicks(locations, 100, 100)).toEqual([
      { y: 8, height: 10 },
      { y: 38, height: 4 },
    ]);
  });
  it("keeps edge ticks inside short rails", () => {
    const locations = [
      { offset: 0, match: match(1) },
      { offset: 100, match: match(2) },
    ];
    expect(overviewTicks(locations, 100, 2)).toEqual([{ y: 0, height: 2 }]);
  });
  it("bounds dense tick painting by viewport pixels", () => {
    const locations = Array.from({ length: 5000 }, (_, index) => ({
      offset: index,
      match: match(1, index, index),
    }));
    const ticks = overviewTicks(locations, 5000, 120);
    expect(ticks.length).toBeLessThanOrEqual(120);
    expect(ticks[0]?.y).toBe(0);
    expect(ticks.every((tick) => tick.height >= 4 && tick.y + tick.height <= 120)).toBe(true);
    expect(nearestOverviewMatch(locations, 4321.1)?.occurrence).toBe(4321);
  });
  it("selects real matches at endpoints and breaks equal-distance ties consistently", () => {
    const one = match(1),
      two = match(2);
    const locations = [
      { offset: 10, match: one },
      { offset: 30, match: two },
    ];
    expect(nearestOverviewMatch([], 0)).toBeUndefined();
    expect(nearestOverviewMatch(locations, -100)).toBe(one);
    expect(nearestOverviewMatch(locations, 100)).toBe(two);
    expect(nearestOverviewMatch(locations, 20)).toBe(one);
    expect(nearestOverviewMatch(locations, 21)).toBe(two);
  });
});
