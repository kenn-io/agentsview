import { describe, expect, it, vi } from "vite-plus/test";
import {
  YokedDatesStore,
  panelDateState,
  panelStateToRange,
  rangeToActivityParams,
  rangeToInsightParams,
  rangeToPanelDate,
  rangeToSessionParams,
  sessionParamsToPanelDate,
} from "./yokedDates.svelte.js";

function fakeStorage(initial: Record<string, string> = {}): Storage {
  const data = new Map(Object.entries(initial));
  return {
    get length() {
      return data.size;
    },
    clear() {
      data.clear();
    },
    getItem(key: string) {
      return data.get(key) ?? null;
    },
    key(index: number) {
      return Array.from(data.keys())[index] ?? null;
    },
    removeItem(key: string) {
      data.delete(key);
    },
    setItem(key: string, value: string) {
      data.set(key, value);
    },
  };
}

describe("YokedDatesStore", () => {
  it("defaults to no stored range", () => {
    const store = new YokedDatesStore(fakeStorage());

    expect(store.range).toBeNull();
  });

  it("hydrates valid stored state", () => {
    const storage = fakeStorage({
      "yoked-dates": JSON.stringify({
        version: 1,
        range: {
          from: "2026-06-01",
          to: "2026-06-07",
          mode: "rolling",
          windowDays: 7,
          updatedAt: 123,
        },
      }),
    });

    const store = new YokedDatesStore(storage);

    expect(store.range).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "rolling",
      windowDays: 7,
      updatedAt: 123,
    });
  });

  it("ignores malformed stored state", () => {
    const storage = fakeStorage({ "yoked-dates": "not json" });

    const store = new YokedDatesStore(storage);

    expect(store.range).toBeNull();
  });

  it("ignores unsupported storage versions", () => {
    const storage = fakeStorage({
      "yoked-dates": JSON.stringify({
        version: 2,
        range: {
          from: "2026-06-01",
          to: "2026-06-07",
          mode: "fixed",
          updatedAt: 123,
        },
      }),
    });

    const store = new YokedDatesStore(storage);

    expect(store.range).toBeNull();
  });

  it("writes panel changes unconditionally without an enabled flag", () => {
    const storage = fakeStorage();
    const store = new YokedDatesStore(storage, () => 123);

    store.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
    });

    expect(store.range).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
      updatedAt: 123,
    });
    expect(JSON.parse(storage.getItem("yoked-dates") ?? "{}")).toEqual({
      version: 1,
      range: {
        from: "2026-06-01",
        to: "2026-06-07",
        mode: "fixed",
        updatedAt: 123,
      },
    });
  });

  it("hydrates legacy disabled state as an always-linked range", () => {
    const store = new YokedDatesStore(fakeStorage({
      "yoked-dates": JSON.stringify({
        version: 1,
        enabled: false,
        range: {
          from: "2026-06-01",
          to: "2026-06-07",
          mode: "fixed",
          updatedAt: 456,
        },
      }),
    }));

    expect(store.range).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
      updatedAt: 456,
    });
  });

  it("preserves rolling window intent when writing from a panel", () => {
    const store = new YokedDatesStore(fakeStorage(), () => 789);

    store.updateFromPanel({
      from: "2026-05-21",
      to: "2026-06-19",
      mode: "rolling",
      windowDays: 30,
    });

    expect(store.range).toEqual({
      from: "2026-05-21",
      to: "2026-06-19",
      mode: "rolling",
      windowDays: 30,
      updatedAt: 789,
    });
  });
});

describe("yoked date adapters", () => {
  it("maps a sessions single date to a same-day range", () => {
    expect(sessionParamsToPanelDate({ date: "2026-06-19" })).toEqual({
      from: "2026-06-19",
      to: "2026-06-19",
      mode: "fixed",
    });
  });

  it("maps sessions date bounds to a range", () => {
    expect(
      sessionParamsToPanelDate({
        date_from: "2026-06-01",
        date_to: "2026-06-07",
      }),
    ).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });
  });

  it("maps a sessions lower date bound to a range ending today", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-19T12:00:00"));
      expect(
        sessionParamsToPanelDate({ date_from: "2026-06-01" }),
      ).toEqual({
        from: "2026-06-01",
        to: "2026-06-19",
        mode: "fixed",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("maps a sessions upper date bound using an available earliest date", () => {
    expect(
      sessionParamsToPanelDate(
        { date_to: "2026-06-07" },
        { earliest: "2026-05-01T14:30:00Z" },
      ),
    ).toEqual({
      from: "2026-05-01",
      to: "2026-06-07",
      mode: "fixed",
    });
  });

  it("maps a sessions upper date bound to a same-day range without an earliest date", () => {
    expect(sessionParamsToPanelDate({ date_to: "2026-06-07" })).toEqual({
      from: "2026-06-07",
      to: "2026-06-07",
      mode: "fixed",
    });
  });

  it("rejects incomplete and inverted panel ranges", () => {
    expect(panelDateState("", "2026-06-07")).toBeNull();
    expect(panelDateState("2026-06-08", "2026-06-07")).toBeNull();
    expect(
      panelStateToRange(
        { from: "2026-06-08", to: "2026-06-07" },
        123,
      ),
    ).toBeNull();
  });

  it("serializes same-day sessions ranges as date", () => {
    expect(
      rangeToSessionParams({
        from: "2026-06-19",
        to: "2026-06-19",
        mode: "fixed",
        updatedAt: 123,
      }),
    ).toEqual({ date: "2026-06-19" });
  });

  it("serializes panel-specific fixed ranges", () => {
    const range = {
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed" as const,
      updatedAt: 123,
    };

    expect(rangeToSessionParams(range)).toEqual({
      date_from: "2026-06-01",
      date_to: "2026-06-07",
    });
    expect(rangeToActivityParams(range)).toEqual({
      preset: "custom",
      from: "2026-06-01",
      to: "2026-06-07",
    });
    expect(rangeToInsightParams(range)).toEqual({
      date_from: "2026-06-01",
      date_to: "2026-06-07",
    });
  });

  it("skips Activity params for yoke ranges beyond the Activity max span", () => {
    expect(
      rangeToActivityParams({
        from: "2025-06-19",
        to: "2026-06-19",
        mode: "fixed",
        updatedAt: 123,
      }),
    ).toEqual({});

    expect(
      rangeToActivityParams(
        {
          from: "2025-06-19",
          to: "2026-06-19",
          mode: "rolling",
          windowDays: 365,
          updatedAt: 123,
        },
        new Date("2026-06-19T12:00:00"),
      ),
    ).toEqual({});

    expect(
      rangeToActivityParams({
        from: "2025-06-20",
        to: "2026-06-19",
        mode: "fixed",
        updatedAt: 123,
      }),
    ).toEqual({
      preset: "custom",
      from: "2025-06-20",
      to: "2026-06-19",
    });
  });

  it("serializes rolling session ranges as rolling URL intent", () => {
    const range = {
      from: "2026-05-20",
      to: "2026-06-19",
      mode: "rolling" as const,
      windowDays: 30,
      updatedAt: 123,
    };

    expect(rangeToSessionParams(range)).toEqual({
      window_days: "30",
    });
  });

  it("serializes rolling panel routes with rolling URL intent", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-19T12:00:00"));
      const range = {
        from: "2026-05-20",
        to: "2026-06-19",
        mode: "rolling" as const,
        windowDays: 30,
        updatedAt: 123,
      };

      expect(rangeToActivityParams(range)).toEqual({
        preset: "custom",
        from: "2026-05-20",
        to: "2026-06-19",
        window_days: "30",
      });
      expect(rangeToInsightParams(range)).toEqual({
        date_from: "2026-05-20",
        date_to: "2026-06-19",
        window_days: "30",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("materializes rolling ranges against the current day", () => {
    expect(
      rangeToPanelDate(
        {
          from: "2026-05-20",
          to: "2026-06-19",
          mode: "rolling",
          windowDays: 30,
          updatedAt: 123,
        },
        new Date("2026-07-04T12:00:00"),
      ),
    ).toEqual({
      from: "2026-06-04",
      to: "2026-07-04",
      mode: "rolling",
      windowDays: 30,
    });
  });
});
