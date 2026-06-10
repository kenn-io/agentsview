import { describe, it, expect } from "vite-plus/test";
import { projectColor, PROJECT_PALETTE } from "./projectColor.js";

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
});
