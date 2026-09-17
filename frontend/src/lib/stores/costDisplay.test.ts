import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  COST_DISPLAY_STORAGE_KEY,
  CostDisplayStore,
  costDisplay,
} from "./costDisplay.svelte.js";

interface TestStorage {
  values: Map<string, string>;
  storage: Pick<Storage, "getItem" | "setItem">;
}

function testStorage(initial: Record<string, string> = {}): TestStorage {
  const values = new Map(Object.entries(initial));
  return {
    values,
    storage: {
      getItem: (key) => values.get(key) ?? null,
      setItem: (key, value) => values.set(key, value),
    },
  };
}

beforeEach(() => {
  localStorage.clear();
  costDisplay.setPreference("USD", null);
});

describe("CostDisplayStore", () => {
  it("defaults to USD when storage has no preference", () => {
    const { storage } = testStorage();
    expect(new CostDisplayStore(storage).preference).toEqual({
      currency: "USD",
      eurPerUsd: null,
    });
  });

  it("hydrates a valid applied pair", () => {
    const { storage } = testStorage({
      [COST_DISPLAY_STORAGE_KEY]: JSON.stringify({
        currency: "EUR",
        eurPerUsd: 0.9,
      }),
    });

    expect(new CostDisplayStore(storage).preference).toEqual({
      currency: "EUR",
      eurPerUsd: 0.9,
    });
  });

  it.each([
    ["null", null],
    ["an unknown currency", { currency: "GBP", eurPerUsd: 0.9 }],
    ["EUR without a rate", { currency: "EUR", eurPerUsd: null }],
    ["a zero rate", { currency: "USD", eurPerUsd: 0 }],
    ["a negative rate", { currency: "USD", eurPerUsd: -0.1 }],
    ["a nonnumeric rate", { currency: "USD", eurPerUsd: "0.9" }],
  ])("defaults safely for %s", (_name, value) => {
    const { storage } = testStorage({
      [COST_DISPLAY_STORAGE_KEY]: JSON.stringify(value),
    });

    expect(new CostDisplayStore(storage).preference).toEqual({
      currency: "USD",
      eurPerUsd: null,
    });
  });

  it("rejects invalid rates without changing the applied pair", () => {
    const { storage } = testStorage();
    const store = new CostDisplayStore(storage);

    expect(store.setPreference("EUR", 0.9)).toBe(true);
    for (const rate of [null, 0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(store.setPreference("EUR", rate)).toBe(false);
      expect(store.preference).toEqual({ currency: "EUR", eurPerUsd: 0.9 });
    }
  });

  it("stores valid pairs and keeps a remembered rate when switching to USD", () => {
    const { storage, values } = testStorage();
    const store = new CostDisplayStore(storage);

    expect(store.setPreference("EUR", 0.9)).toBe(true);
    expect(store.setPreference("USD", null)).toBe(true);
    expect(store.preference).toEqual({ currency: "USD", eurPerUsd: 0.9 });
    expect(JSON.parse(values.get(COST_DISPLAY_STORAGE_KEY)!)).toEqual({
      currency: "USD",
      eurPerUsd: 0.9,
    });
  });

  it("keeps the current-tab pair when storage writes fail", () => {
    const setItem = vi.fn(() => {
      throw new Error("storage blocked");
    });
    const store = new CostDisplayStore({
      getItem: () => null,
      setItem,
    });

    expect(store.setPreference("EUR", 0.9)).toBe(true);
    expect(store.preference).toEqual({ currency: "EUR", eurPerUsd: 0.9 });
    expect(setItem).toHaveBeenCalledOnce();
  });

  it("keeps the default when storage reads fail", () => {
    const store = new CostDisplayStore({
      getItem: () => {
        throw new Error("storage blocked");
      },
      setItem: () => {},
    });

    expect(store.preference).toEqual({ currency: "USD", eurPerUsd: null });
  });

  it("survives Storage prototype failures", () => {
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("storage blocked");
    });
    expect(new CostDisplayStore(localStorage).preference).toEqual({
      currency: "USD",
      eurPerUsd: null,
    });
    getItem.mockRestore();

    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("storage blocked");
    });
    const store = new CostDisplayStore(localStorage);
    expect(store.setPreference("EUR", 0.9)).toBe(true);
    expect(store.preference).toEqual({ currency: "EUR", eurPerUsd: 0.9 });
    setItem.mockRestore();
  });
});
