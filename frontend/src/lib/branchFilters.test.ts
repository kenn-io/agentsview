import { describe, expect, it } from "vite-plus/test";
import {
  NO_BRANCH_FILTER_TOKEN,
  branchFilterValuesEqual,
  branchFilterToken,
  branchLabel,
  branchTokenLabel,
  branchPickerValues,
  reconcileBranchFilterValues,
  scopeBranchFilterValues,
} from "./branchFilters.js";

describe("branch filter compatibility", () => {
  it("compares qualified identities before falling back to branch names", () => {
    expect(
      branchFilterValuesEqual(
        branchFilterToken("proj-a", "main"),
        branchFilterToken("proj-b", "main"),
      ),
    ).toBe(false);
    expect(branchFilterValuesEqual("main", branchFilterToken("proj-a", "main"))).toBe(true);
  });

  it("matches a legacy project label to the opaque key that stands for it", () => {
    const labels = new Map([
      ["pl1:sha256:alpha", "alpha"],
      ["pl1:sha256:beta", "beta"],
    ]);
    const lookup = (key: string) => labels.get(key);
    const legacy = branchFilterToken("alpha", "main");

    expect(
      branchFilterValuesEqual(legacy, branchFilterToken("pl1:sha256:alpha", "main"), lookup),
    ).toBe(true);
    expect(
      branchFilterValuesEqual(branchFilterToken("pl1:sha256:beta", "main"), legacy, lookup),
    ).toBe(false);
    expect(
      branchFilterValuesEqual(legacy, branchFilterToken("pl1:sha256:unknown", "main"), lookup),
    ).toBe(false);
  });

  it("normalizes legacy picker values and preserves no-branch selections", () => {
    expect(
      branchPickerValues([
        branchFilterToken("proj-a", "main"),
        branchFilterToken("proj-b", "main"),
        branchFilterToken("proj-a", ""),
      ]),
    ).toEqual(["main", NO_BRANCH_FILTER_TOKEN]);
  });

  it("preserves existing legacy values while adding plain picker values", () => {
    expect(
      reconcileBranchFilterValues(
        [branchFilterToken("proj-a", "main"), branchFilterToken("proj-b", "dev")],
        ["main", "feature"],
      ),
    ).toEqual([branchFilterToken("proj-a", "main"), "feature"]);
  });

  it("reconciles every qualified value selected by one plain branch", () => {
    const first = branchFilterToken("proj-a", "main");
    const second = branchFilterToken("proj-b", "main");

    expect(reconcileBranchFilterValues([first, second], ["main"])).toEqual([first, second]);
    expect(reconcileBranchFilterValues([second, first], ["main"])).toEqual([second, first]);
  });

  it("uses sentinel values that cannot collide with legal Git branch names", () => {
    // oxlint-disable-next-line no-control-regex -- intentional: the sentinels must contain a control character
    expect(NO_BRANCH_FILTER_TOKEN).toMatch(/[\u0000-\u001f]/);
    expect(
      branchPickerValues(["__agentsview_no_branch__", "__agentsview_no_branch_match__"]),
    ).toEqual(["__agentsview_no_branch__", "__agentsview_no_branch_match__"]);
  });

  it("keeps plain names when scoping and drops conflicting legacy pairs", () => {
    expect(
      scopeBranchFilterValues(
        ["main", branchFilterToken("proj-a", "dev"), branchFilterToken("proj-b", "feature")],
        "proj-a",
      ),
    ).toEqual(["main", branchFilterToken("proj-a", "dev")]);
  });

  it("preserves opaque project identities for server-side scoping", () => {
    const opaque = branchFilterToken("pl1:sha256:alpha", "main");
    expect(scopeBranchFilterValues([opaque, branchFilterToken("beta", "dev")], "alpha")).toEqual([
      opaque,
    ]);
  });
});

describe("branch filter labels", () => {
  it("keeps empty branch labels distinct from a real unknown branch", () => {
    const noBranch = "(no branch)";
    expect(branchLabel("proj", "", noBranch)).toBe("proj/(no branch)");
    expect(branchLabel("proj", "unknown", noBranch)).toBe("proj/unknown");
    expect(branchTokenLabel(branchFilterToken("proj", ""), noBranch)).toBe("proj/(no branch)");
    expect(branchTokenLabel(branchFilterToken("proj", ""), "No branch")).toBe("proj/No branch");
  });
});
