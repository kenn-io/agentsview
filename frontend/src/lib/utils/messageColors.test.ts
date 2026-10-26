import { describe, expect, it } from "vite-plus/test";
import {
  COLORBLIND_COLORS,
  colorIssues,
  CONTRAST_MODES,
  defaultRoleColor,
  MESSAGE_ROLES,
  parseStoredColors,
  roleColorProperties,
} from "./messageColors.js";

describe("colorIssues", () => {
  it("finds nothing wrong with the color-blind preset in any mode", () => {
    for (const role of MESSAGE_ROLES) {
      expect(colorIssues(role, COLORBLIND_COLORS[role]), role).toEqual([]);
    }
  });

  it("flags a pale yellow bar on light surfaces only", () => {
    const issues = colorIssues("user", "#f0e442");
    const marker = issues.find((issue) => issue.kind === "marker");
    expect(marker?.modes).toEqual(["light", "high-contrast-light"]);
    expect(marker?.ratio).toBeLessThan(marker!.required);
    expect(marker?.required).toBe(3);
  });

  it("flags label text that a near-white color washes out", () => {
    const text = colorIssues("assistant", "#fafafa").find((issue) => issue.kind === "text");
    expect(text?.modes).toContain("light");
    expect(text?.required).toBe(4.5);
  });

  it("flags a near-black color on dark surfaces only", () => {
    const issues = colorIssues("tool", "#111111");
    expect(issues.map((issue) => [issue.kind, issue.modes])).toEqual([
      ["text", ["dark", "high-contrast-dark"]],
      ["marker", ["dark", "high-contrast-dark"]],
    ]);
  });

  it("checks only the frame for code blocks", () => {
    const issues = colorIssues("code", "#16161e");
    expect(issues).toHaveLength(1);
    expect(issues[0]!.kind).toBe("marker");
    expect(issues[0]!.modes).toEqual(["dark", "high-contrast-dark"]);
  });

  it("ignores values that are not colors", () => {
    expect(colorIssues("user", "blue")).toEqual([]);
  });
});

describe("defaultRoleColor", () => {
  it("follows the high-contrast accent where the theme changes it", () => {
    expect(defaultRoleColor("user", "high-contrast-light")).not.toBe(
      defaultRoleColor("user", "light"),
    );
    expect(defaultRoleColor("assistant", "high-contrast-dark")).toBe(
      defaultRoleColor("assistant", "dark"),
    );
  });

  it("has a color for every role in every mode", () => {
    for (const mode of CONTRAST_MODES) {
      for (const role of MESSAGE_ROLES) {
        expect(defaultRoleColor(role, mode)).toMatch(/^#[0-9a-f]{6}$/);
      }
    }
  });
});

describe("roleColorProperties", () => {
  it("picks an icon letter color that reads on the chosen fill", () => {
    expect(roleColorProperties("user", "#f0e442")["--role-user-accent-foreground"]).toBe("#000000");
    expect(roleColorProperties("user", "#0b1e3f")["--role-user-accent-foreground"]).toBe("#ffffff");
  });

  it("tints the role's own background token", () => {
    expect(Object.keys(roleColorProperties("thinking", "#b8689a"))).toContain("--thinking-bg");
    expect(Object.keys(roleColorProperties("tool", "#009e73"))).toContain("--tool-bg");
  });

  it("turns on the code frame only for a custom code color", () => {
    expect(roleColorProperties("code", "#8f7d00")).toEqual({
      "--role-code-accent": "#8f7d00",
      "--role-code-frame-width": "2px",
    });
  });
});

describe("parseStoredColors", () => {
  it("keeps known roles with valid colors and normalizes them", () => {
    expect(
      parseStoredColors({ user: "0072B2", assistant: "nope", system: "#ffffff", tool: 7 }),
    ).toEqual({ user: "#0072b2" });
  });

  it("returns no colors for malformed payloads", () => {
    expect(parseStoredColors(null)).toEqual({});
    expect(parseStoredColors("#0072b2")).toEqual({});
  });
});
