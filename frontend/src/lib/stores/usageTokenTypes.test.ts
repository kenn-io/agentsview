import { describe, expect, it } from "vite-plus/test";
import {
  ALL_TOKEN_TYPES,
  selectedTokenTypesFromParams,
  sumSelectedTokens,
  withSelectedTokenTypes,
} from "./usageTokenTypes.js";

describe("selectedTokenTypesFromParams", () => {
  it.each([
    [{}, ALL_TOKEN_TYPES],
    [{ token_types: "" }, ALL_TOKEN_TYPES],
    [{ token_types: "output" }, ["output"]],
    [{ token_types: "output,input,output" }, ["input", "output"]],
    [{ token_types: "output,unknown" }, ALL_TOKEN_TYPES],
  ] as const)("maps %o to the canonical selection", (params, expected) => {
    expect(selectedTokenTypesFromParams(params)).toEqual(expected);
  });
});

describe("withSelectedTokenTypes", () => {
  it("writes a non-default token selection in stable order", () => {
    expect(
      withSelectedTokenTypes({ view: "tokens", project: "demo" }, ["output", "input"], "token"),
    ).toEqual({
      view: "tokens",
      project: "demo",
      token_types: "input,output",
    });
  });

  it("omits all-token and cost-mode selections", () => {
    expect(
      withSelectedTokenTypes({ view: "tokens", token_types: "output" }, ALL_TOKEN_TYPES, "token"),
    ).toEqual({ view: "tokens" });
    expect(
      withSelectedTokenTypes({ project: "demo", token_types: "output" }, ["output"], "cost"),
    ).toEqual({ project: "demo" });
  });
});

describe("sumSelectedTokens", () => {
  const breakdown = {
    inputTokens: 100,
    cacheCreationTokens: 40,
    cacheReadTokens: 800,
    outputTokens: 25,
  };

  it("sums only the selected token economics", () => {
    expect(sumSelectedTokens(breakdown, ["cache_read", "output"])).toBe(825);
  });

  it("preserves the current all-token total by default", () => {
    expect(sumSelectedTokens(breakdown, ALL_TOKEN_TYPES)).toBe(965);
  });
});
