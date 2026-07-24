import { describe, expect, it } from "vitest";
import {
  NO_BRANCH_FILTER_TOKEN,
  NO_BRANCH_MATCH_TOKEN,
  branchFilterToken,
  branchLabel,
  branchTokenLabel,
  branchPickerValues,
  intersectBranchFilterValues,
  reconcileBranchFilterValues,
  scopeBranchFilterValues,
} from "./branchFilters.js";

describe("branch filter compatibility", () => {
  it("normalizes legacy picker values and preserves no-branch selections", () => {
    expect(branchPickerValues([
      branchFilterToken("proj-a", "main"),
      branchFilterToken("proj-b", "main"),
      branchFilterToken("proj-a", ""),
    ])).toEqual(["main", NO_BRANCH_FILTER_TOKEN]);
  });

  it("intersects plain branch names with legacy project-pair tokens", () => {
    expect(intersectBranchFilterValues(
      ["main", "dev"],
      [branchFilterToken("proj-a", "main"), "feature"],
    )).toEqual([branchFilterToken("proj-a", "main")]);
  });

  it("does not intersect same-named legacy branches from different projects", () => {
    expect(intersectBranchFilterValues(
      [branchFilterToken("proj-a", "main")],
      [branchFilterToken("proj-b", "main")],
    )).toEqual([]);
  });

  it("preserves qualified identity regardless of intersection order", () => {
    const qualified = branchFilterToken("proj-a", "main");
    expect(intersectBranchFilterValues([qualified], ["main"])).toEqual([
      qualified,
    ]);
    expect(intersectBranchFilterValues(["main"], [qualified])).toEqual([
      qualified,
    ]);
  });

  it("preserves existing legacy values while adding plain picker values", () => {
    expect(reconcileBranchFilterValues(
      [
        branchFilterToken("proj-a", "main"),
        branchFilterToken("proj-b", "dev"),
      ],
      ["main", "feature"],
    )).toEqual([branchFilterToken("proj-a", "main"), "feature"]);
  });

  it("uses sentinel values that cannot collide with legal Git branch names", () => {
    expect(NO_BRANCH_FILTER_TOKEN).toMatch(/[\u0000-\u001f]/);
    expect(NO_BRANCH_MATCH_TOKEN).toMatch(/[\u0000-\u001f]/);
    expect(branchPickerValues([
      "__agentsview_no_branch__",
      "__agentsview_no_branch_match__",
    ])).toEqual([
      "__agentsview_no_branch__",
      "__agentsview_no_branch_match__",
    ]);
  });

  it("keeps plain names when scoping and drops conflicting legacy pairs", () => {
    expect(scopeBranchFilterValues([
      "main",
      branchFilterToken("proj-a", "dev"),
      branchFilterToken("proj-b", "feature"),
    ], "proj-a")).toEqual(["main", branchFilterToken("proj-a", "dev")]);
  });
});

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
