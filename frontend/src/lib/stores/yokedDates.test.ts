import { describe, expect, it, vi } from "vite-plus/test";
import {
  YokedDatesStore,
  panelDateState,
  panelDateToSessionFilterParams,
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
  it("defaults to disabled with no shared range", () => {
    const store = new YokedDatesStore(fakeStorage());

    expect(store.enabled).toBe(false);
    expect(store.range).toBeNull();
    expect(store.seedForPanel()).toBeNull();
  });

  it("hydrates an enabled version 2 rolling range", () => {
    const storage = fakeStorage({
      "yoked-dates": JSON.stringify({
        version: 2,
        enabled: true,
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

    expect(store.enabled).toBe(true);
    expect(store.range).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "rolling",
      windowDays: 7,
      updatedAt: 123,
    });
  });

  it.each([
    {
      name: "valid range",
      range: {
        from: "2026-06-01",
        to: "2026-06-07",
        mode: "fixed",
        updatedAt: 123,
      },
    },
    { name: "malformed range", range: { from: "invalid" } },
  ])(
    "migrates a version 1 $name to disabled empty state",
    ({ range }) => {
      const storage = fakeStorage({
        "yoked-dates": JSON.stringify({ version: 1, range }),
      });

      const store = new YokedDatesStore(storage);

      expect(store.enabled).toBe(false);
      expect(store.range).toBeNull();
      expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
        version: 2,
        enabled: false,
        range: null,
      });
    },
  );

  it.each([
    {
      name: "valid range",
      range: {
        from: "2026-06-01",
        to: "2026-06-07",
        mode: "fixed",
        updatedAt: 123,
      },
    },
    { name: "malformed range", range: { from: "invalid" } },
  ])(
    "normalizes a disabled version 2 $name to a null range",
    ({ range }) => {
      const storage = fakeStorage({
        "yoked-dates": JSON.stringify({
          version: 2,
          enabled: false,
          range,
        }),
      });

      const store = new YokedDatesStore(storage);

      expect(store.enabled).toBe(false);
      expect(store.range).toBeNull();
      expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
        version: 2,
        enabled: false,
        range: null,
      });
    },
  );

  it.each([
    "not json",
    JSON.stringify({ version: 99, enabled: true, range: null }),
    JSON.stringify({
      version: 2,
      enabled: true,
      range: { from: "bad" },
    }),
  ])("fails malformed or unsupported state toward disabled", (raw) => {
    const storage = fakeStorage({ "yoked-dates": raw });

    const store = new YokedDatesStore(storage);

    expect(store.enabled).toBe(false);
    expect(store.range).toBeNull();
  });

  it("publishes the latest disabled panel selection when enabled", () => {
    const storage = fakeStorage();
    const store = new YokedDatesStore(storage, () => 123);

    store.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
    });

    expect(store.enabled).toBe(false);
    expect(store.range).toBeNull();
    expect(store.seedForPanel()).toBeNull();
    expect(storage.getItem("yoked-dates")).toBeNull();

    store.setEnabled(true);

    expect(store.seedForPanel()).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
      updatedAt: 123,
    });
    expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
      version: 2,
      enabled: true,
      range: {
        from: "2026-06-01",
        to: "2026-06-07",
        mode: "fixed",
        updatedAt: 123,
      },
    });
  });

  it("publishes and seeds rolling ranges while enabled", () => {
    const storage = fakeStorage();
    const store = new YokedDatesStore(storage, () => 789);
    store.setEnabled(true);
    expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
      version: 2,
      enabled: true,
      range: null,
    });

    store.updateFromPanel({
      from: "2026-05-21",
      to: "2026-06-19",
      mode: "rolling",
      windowDays: 30,
    });

    expect(store.seedForPanel()).toEqual({
      from: "2026-05-21",
      to: "2026-06-19",
      mode: "rolling",
      windowDays: 30,
      updatedAt: 789,
    });
    expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
      version: 2,
      enabled: true,
      range: {
        from: "2026-05-21",
        to: "2026-06-19",
        mode: "rolling",
        windowDays: 30,
        updatedAt: 789,
      },
    });
  });

  it("restores an enabled preference before any range is published", () => {
    const storage = fakeStorage();
    const firstStore = new YokedDatesStore(storage);
    firstStore.setEnabled(true);

    const reloadedStore = new YokedDatesStore(storage);

    expect(reloadedStore.enabled).toBe(true);
    expect(reloadedStore.range).toBeNull();
    expect(reloadedStore.seedForPanel()).toBeNull();
  });

  it("disabling clears the shared range atomically", () => {
    const storage = fakeStorage();
    const store = new YokedDatesStore(storage, () => 123);
    store.setEnabled(true);
    store.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
    });

    store.setEnabled(false);

    expect(store.enabled).toBe(false);
    expect(store.range).toBeNull();
    expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
      version: 2,
      enabled: false,
      range: null,
    });
  });

  it("clear persists an empty range without changing the preference", () => {
    const storage = fakeStorage();
    const store = new YokedDatesStore(storage, () => 123);
    store.setEnabled(true);
    store.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
    });

    store.clear();

    expect(store.enabled).toBe(true);
    expect(store.range).toBeNull();
    expect(JSON.parse(storage.getItem("yoked-dates")!)).toEqual({
      version: 2,
      enabled: true,
      range: null,
    });
  });

  it("fails safely when storage reads throw", () => {
    const storage = fakeStorage();
    storage.getItem = () => {
      throw new Error("storage unavailable");
    };
    let store: YokedDatesStore | undefined;

    expect(() => {
      store = new YokedDatesStore(storage);
    }).not.toThrow();
    expect(store).toBeDefined();
    expect(store!.enabled).toBe(false);
    expect(store!.range).toBeNull();
  });

  it("retains current-tab state when storage writes throw", () => {
    const storage = fakeStorage();
    storage.setItem = () => {
      throw new Error("storage full");
    };
    const store = new YokedDatesStore(storage, () => 123);

    expect(() => {
      store.setEnabled(true);
      store.updateFromPanel({
        from: "2026-06-01",
        to: "2026-06-07",
      });
    }).not.toThrow();
    expect(store.enabled).toBe(true);
    expect(store.range).toEqual({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
      updatedAt: 123,
    });
  });
});

describe("yoked date adapters", () => {
  it("maps a Sessions rolling window before materialized bounds", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-07-10T12:00:00"));
      expect(
        sessionParamsToPanelDate({
          window_days: "30",
          date_from: "2026-01-01",
          date_to: "2026-01-07",
        }),
      ).toEqual({
        from: "2026-06-11",
        to: "2026-07-10",
        mode: "rolling",
        windowDays: 30,
      });
    } finally {
      vi.useRealTimers();
    }
  });

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

    // A 365-day rolling window now spans exactly 365 calendar days inclusive
    // of today, which fits the Activity limit; 366 is the first size beyond it.
    expect(
      rangeToActivityParams(
        {
          from: "2025-06-19",
          to: "2026-06-19",
          mode: "rolling",
          windowDays: 366,
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

  it("materializes rolling panel dates for session request filters", () => {
    expect(
      panelDateToSessionFilterParams({
        from: "2026-05-21",
        to: "2026-06-19",
        mode: "rolling",
        windowDays: 30,
      }),
    ).toEqual({
      date_from: "2026-05-21",
      date_to: "2026-06-19",
    });
  });

  it("serializes rolling panel routes with rolling URL intent", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-19T12:00:00"));
      const range = {
        from: "2026-05-21",
        to: "2026-06-19",
        mode: "rolling" as const,
        windowDays: 30,
        updatedAt: 123,
      };

      expect(rangeToActivityParams(range)).toEqual({
        preset: "custom",
        from: "2026-05-21",
        to: "2026-06-19",
        window_days: "30",
      });
      expect(rangeToInsightParams(range)).toEqual({
        date_from: "2026-05-21",
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
          from: "2026-05-21",
          to: "2026-06-19",
          mode: "rolling",
          windowDays: 30,
          updatedAt: 123,
        },
        new Date("2026-07-04T12:00:00"),
      ),
    ).toEqual({
      from: "2026-06-05",
      to: "2026-07-04",
      mode: "rolling",
      windowDays: 30,
    });
  });
});
