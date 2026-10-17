import { describe, expect, it } from "vitest";
import { excludedModelGroups, groupedModelCsv, modelPickerItems, toggleGroupedModel } from "./model-groups.js";

const aliases = { a: "group", b: "group" };

describe("grouped model filters", () => {
  it.each([
    ["a,b,group,other", "group,other"],
    [" a, b, , other ", "group,other"],
    ["constructor", "constructor"],
    ["toString", "toString"],
    ["__proto__", "__proto__"],
    ["unknown", "unknown"],
    ["", ""],
  ])("displays %s as %s", (saved, displayed) => {
    expect(groupedModelCsv(saved, aliases)).toBe(displayed);
  });

  it.each([
    ["a,b,other", "group", "other"],
    [" a, b, , other ", "group", "other"],
    ["a,b,other", "a", "b,other"],
    ["group,a,other", "group", "other"],
    ["a", "other", "a,other"],
    ["", "group", "group"],
  ])("toggles %s by %s to %s", (saved, name, result) => {
    expect(toggleGroupedModel(saved, name, aliases)).toBe(result);
  });

  it.each([
    ["a", "", 1],
    [" a, , other ", "other", 1],
    ["a,b,other", "group,other", 0],
    ["a,group", "group", 0],
    ["unknown", "unknown", 0],
    ["", "", 0],
  ])("shows %s in exclude mode as %s with %i partly hidden", (saved, csv, partial) => {
    expect(excludedModelGroups(saved, aliases)).toEqual({ csv, partial });
  });

  it("groups and sorts picker items while describing selected members", () => {
    expect(modelPickerItems(["group"], " a, , other ", aliases)).toEqual([
      { name: "group", description: "a" },
      { name: "other", description: undefined },
    ]);
    expect(modelPickerItems(["z", "b", "a", "other"], "b,a,unknown", aliases)).toEqual([
      { name: "group", description: "b, a" },
      { name: "other", description: undefined },
      { name: "unknown", description: undefined },
      { name: "z", description: undefined },
    ]);
    expect(modelPickerItems(["a", "b"], "a,group", aliases)).toEqual([
      { name: "group", description: undefined },
    ]);
    expect(modelPickerItems([], "", aliases)).toEqual([]);
  });
});
