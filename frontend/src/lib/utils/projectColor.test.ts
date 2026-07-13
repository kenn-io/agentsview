import { describe, it, expect } from "vite-plus/test";
import {
  projectColor,
  PROJECT_PALETTE,
  seriesColorMap,
} from "./projectColor.js";

describe("projectColor", () => {
  it("returns a palette color for any non-empty string", () => {
    expect(PROJECT_PALETTE).toContain(projectColor("agentsview"));
  });

  it("is deterministic", () => {
    expect(projectColor("agentsview")).toBe(projectColor("agentsview"));
  });

  it("maps empty input to the muted fallback", () => {
    expect(projectColor("")).toBe("var(--text-muted)");
  });

  it("spreads different names across the palette", () => {
    const names = [
      "agentsview", "quokka", "arrow-rs", "side-quests",
      "infrastructure", "blog", "experiments", "docs",
      "dotfiles", "playground", "sandbox", "notes",
    ];
    const seen = new Set(names.map(projectColor));
    expect(seen.size).toBeGreaterThanOrEqual(6);
  });

  it("resolves the reported model collision", () => {
    const ids = ["claude-sonnet-5", "claude-opus-4-8"];
    expect(projectColor(ids[0]!)).toBe(projectColor(ids[1]!));
    expect(seriesColorMap(ids).get(ids[0]!)).not.toBe(
      seriesColorMap(ids).get(ids[1]!),
    );
  });

  it("is stable across permutations and duplicates", () => {
    const ids = ["claude-sonnet-5", "claude-opus-4-8", "claude-sonnet-5"];
    expect([...seriesColorMap(ids)]).toEqual([
      ...seriesColorMap([...ids].reverse()),
    ]);
  });

  it("keeps empty and other identifiers muted", () => {
    const colors = seriesColorMap(["", "__other__"]);
    expect(colors.get("")).toBe("var(--text-muted)");
    expect(colors.get("__other__")).toBe("var(--text-muted)");
  });

  it("uses every palette slot before deterministic overflow", () => {
    const ids = Array.from({ length: PROJECT_PALETTE.length }, (_, i) => `id-${i}`);
    const atCapacity = seriesColorMap(ids);
    expect(new Set(ids.map((id) => atCapacity.get(id))).size).toBe(
      PROJECT_PALETTE.length,
    );

    const overflowIds = [...ids, "id-overflow"];
    const overflow = seriesColorMap(overflowIds);
    const repeat = seriesColorMap([...overflowIds].reverse());
    expect(overflow).toEqual(repeat);
    expect(PROJECT_PALETTE).toContain(overflow.get("id-overflow"));
  });
});
