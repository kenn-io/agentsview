import { describe, expect, it } from "vitest";
import { groupedModelCsv, modelPickerItems, toggleGroupedModel } from "./model-groups.js";

const aliases = { a: "group", b: "group" };

describe("grouped model filters", () => {
  it.each([
    ["a,b,group,other", "group,other"],
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
  ])("toggles %s by %s to %s", (saved, name, result) => {
    expect(toggleGroupedModel(saved, name, aliases)).toBe(result);
  });

  it.each(["constructor", "toString", "__proto__"])("selects and clears the %s group", (name) => {
    const aliases = { a: name, b: name };
    expect(groupedModelCsv("a,b", aliases)).toBe(name);
    expect(groupedModelCsv(name, aliases)).toBe(name);
    expect(toggleGroupedModel("", name, aliases)).toBe(name);
    expect(toggleGroupedModel(name, name, aliases)).toBe("");
    expect(toggleGroupedModel("a,b,other", name, aliases)).toBe("other");
    expect(modelPickerItems([name, "a", "b"], "a", aliases)).toEqual([{ name, description: "a" }]);
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
