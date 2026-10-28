import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

const STORAGE_KEY = "agentsview-message-colors";

let flushSync: () => void;

// Each test loads a fresh store, as the app does on startup. The store's
// effects run on the Svelte runtime loaded with it, so flush that one.
async function loadStore() {
  vi.resetModules();
  const module = await import("./messageColors.svelte.js");
  flushSync = (await import("svelte")).flushSync;
  flushSync();
  return module.messageColors;
}

function rootProperty(name: string): string {
  return document.documentElement.style.getPropertyValue(name);
}

describe("messageColors store", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("style");
    document.documentElement.removeAttribute("data-message-role-patterns");
  });

  afterEach(() => {
    localStorage.clear();
  });

  it("applies a saved color before anything else touches the store", async () => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ colors: { user: "#0072b2" }, patterns: true }),
    );
    const store = await loadStore();
    expect(store.colors).toEqual({ user: "#0072b2" });
    expect(rootProperty("--role-user-accent")).toBe("#0072b2");
    expect(rootProperty("--user-bg")).toContain("#0072b2");
    expect(document.documentElement.hasAttribute("data-message-role-patterns")).toBe(true);
  });

  it("persists a picked color and clears it on reset", async () => {
    const store = await loadStore();
    store.setColor("tool", "009E73");
    flushSync();
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!).colors).toEqual({ tool: "#009e73" });
    expect(rootProperty("--role-tool-accent")).toBe("#009e73");
    expect(store.preset).toBe("custom");

    store.resetColor("tool");
    flushSync();
    expect(rootProperty("--role-tool-accent")).toBe("");
    expect(rootProperty("--tool-bg")).toBe("");
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
    expect(store.preset).toBe("default");
  });

  it("ignores a value that is not a color", async () => {
    const store = await loadStore();
    store.setColor("user", "blue");
    flushSync();
    expect(store.colors).toEqual({});
    expect(rootProperty("--role-user-accent")).toBe("");
  });

  it("applies and recognizes the color-blind preset, then restores defaults", async () => {
    const store = await loadStore();
    store.applyPreset("colorblind");
    flushSync();
    expect(store.preset).toBe("colorblind");
    expect(rootProperty("--role-assistant-accent")).toBe("#d55e00");
    expect(rootProperty("--role-code-frame-width")).toBe("2px");
    expect(document.documentElement.hasAttribute("data-message-role-patterns")).toBe(true);

    store.setColor("user", "#7f7f7f");
    expect(store.preset).toBe("custom");

    store.applyPreset("default");
    flushSync();
    expect(store.isDefault).toBe(true);
    expect(rootProperty("--role-assistant-accent")).toBe("");
    expect(document.documentElement.hasAttribute("data-message-role-patterns")).toBe(false);
  });
});
