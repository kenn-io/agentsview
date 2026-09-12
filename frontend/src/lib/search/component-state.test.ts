import { describe, expect, it } from "vite-plus/test";
import { searchCollapsed, toolSearchKey } from "./component-state.js";

describe("tool search scope", () => {
  it("leaves embedded subagents outside the parent registry", () => {
    expect(toolSearchKey(undefined, "tool-input")).toBeUndefined();
    expect(toolSearchKey(undefined, "tool-output")).toBeUndefined();
    expect(toolSearchKey(undefined, "tool-history", 0)).toBeUndefined();
  });
  it("keeps structured inputs and outputs distinct", () => {
    expect(toolSearchKey({ ordinal: 7, callIdx: 2 }, "tool-input")).toBe("7:tool-input:2");
    expect(toolSearchKey({ ordinal: 7, callIdx: 2 }, "tool-output")).toBe("7:tool-output:2");
  });
  it("uses the event's array position, including zero", () => {
    expect(toolSearchKey({ ordinal: 7, callIdx: 2 }, "tool-history", 0)).toBe("7:tool-history:2.0");
    expect(toolSearchKey({ ordinal: 7, callIdx: 2 }, "tool-history", 1)).toBe("7:tool-history:2.1");
  });
  it("preserves legacy segment identity", () => {
    expect(toolSearchKey({ ordinal: 0, callIdx: "seg1" }, "tool-input")).toBe("0:tool-input:seg1");
    expect(toolSearchKey({ ordinal: 0, callIdx: "seg1" }, "tool-history", 0)).toBe(
      "0:tool-history:seg1.0",
    );
  });
});

describe("search disclosure", () => {
  it("does not expand a block merely because it has matches", () => {
    expect(searchCollapsed(true, false, 1, -1)).toBe(true);
  });
  it("opens the current block without changing user state", () => {
    expect(searchCollapsed(true, true, 1, -1)).toBe(false);
    expect(searchCollapsed(true, false, 2, -1)).toBe(true);
  });
  it("respects a manual collapse throughout the same navigation", () => {
    expect(searchCollapsed(true, true, 3, 3)).toBe(true);
    expect(searchCollapsed(true, true, 3, 3)).toBe(true);
  });
  it("reveals again on a subsequent navigation", () => {
    expect(searchCollapsed(true, true, 4, 3)).toBe(false);
  });
  it("preserves a manually expanded block after leaving the match", () => {
    expect(searchCollapsed(false, false, 4, 3)).toBe(false);
  });
  it("keeps output and history overrides independent", () => {
    expect(searchCollapsed(true, false, 4, -1)).toBe(true);
    expect(searchCollapsed(true, true, 4, -1)).toBe(false);
    expect(searchCollapsed(true, true, 4, 4)).toBe(true);
  });
});
