import { describe, expect, it } from "vite-plus/test";
import {
  excludedModelGroups,
  groupedModelCsv,
  modelPickerItems,
  toggleGroupedModel,
} from "./model-groups.js";

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
    ["a,b,other", "a", "b,other"],
    ["group,a,other", "group", "other"],
    ["a", "other", "a,other"],
    ["", "group", "group"],
    ["a", "group", "group", true],
    ["group", "group", "", true],
  ])("toggles %s by %s to %s", (saved, name, result, exclude = false) => {
    expect(toggleGroupedModel(saved, name, aliases, exclude)).toBe(result);
  });

  it.each([
    ["a", "", 1],
    ["a,b,other", "other", 2],
    // A raw model named "group" remains visible when every alias is excluded.
    ["a,b", "", 2],
    ["a,group", "group", 0],
    ["unknown", "unknown", 0],
    ["", "", 0],
  ])("shows %s in exclude mode as %s with %i partly hidden", (saved, csv, partial) => {
    expect(excludedModelGroups(saved, aliases)).toEqual({ csv, partial });
  });

  it("groups and sorts picker items while describing selected members", () => {
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
