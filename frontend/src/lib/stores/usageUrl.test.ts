import { describe, expect, it } from "vite-plus/test";
import { buildUsageUrlParams, mergeUsageAndSessionUrlParams, parseWindowDays } from "./usageUrl.js";

describe("buildUsageUrlParams", () => {
  it("omits from/to when isPinned is false with default window, includes header filters", () => {
    const params = buildUsageUrlParams({
      from: "2026-03-26",
      to: "2026-04-25",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "p1",
      excludedProjectKeys: "pk1",
      excludedAgents: "a1",
      excludedModels: "m1",
    });
    expect(params).toEqual({
      exclude_project: "p1",
      exclude_agent: "a1",
      exclude_model: "m1",
    });
  });

  it("includes from/to when isPinned is true", () => {
    const params = buildUsageUrlParams({
      from: "2026-01-01",
      to: "2026-01-15",
      isPinned: true,
      windowDays: 30,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });

  it("returns empty object when nothing is set", () => {
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({});
  });

  it("omits empty from/to even when pinned", () => {
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: true,
      windowDays: 30,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({});
  });

  it("emits window_days for unpinned non-default windows", () => {
    const params = buildUsageUrlParams({
      from: "2026-04-19",
      to: "2026-04-25",
      isPinned: false,
      windowDays: 7,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({ window_days: "7" });
  });

  it("omits window_days when isPinned is true", () => {
    const params = buildUsageUrlParams({
      from: "2026-01-01",
      to: "2026-01-15",
      isPinned: true,
      windowDays: 7,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });
});

describe("mergeUsageAndSessionUrlParams", () => {
  it("merges overlapping CSV params instead of overwriting usage filters", () => {
    expect(
      mergeUsageAndSessionUrlParams(
        {
          exclude_project: "alpha,beta",
          model: "gpt-5.5",
        },
        {
          exclude_project: "unknown,beta",
          machine: "host-a",
        },
      ),
    ).toEqual({
      exclude_project: "alpha,beta,unknown",
      model: "gpt-5.5",
      machine: "host-a",
    });
  });

  it("omits hidden session date params from usage URLs", () => {
    expect(
      mergeUsageAndSessionUrlParams(
        {
          from: "2026-02-01",
          to: "2026-02-07",
        },
        {
          date: "2026-01-15",
          date_from: "2026-01-01",
          date_to: "2026-01-31",
          project: "agentsview",
        },
      ),
    ).toEqual({
      from: "2026-02-01",
      to: "2026-02-07",
      project: "agentsview",
    });
  });

  it("preserves supported termination params in usage URLs", () => {
    expect(
      mergeUsageAndSessionUrlParams(
        {
          from: "2026-02-01",
          to: "2026-02-07",
        },
        {
          termination: "unclean",
          project: "agentsview",
        },
      ),
    ).toEqual({
      from: "2026-02-01",
      to: "2026-02-07",
      termination: "unclean",
      project: "agentsview",
    });
  });
});

describe("parseWindowDays", () => {
  it("returns the parsed integer for valid positive integers", () => {
    expect(parseWindowDays("7")).toBe(7);
    expect(parseWindowDays("365")).toBe(365);
  });

  it("rejects non-positive, non-integer, and malformed values", () => {
    expect(parseWindowDays(undefined)).toBeNull();
    expect(parseWindowDays("")).toBeNull();
    expect(parseWindowDays("0")).toBeNull();
    expect(parseWindowDays("-7")).toBeNull();
    expect(parseWindowDays("7.5")).toBeNull();
    expect(parseWindowDays("7d")).toBeNull();
    expect(parseWindowDays("abc")).toBeNull();
  });

  it("accepts values up to the 100-year cap and rejects beyond", () => {
    expect(parseWindowDays("36500")).toBe(36500);
    expect(parseWindowDays("36501")).toBeNull();
    expect(parseWindowDays("1000000000")).toBeNull();
  });
});
