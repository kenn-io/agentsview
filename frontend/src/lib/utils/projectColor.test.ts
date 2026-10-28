import { describe, it, expect } from "vite-plus/test";
import { chartSeriesColorMap } from "./chartPalette.js";
import { projectColor, PROJECT_PALETTE } from "./projectColor.js";

describe("projectColor", () => {
  it("returns a palette color for any non-empty string", () => {
    expect(PROJECT_PALETTE).toContain(projectColor("agentsview"));
  });

  it("maps empty input to the muted fallback", () => {
    expect(projectColor("")).toBe("var(--text-muted)");
  });

  it("spreads different names across the palette", () => {
    const names = [
      "agentsview",
      "quokka",
      "arrow-rs",
      "side-quests",
      "infrastructure",
      "blog",
      "experiments",
      "docs",
      "dotfiles",
      "playground",
      "sandbox",
      "notes",
    ];
    const seen = new Set(names.map(projectColor));
    expect(seen.size).toBeGreaterThanOrEqual(6);
  });

  it("resolves the reported model collision", () => {
    const ids = ["claude-sonnet-5", "claude-opus-4-8"];
    expect(projectColor(ids[0]!)).toBe(projectColor(ids[1]!));
    expect(chartSeriesColorMap(ids, "agentsview").get(ids[0]!)).not.toBe(
      chartSeriesColorMap(ids, "agentsview").get(ids[1]!),
    );
  });

  it("is stable across permutations and duplicates", () => {
    const ids = ["claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5", "claude-sonnet-5"];
    const shuffled = ["claude-haiku-4-5", "claude-sonnet-5", "claude-opus-4-8"];
    expect([...chartSeriesColorMap(ids, "agentsview")]).toEqual([
      ...chartSeriesColorMap(shuffled, "agentsview"),
    ]);
  });

  it("keeps empty and other identifiers muted", () => {
    const colors = chartSeriesColorMap(["", "__other__"], "agentsview");
    expect(colors.get("")).toBe("var(--text-muted)");
    expect(colors.get("__other__")).toBe("var(--text-muted)");
  });

  it("uses every palette slot before deterministic overflow", () => {
    const ids = Array.from({ length: PROJECT_PALETTE.length }, (_, i) => `id-${i}`);
    const atCapacity = chartSeriesColorMap(ids, "agentsview");
    expect(new Set(ids.map((id) => atCapacity.get(id))).size).toBe(PROJECT_PALETTE.length);

    const overflowIds = [...ids, "id-overflow"];
    const overflow = chartSeriesColorMap(overflowIds, "agentsview");
    const repeat = chartSeriesColorMap([...overflowIds].reverse(), "agentsview");
    expect(overflow).toEqual(repeat);
    expect(PROJECT_PALETTE).toContain(overflow.get("id-overflow"));
  });
});
