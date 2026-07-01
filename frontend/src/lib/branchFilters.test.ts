import { describe, expect, it } from "vitest";
import {
  branchFilterToken,
  branchLabel,
  branchTokenLabel,
} from "./branchFilters.js";

describe("branch filter labels", () => {
  it("keeps empty branch labels distinct from a real unknown branch", () => {
    const noBranch = "(no branch)";
    expect(branchLabel("proj", "", noBranch)).toBe("proj/(no branch)");
    expect(branchLabel("proj", "unknown", noBranch)).toBe("proj/unknown");
    expect(branchTokenLabel(branchFilterToken("proj", ""), noBranch)).toBe(
      "proj/(no branch)",
    );
    expect(branchTokenLabel(
      branchFilterToken("proj", ""),
      "No branch",
    )).toBe("proj/No branch");
  });
});
