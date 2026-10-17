import { describe, expect, it } from "vitest";
import { groupedModelCsv, toggleGroupedModel } from "./model-groups.js";

const aliases = { a: "group", b: "group" };

describe("grouped model filters", () => {
  it.each([
    ["a,b,group,other", "group,other"],
    ["unknown", "unknown"],
    ["", ""],
  ])("displays %s as %s", (saved, displayed) => {
    expect(groupedModelCsv(saved, aliases)).toBe(displayed);
  });

  it.each([
    ["a,b,other", "group", "other"],
    ["a,b,other", "a", "b,other"],
    ["group,a,other", "group", "other"],
    ["a", "other", "a,other"],
    ["", "group", "group"],
  ])("toggles %s by %s to %s", (saved, name, result) => {
    expect(toggleGroupedModel(saved, name, aliases)).toBe(result);
  });
});
