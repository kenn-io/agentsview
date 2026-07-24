// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DbProjectInventory, DbProjectInventoryRow } from "../api/generated/index";

const api = vi.hoisted(() => ({
  getApiV1DataProjects: vi.fn(),
}));

const apiRuntimeMocks = vi.hoisted(() => ({
  callGenerated: vi.fn((request: () => Promise<unknown>, _signal?: AbortSignal) => request()),
  isAbortError: vi.fn(() => false),
}));

vi.mock("../api/generated/index", () => ({
  DataService: { getApiV1DataProjects: api.getApiV1DataProjects },
}));
vi.mock("../api/runtime.js", () => ({
  callGenerated: apiRuntimeMocks.callGenerated,
  isAbortError: apiRuntimeMocks.isAbortError,
}));
vi.mock("./router.svelte.js", () => ({
  router: { params: {}, replaceParams: vi.fn() },
}));

import { data } from "./data.svelte.js";
import * as routerMod from "./router.svelte.js";

function makeRow(overrides: Partial<DbProjectInventoryRow> = {}): DbProjectInventoryRow {
  return {
    agents: 1,
    distinct_cwds: 1,
    enabled_rules_targeting: 0,
    label: "Project",
    machines: 1,
    project_key: "k1",
    recorded_as_original: true,
    sessions: 1,
    ...overrides,
  };
}

function makeInventory(rows: DbProjectInventoryRow[]): DbProjectInventory {
  return {
    governed_sessions: 0,
    projects: rows,
    total_projects: rows.length,
    total_sessions: rows.reduce((sum, r) => sum + r.sessions, 0),
  };
}

// Holds a DataPage attachment for tests that register one, so afterEach can
// release it and keep the shared singleton's attach count isolated.
let detach: (() => void) | null = null;

beforeEach(() => {
  api.getApiV1DataProjects.mockReset();
  apiRuntimeMocks.callGenerated.mockReset();
  apiRuntimeMocks.callGenerated.mockImplementation(
    (request: () => Promise<unknown>, _signal?: AbortSignal) => request(),
  );
  apiRuntimeMocks.isAbortError.mockReset();
  apiRuntimeMocks.isAbortError.mockReturnValue(false);
  data.inventory = null;
  data.loading = false;
  data.error = "";
  data.view = "inventory";
  data.selectedProjectKey = "";
  data.rulesMachine = "";
  (routerMod.router as unknown as { params: Record<string, string> }).params = {};
  (routerMod.router as unknown as { replaceParams: ReturnType<typeof vi.fn> }).replaceParams =
    vi.fn();
});

afterEach(() => {
  detach?.();
  detach = null;
});

describe("hydrateFromUrl", () => {
  it("defaults to the inventory view with no selection or machine", () => {
    data.hydrateFromUrl({});
    expect(data.view).toBe("inventory");
    expect(data.selectedProjectKey).toBe("");
    expect(data.rulesMachine).toBe("");
  });

  it("selects the project_key param in the inventory view", () => {
    data.hydrateFromUrl({ project_key: "k1" });
    expect(data.view).toBe("inventory");
    expect(data.selectedProjectKey).toBe("k1");
  });

  it("hydrates the rules view with a machine and clears any selection", () => {
    data.hydrateFromUrl({ view: "rules", machine: "machine-a" });
    expect(data.view).toBe("rules");
    expect(data.rulesMachine).toBe("machine-a");
    expect(data.selectedProjectKey).toBe("");
  });

  it("falls back to the inventory view for an unknown view value", () => {
    data.hydrateFromUrl({ view: "bogus" });
    expect(data.view).toBe("inventory");
    expect(data.rulesMachine).toBe("");
  });
});

describe("view/selection transitions", () => {
  it("resolves a merged inventory row through any underlying project key", () => {
    const row = makeRow({ project_key: "canonical", project_keys: ["canonical", "secondary"] });
    data.inventory = makeInventory([row]);
    data.selectedProjectKey = "secondary";

    expect(data.selectedRow).toEqual(row);
    expect(data.unknownProjectKey).toBe(false);
  });

  it("showRules clears project_key from the written params", () => {
    data.selectProject("k1");
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();

    data.showRules();
    expect(spy).toHaveBeenLastCalledWith({ view: "rules" });
    expect(data.selectedProjectKey).toBe("");

    spy.mockClear();
    data.showRules("machine-a");
    expect(spy).toHaveBeenLastCalledWith({ view: "rules", machine: "machine-a" });
  });

  it("showInventory clears view and machine from the written params", () => {
    data.showRules("machine-a");
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();

    data.showInventory();
    expect(spy).toHaveBeenLastCalledWith({});
    expect(data.view).toBe("inventory");
    expect(data.rulesMachine).toBe("");
  });
});

describe("load", () => {
  it("populates inventory from the mocked response", async () => {
    const inventory = makeInventory([makeRow()]);
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    await expect(data.load()).resolves.toBe(true);

    expect(data.inventory).toEqual(inventory);
    expect(data.loading).toBe(false);
    expect(data.error).toBe("");
  });

  it("sets error and returns false on rejection", async () => {
    api.getApiV1DataProjects.mockRejectedValueOnce(new Error("network down"));

    await expect(data.load()).resolves.toBe(false);

    expect(data.error).toBe("network down");
    expect(data.loading).toBe(false);
    expect(data.inventory).toBeNull();
  });

  it("never lets a stale load overwrite a newer result", async () => {
    let resolveFirst!: (inventory: DbProjectInventory) => void;
    let resolveSecond!: (inventory: DbProjectInventory) => void;
    api.getApiV1DataProjects
      .mockImplementationOnce(
        () =>
          new Promise<DbProjectInventory>((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockImplementationOnce(
        () =>
          new Promise<DbProjectInventory>((resolve) => {
            resolveSecond = resolve;
          }),
      );

    const first = data.load();
    const second = data.load();

    const newer = makeInventory([makeRow({ project_key: "newer" })]);
    resolveSecond(newer);
    await expect(second).resolves.toBe(true);
    expect(data.inventory).toEqual(newer);

    const older = makeInventory([makeRow({ project_key: "older" })]);
    resolveFirst(older);
    await expect(first).resolves.toBe(false);
    expect(data.inventory).toEqual(newer);
    expect(data.loading).toBe(false);
  });

  it("keeps existing inventory and suppresses the error when a background load fails", async () => {
    const inventory = makeInventory([makeRow()]);
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);
    await expect(data.load()).resolves.toBe(true);
    expect(data.inventory).toEqual(inventory);

    api.getApiV1DataProjects.mockRejectedValueOnce(new Error("network down"));
    await expect(data.load({ background: true })).resolves.toBe(false);

    expect(data.inventory).toEqual(inventory);
    expect(data.error).toBe("");
    expect(data.loading).toBe(false);
  });
});

describe("attach", () => {
  it("hydrates from the URL on attach, then re-hydrates and reloads on popstate", async () => {
    const initial = makeInventory([makeRow({ project_key: "k1" })]);
    api.getApiV1DataProjects.mockResolvedValueOnce(initial);
    await expect(data.load()).resolves.toBe(true);
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(1);

    (routerMod.router as unknown as { params: Record<string, string> }).params = {
      project_key: "k1",
    };
    detach = data.attach();
    expect(data.view).toBe("inventory");
    expect(data.selectedProjectKey).toBe("k1");

    const updated = makeInventory([makeRow({ project_key: "k2" })]);
    api.getApiV1DataProjects.mockResolvedValueOnce(updated);
    (routerMod.router as unknown as { params: Record<string, string> }).params = {
      view: "rules",
      machine: "m1",
    };
    window.dispatchEvent(new Event("popstate"));

    expect(data.view).toBe("rules");
    expect(data.rulesMachine).toBe("m1");
    expect(data.selectedProjectKey).toBe("");
    await vi.waitFor(() => expect(data.inventory).toEqual(updated));
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(2);
  });

  it("stops re-hydrating and reloading once the returned detach runs", async () => {
    (routerMod.router as unknown as { params: Record<string, string> }).params = {};
    detach = data.attach();
    const release = detach;
    detach = null;
    release();

    (routerMod.router as unknown as { params: Record<string, string> }).params = {
      project_key: "k9",
    };
    window.dispatchEvent(new Event("popstate"));
    await Promise.resolve();

    expect(data.selectedProjectKey).toBe("");
    expect(api.getApiV1DataProjects).not.toHaveBeenCalled();
  });
});

describe("cancelInFlightReads", () => {
  it("stops loading and a late resolution does not overwrite inventory or loading", async () => {
    const signals: AbortSignal[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal as AbortSignal);
        return request();
      },
    );

    let resolveLoad!: (inventory: DbProjectInventory) => void;
    api.getApiV1DataProjects.mockImplementationOnce(
      () =>
        new Promise<DbProjectInventory>((resolve) => {
          resolveLoad = resolve;
        }),
    );

    const pending = data.load();
    expect(data.loading).toBe(true);
    expect(signals[0]?.aborted).toBe(false);

    data.cancelInFlightReads();
    expect(data.loading).toBe(false);
    expect(signals[0]?.aborted).toBe(true);

    resolveLoad(makeInventory([makeRow()]));
    await expect(pending).resolves.toBe(false);

    expect(data.inventory).toBeNull();
    expect(data.loading).toBe(false);
  });
});

describe("unknownProjectKey", () => {
  it("is false while the inventory has not loaded", () => {
    data.selectedProjectKey = "missing";
    expect(data.inventory).toBeNull();
    expect(data.unknownProjectKey).toBe(false);
  });

  it("is true once loaded when the selected key matches no row", () => {
    data.inventory = makeInventory([makeRow({ project_key: "k1" })]);
    data.selectedProjectKey = "missing";
    expect(data.unknownProjectKey).toBe(true);
    expect(data.selectedRow).toBeNull();

    data.selectedProjectKey = "k1";
    expect(data.unknownProjectKey).toBe(false);
    expect(data.selectedRow?.project_key).toBe("k1");
  });
});

describe("refreshAfterApply", () => {
  it("keeps the selection when the original key still exists", async () => {
    const inventory = makeInventory([
      makeRow({ project_key: "k1", label: "Alpha" }),
      makeRow({ project_key: "k2", label: "Beta" }),
    ]);
    data.selectedProjectKey = "k1";
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    await expect(data.refreshAfterApply("k1", "Alpha (merged)")).resolves.toBe(true);

    expect(data.selectedProjectKey).toBe("k1");
  });

  it("selects the applied target's row when the original key is gone", async () => {
    const inventory = makeInventory([
      makeRow({ project_key: "k2", label: "Beta" }),
      makeRow({ project_key: "k3", label: "Merged Target" }),
    ]);
    data.selectedProjectKey = "k1";
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();

    await expect(data.refreshAfterApply("k1", "Merged Target")).resolves.toBe(true);

    expect(data.selectedProjectKey).toBe("k3");
    expect(spy).toHaveBeenLastCalledWith({ project_key: "k3" });
  });

  it("clears the selection when neither the original key nor the target label is found", async () => {
    const inventory = makeInventory([makeRow({ project_key: "k2", label: "Beta" })]);
    data.selectedProjectKey = "k1";
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    await expect(data.refreshAfterApply("k1", "Nonexistent")).resolves.toBe(true);

    expect(data.selectedProjectKey).toBe("");
  });

  it("leaves the selection untouched and returns false when the reload fails", async () => {
    data.selectedProjectKey = "k1";
    api.getApiV1DataProjects.mockRejectedValueOnce(new Error("network down"));

    await expect(data.refreshAfterApply("k1", "Alpha")).resolves.toBe(false);

    expect(data.selectedProjectKey).toBe("k1");
  });

  it("skips reselection when called with a captured key after the selection was cleared", async () => {
    // The workspace was dismissed before the apply refresh ran; the caller
    // still passes the key captured when the workspace was open.
    data.selectedProjectKey = "";
    const inventory = makeInventory([makeRow({ project_key: "k3", label: "Merged Target" })]);
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();

    await expect(data.refreshAfterApply("k1", "Merged Target")).resolves.toBe(true);

    expect(data.selectedProjectKey).toBe("");
    expect(spy).not.toHaveBeenCalled();
  });

  it("lets a mid-refresh clearSelection win over reselecting the applied target", async () => {
    data.selectedProjectKey = "k1";
    let resolveLoad!: (inventory: DbProjectInventory) => void;
    api.getApiV1DataProjects.mockImplementationOnce(
      () =>
        new Promise<DbProjectInventory>((resolve) => {
          resolveLoad = resolve;
        }),
    );
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();

    const pending = data.refreshAfterApply("k1", "Merged Target");
    data.clearSelection();
    spy.mockClear();

    const inventory = makeInventory([
      makeRow({ project_key: "k2", label: "Beta" }),
      makeRow({ project_key: "k3", label: "Merged Target" }),
    ]);
    resolveLoad(inventory);

    await expect(pending).resolves.toBe(true);
    expect(data.selectedProjectKey).toBe("");
    expect(spy).not.toHaveBeenCalled();
  });
});
