import { describe, expect, it } from "vite-plus/test";
import {
  SESSION_FILTER_KEYS,
  hasSessionRouteDateIntent,
  sessionDateIntentCleared,
  sessionRouteParamsForDetailExit,
  sessionRouteParamsForFilters,
} from "./sessionRouteParams.js";

describe("session route params", () => {
  it("preserves rolling intent when materialized date bounds still match", () => {
    const params = sessionRouteParamsForFilters(
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
        project: "agentsview",
      },
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
        window_days: "30",
      },
      new Date(2026, 5, 20, 12),
    );

    expect(params).toEqual({
      date_from: "2026-05-21",
      date_to: "2026-06-20",
      project: "agentsview",
      window_days: "30",
    });
  });

  it("preserves direct rolling links during first materialized date writeback", () => {
    const params = sessionRouteParamsForFilters(
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
      },
      { window_days: "30" },
      new Date(2026, 5, 20, 12),
    );

    expect(params).toEqual({
      date_from: "2026-05-21",
      date_to: "2026-06-20",
      window_days: "30",
    });
  });

  it("preserves stale materialized rolling links after date rollover", () => {
    const params = sessionRouteParamsForFilters(
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
      },
      {
        date_from: "2026-05-20",
        date_to: "2026-06-19",
        window_days: "30",
      },
      new Date(2026, 5, 20, 12),
    );

    expect(params).toEqual({
      date_from: "2026-05-21",
      date_to: "2026-06-20",
      window_days: "30",
    });
  });

  it("does not preserve rolling intent for unrelated fixed dates", () => {
    const params = sessionRouteParamsForFilters(
      {
        date_from: "2026-01-01",
        date_to: "2026-01-31",
      },
      {
        date_from: "2026-05-20",
        date_to: "2026-06-19",
        window_days: "30",
      },
      new Date(2026, 5, 20, 12),
    );

    expect(params).toEqual({
      date_from: "2026-01-01",
      date_to: "2026-01-31",
    });
  });

  it("drops rolling intent when materialized date filters are cleared", () => {
    const params = sessionRouteParamsForFilters(
      { project: "agentsview" },
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
        window_days: "30",
        project: "agentsview",
      },
      new Date(2026, 5, 20, 12),
    );

    expect(params).toEqual({ project: "agentsview" });
  });

  it("detects removed session date intent", () => {
    expect(sessionDateIntentCleared(
      {
        date_from: "2026-05-21",
        date_to: "2026-06-20",
        window_days: "30",
      },
      { project: "agentsview" },
    )).toBe(true);
    expect(sessionDateIntentCleared(
      { window_days: "30" },
      { window_days: "30", project: "agentsview" },
    )).toBe(false);
    expect(sessionDateIntentCleared(
      { project: "agentsview" },
      { project: "agentsview" },
    )).toBe(false);
  });

  it("only treats window_days as session date intent on sessions routes", () => {
    expect(hasSessionRouteDateIntent("sessions", {
      window_days: "30",
    })).toBe(true);
    expect(hasSessionRouteDateIntent("usage", {
      window_days: "30",
    })).toBe(false);
    expect(hasSessionRouteDateIntent("insights", {
      window_days: "30",
    })).toBe(false);
  });

  it("prefers direct detail URL params over saved filters on exit", () => {
    const params = sessionRouteParamsForDetailExit(
      { project: "saved", agent: "codex" },
      { project: "direct", window_days: "14", msg: "stale" },
    );

    expect(params).toEqual({
      project: "direct",
      window_days: "14",
    });
  });

  it("tracks rolling window and termination as session route params", () => {
    expect(SESSION_FILTER_KEYS.has("window_days")).toBe(true);
    expect(SESSION_FILTER_KEYS.has("termination")).toBe(true);
  });
});
