import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { tick } from "svelte";
import {
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_KEY,
  SIDEBAR_WIDTH_MIN,
  SIDEBAR_WIDTH_STORAGE_MAX,
} from "../components/layout/sidebar-width.js";
import {
  ALL_BLOCK_TYPES,
  parseBlockFilters,
  serializeBlockFilters,
  ui,
  type BlockType,
} from "./ui.svelte.js";

describe("UIStore", () => {
  beforeEach(() => {
    ui.activeModal = null;
    ui.clearPublishTarget();
    ui.publishSecret = false;
    ui.selectedOrdinal = null;
    ui.pendingScrollOrdinal = null;
    ui.followLatest = false;
    ui.followLatestRequest = 0;
  });

  describe("activeModal", () => {
    it("should default to null", () => {
      expect(ui.activeModal).toBeNull();
    });

    it("should set and clear modal type", () => {
      ui.activeModal = "commandPalette";
      expect(ui.activeModal).toBe("commandPalette");

      ui.activeModal = null;
      expect(ui.activeModal).toBeNull();
    });

    it("should switch between modal types", () => {
      ui.activeModal = "shortcuts";
      expect(ui.activeModal).toBe("shortcuts");

      ui.activeModal = "publish";
      expect(ui.activeModal).toBe("publish");
    });
  });

  describe("publishTarget", () => {
    it("defaults to null", () => {
      expect(ui.publishTarget).toBeNull();
    });

    it("stores the selected publish target", () => {
      ui.setPublishTarget({ kind: "insight", id: 42 });
      expect(ui.publishTarget).toEqual({
        kind: "insight",
        id: 42,
      });
    });

    it("stores a selected session publish target", () => {
      ui.setPublishTarget({ kind: "session", id: "sess-123" });
      expect(ui.publishTarget).toEqual({
        kind: "session",
        id: "sess-123",
      });
    });

    it("clears the publish target when publish modal closes", async () => {
      ui.setPublishTarget({ kind: "insight", id: 42 });
      ui.activeModal = "publish";
      await tick();

      ui.activeModal = null;
      await tick();

      expect(ui.publishTarget).toBeNull();
    });
  });

  describe("closeAll", () => {
    it("should set activeModal to null", () => {
      ui.activeModal = "commandPalette";
      ui.closeAll();
      expect(ui.activeModal).toBeNull();
    });

    it("should be idempotent when already null", () => {
      ui.closeAll();
      expect(ui.activeModal).toBeNull();
    });
  });

  describe("selectedOrdinal null flows", () => {
    it("should default to null", () => {
      expect(ui.selectedOrdinal).toBeNull();
    });

    it("should set ordinal via selectOrdinal", () => {
      ui.selectOrdinal(5);
      expect(ui.selectedOrdinal).toBe(5);
    });

    it("should clear to null via clearSelection", () => {
      ui.selectOrdinal(5);
      ui.clearSelection();
      expect(ui.selectedOrdinal).toBeNull();
    });

    it("should handle ordinal 0 without confusion", () => {
      ui.selectOrdinal(0);
      expect(ui.selectedOrdinal).toBe(0);
    });

    it("clearSelection should be idempotent", () => {
      ui.clearSelection();
      expect(ui.selectedOrdinal).toBeNull();
    });
  });

  describe("pendingScrollOrdinal null flows", () => {
    it("should default to null", () => {
      expect(ui.pendingScrollOrdinal).toBeNull();
    });

    it("should set both selected and pending via scrollToOrdinal", () => {
      ui.scrollToOrdinal(10);
      expect(ui.selectedOrdinal).toBe(10);
      expect(ui.pendingScrollOrdinal).toBe(10);
      expect(ui.pendingScrollSession).toBeNull();
    });

    it("should store session ID when provided", () => {
      ui.scrollToOrdinal(5, "sess-123");
      expect(ui.pendingScrollOrdinal).toBe(5);
      expect(ui.pendingScrollSession).toBe("sess-123");
    });

    it("should allow clearing pending independently", () => {
      ui.scrollToOrdinal(10);
      ui.pendingScrollOrdinal = null;
      expect(ui.pendingScrollOrdinal).toBeNull();
      expect(ui.selectedOrdinal).toBe(10);
    });

    it("should handle ordinal 0", () => {
      ui.scrollToOrdinal(0);
      expect(ui.selectedOrdinal).toBe(0);
      expect(ui.pendingScrollOrdinal).toBe(0);
    });
  });

  describe("followLatest", () => {
    it("defaults to disabled", () => {
      expect(ui.followLatest).toBe(false);
    });

    it("can be enabled and disabled", () => {
      ui.setFollowLatest(true);
      expect(ui.followLatest).toBe(true);

      ui.setFollowLatest(false);
      expect(ui.followLatest).toBe(false);
    });

    it("records a new request when already enabled", () => {
      ui.setFollowLatest(true);
      const first = ui.followLatestRequest;

      ui.setFollowLatest(true);

      expect(ui.followLatest).toBe(true);
      expect(ui.followLatestRequest).toBe(first + 1);
    });

    it("toggles follow latest mode", () => {
      ui.toggleFollowLatest();
      expect(ui.followLatest).toBe(true);
      expect(ui.followLatestRequest).toBe(1);

      ui.toggleFollowLatest();
      expect(ui.followLatest).toBe(false);
      expect(ui.followLatestRequest).toBe(1);
    });

    it("is disabled when jumping to a specific ordinal", () => {
      ui.setFollowLatest(true);
      ui.scrollToOrdinal(10);

      expect(ui.followLatest).toBe(false);
      expect(ui.pendingScrollOrdinal).toBe(10);
    });
  });

  describe("theme initialization", () => {
    it("should fall back to light when stored theme is absent", () => {
      expect(ui.theme).toBeDefined();
      expect(["light", "dark"]).toContain(ui.theme);
    });

    it("migrates the legacy high-contrast key on module init", async () => {
      const original = globalThis.localStorage;
      const store = new Map<string, string>([["agentsview-high-contrast", "true"]]);
      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: (key: string) => store.get(key) ?? null,
          setItem: (key: string, value: string) => {
            store.set(key, value);
          },
        },
        writable: true,
        configurable: true,
      });
      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?hcMigration");
        expect(store.get("theme-high-contrast")).toBe("true");
        expect(mod.ui.highContrast).toBe(true);
        // Reset the kit-ui singleton so later tests start from default state.
        mod.ui.highContrast = false;
      } finally {
        document.documentElement.classList.remove("high-contrast");
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("should survive when localStorage.getItem is unavailable", async () => {
      const original = globalThis.localStorage;
      // Replace with an object that lacks getItem/setItem
      Object.defineProperty(globalThis, "localStorage", {
        value: {},
        writable: true,
        configurable: true,
      });
      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?noGetItem");
        expect(mod.ui.theme).toBe("light");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("should survive when localStorage is null", async () => {
      const original = globalThis.localStorage;
      Object.defineProperty(globalThis, "localStorage", {
        value: null,
        writable: true,
        configurable: true,
      });
      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?nullStorage");
        expect(mod.ui.theme).toBe("light");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("should survive when localStorage is undefined", async () => {
      const original = globalThis.localStorage;
      // @ts-expect-error -- deliberately removing localStorage
      delete globalThis.localStorage;
      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?noStorage");
        expect(mod.ui.theme).toBe("light");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });
  });

  describe("sidebar width", () => {
    it("defaults to the helper default when storage is empty", async () => {
      const original = globalThis.localStorage;
      const getItem = vi.fn(() => null);
      const setItem = vi.fn();

      Object.defineProperty(globalThis, "localStorage", {
        value: { getItem, setItem },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthEmpty");
        expect(getItem.mock.calls).toContainEqual([SIDEBAR_WIDTH_KEY]);
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("reads and clamps stored widths including stored strings", async () => {
      const original = globalThis.localStorage;

      try {
        Object.defineProperty(globalThis, "localStorage", {
          value: {
            getItem: vi.fn((key: string) =>
              key === SIDEBAR_WIDTH_KEY ? String(SIDEBAR_WIDTH_MIN - 50) : null,
            ),
            setItem: vi.fn(),
          },
          writable: true,
          configurable: true,
        });
        // @ts-expect-error -- query string busts module cache
        const minMod = await import("./ui.svelte.js?sidebarWidthStoredMin");

        Object.defineProperty(globalThis, "localStorage", {
          value: {
            getItem: vi.fn((key: string) =>
              key === SIDEBAR_WIDTH_KEY ? String(SIDEBAR_WIDTH_STORAGE_MAX + 50) : null,
            ),
            setItem: vi.fn(),
          },
          writable: true,
          configurable: true,
        });
        // @ts-expect-error -- query string busts module cache
        const maxMod = await import("./ui.svelte.js?sidebarWidthStoredMax");

        Object.defineProperty(globalThis, "localStorage", {
          value: {
            getItem: vi.fn((key: string) => (key === SIDEBAR_WIDTH_KEY ? "300" : null)),
            setItem: vi.fn(),
          },
          writable: true,
          configurable: true,
        });
        // @ts-expect-error -- query string busts module cache
        const stringMod = await import("./ui.svelte.js?sidebarWidthStoredString");

        expect(minMod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_MIN);
        expect(maxMod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_STORAGE_MAX);
        expect(stringMod.ui.sidebarWidth).toBe(300);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("persists clamped widths through setSidebarWidth", async () => {
      const original = globalThis.localStorage;
      const setItem = vi.fn();

      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: vi.fn(() => null),
          setItem,
        },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthPersist");
        setItem.mockClear();

        mod.ui.setSidebarWidth(SIDEBAR_WIDTH_MIN - 10);
        await tick();
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_MIN);
        expect(setItem).toHaveBeenCalledTimes(1);
        expect(setItem).toHaveBeenLastCalledWith(SIDEBAR_WIDTH_KEY, String(SIDEBAR_WIDTH_MIN));

        setItem.mockClear();
        mod.ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX + 10);
        await tick();
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_STORAGE_MAX);
        expect(setItem).toHaveBeenCalledTimes(1);
        expect(setItem).toHaveBeenLastCalledWith(
          SIDEBAR_WIDTH_KEY,
          String(SIDEBAR_WIDTH_STORAGE_MAX),
        );
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("survives when localStorage.getItem is unavailable", async () => {
      const original = globalThis.localStorage;

      Object.defineProperty(globalThis, "localStorage", {
        value: {
          setItem: vi.fn(),
        },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthNoGetItem");
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("survives when localStorage.setItem is unavailable", async () => {
      const original = globalThis.localStorage;

      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: vi.fn(() => String(SIDEBAR_WIDTH_DEFAULT + 10)),
        },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthNoSetItem");
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 10);
        expect(() => mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 20)).not.toThrow();
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT + 20);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("survives when localStorage is null", async () => {
      const original = globalThis.localStorage;

      Object.defineProperty(globalThis, "localStorage", {
        value: null,
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthNullStorage");
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
        expect(() => mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 15)).not.toThrow();
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("survives when localStorage is undefined", async () => {
      const original = globalThis.localStorage;
      // @ts-expect-error -- deliberately removing localStorage
      delete globalThis.localStorage;

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?sidebarWidthNoStorage");
        expect(mod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_DEFAULT);
        expect(() => mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 25)).not.toThrow();
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });
  });

  describe("Calls detail preference", () => {
    it("defaults the Calls detail to expanded", async () => {
      const original = globalThis.localStorage;
      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: vi.fn(() => null),
          setItem: vi.fn(),
        },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?vitalsCallsDefault");
        expect(mod.ui.vitalsCallsExpanded).toBe(true);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("restores a collapsed Calls detail preference", async () => {
      const original = globalThis.localStorage;
      const setItem = vi.fn();
      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: vi.fn((key: string) =>
            key === "agentsview-session-vitals-calls-expanded" ? "false" : null,
          ),
          setItem,
        },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?vitalsCallsCollapsed");
        expect(mod.ui.vitalsCallsExpanded).toBe(false);
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("persists Calls detail changes", async () => {
      const original = globalThis.localStorage;
      const setItem = vi.fn();
      Object.defineProperty(globalThis, "localStorage", {
        value: { getItem: vi.fn(() => null), setItem },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?vitalsCallsPersist");
        setItem.mockClear();

        mod.ui.toggleVitalsCalls();
        await tick();

        expect(mod.ui.vitalsCallsExpanded).toBe(false);
        expect(setItem).toHaveBeenCalledWith("agentsview-session-vitals-calls-expanded", "false");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });
  });

  describe("postMessage theme control", () => {
    it("should change theme on valid theme:set message", () => {
      ui.theme = "light";
      window.dispatchEvent(
        new MessageEvent("message", {
          data: { type: "theme:set", theme: "dark" },
        }),
      );
      expect(ui.theme).toBe("dark");
    });

    it("should ignore invalid theme values", () => {
      ui.theme = "light";
      window.dispatchEvent(
        new MessageEvent("message", {
          data: { type: "theme:set", theme: "purple" },
        }),
      );
      expect(ui.theme).toBe("light");
    });

    it("should ignore unrelated message types", () => {
      ui.theme = "light";
      window.dispatchEvent(
        new MessageEvent("message", {
          data: { type: "some-other-event", theme: "dark" },
        }),
      );
      expect(ui.theme).toBe("light");
    });
  });

  describe("toggles", () => {
    it("should toggle theme between light and dark", () => {
      ui.theme = "light";
      ui.toggleTheme();
      expect(ui.theme).toBe("dark");
      ui.toggleTheme();
      expect(ui.theme).toBe("light");
    });

    it("should toggle sortNewestFirst", () => {
      const initial = ui.sortNewestFirst;
      ui.toggleSort();
      expect(ui.sortNewestFirst).toBe(!initial);
    });
  });

  describe("block type filtering", () => {
    beforeEach(() => {
      ui.showAllBlocks();
    });

    it("should start with all blocks visible", () => {
      expect(ui.hiddenBlockCount).toBe(0);
      expect(ui.hasBlockFilters).toBe(false);
      expect(ui.isBlockVisible("user")).toBe(true);
      expect(ui.isBlockVisible("tool")).toBe(true);
      expect(ui.isBlockVisible("thinking")).toBe(true);
      expect(ui.isBlockVisible("code")).toBe(true);
      expect(ui.isBlockVisible("assistant")).toBe(true);
      expect(ui.isBlockVisible("system")).toBe(true);
    });

    it("keeps a hidden system boundary hidden across a reload", () => {
      const visible = parseBlockFilters(JSON.stringify({ hidden: ["system"] }));

      expect(visible.has("system")).toBe(false);
      expect(visible.has("user")).toBe(true);
    });

    it("restores the same hidden set it stored", () => {
      const chosen = new Set<BlockType>(ALL_BLOCK_TYPES);
      chosen.delete("system");
      chosen.delete("tool");

      const restored = parseBlockFilters(serializeBlockFilters(chosen));

      expect([...restored].sort()).toEqual(["assistant", "code", "thinking", "user"]);
    });

    it("shows every block when the stored payload is missing or unreadable", () => {
      expect(parseBlockFilters(null).size).toBe(ALL_BLOCK_TYPES.length);
      expect(parseBlockFilters("{not json").size).toBe(ALL_BLOCK_TYPES.length);
    });

    it("should toggle a block type off and on", () => {
      ui.toggleBlock("tool");
      expect(ui.isBlockVisible("tool")).toBe(false);
      expect(ui.hiddenBlockCount).toBe(1);
      expect(ui.hasBlockFilters).toBe(true);

      ui.toggleBlock("tool");
      expect(ui.isBlockVisible("tool")).toBe(true);
      expect(ui.hiddenBlockCount).toBe(0);
    });

    it("should reset all with showAllBlocks", () => {
      ui.toggleBlock("user");
      ui.toggleBlock("tool");
      ui.toggleBlock("code");
      expect(ui.hiddenBlockCount).toBe(3);

      ui.showAllBlocks();
      expect(ui.hiddenBlockCount).toBe(0);
      expect(ui.hasBlockFilters).toBe(false);
    });
  });

  describe("sidebar", () => {
    beforeEach(() => {
      ui.sidebarOpen = true;
    });

    it("should default to open", () => {
      expect(ui.sidebarOpen).toBe(true);
    });

    it("should toggle sidebar", () => {
      ui.toggleSidebar();
      expect(ui.sidebarOpen).toBe(false);

      ui.toggleSidebar();
      expect(ui.sidebarOpen).toBe(true);
    });

    it("should close sidebar", () => {
      ui.closeSidebar();
      expect(ui.sidebarOpen).toBe(false);
    });

    it("closeSidebar should be idempotent", () => {
      ui.closeSidebar();
      ui.closeSidebar();
      expect(ui.sidebarOpen).toBe(false);
    });

    it("isMobileViewport should default to false in test environment", () => {
      // matchMedia is unavailable in test env, so isMobileViewport
      // stays at its initial value (false = desktop assumption).
      expect(ui.isMobileViewport).toBe(false);
    });

    it("should initialize sidebar closed on narrow viewport", async () => {
      const originalMatchMedia = window.matchMedia;
      // The store watches kit-ui's MEDIA.medium (max-width: 760px), so a
      // matching query means a narrow viewport.
      window.matchMedia = vi.fn().mockReturnValue({
        matches: true,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      }) as unknown as typeof window.matchMedia;
      try {
        // @ts-expect-error -- cache bust for fresh UIStore
        const mod = await import("./ui.svelte.js?narrowViewport");
        expect(mod.ui.sidebarOpen).toBe(false);
        expect(mod.ui.isMobileViewport).toBe(true);
      } finally {
        window.matchMedia = originalMatchMedia;
      }
    });

    it("should initialize sidebar open on wide viewport", async () => {
      const originalMatchMedia = window.matchMedia;
      // max-width: 760px does not match on a wide viewport.
      window.matchMedia = vi.fn().mockReturnValue({
        matches: false,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      }) as unknown as typeof window.matchMedia;
      try {
        // @ts-expect-error -- cache bust for fresh UIStore
        const mod = await import("./ui.svelte.js?wideViewport");
        expect(mod.ui.sidebarOpen).toBe(true);
        expect(mod.ui.isMobileViewport).toBe(false);
      } finally {
        window.matchMedia = originalMatchMedia;
      }
    });
  });

  describe("messageLayout", () => {
    beforeEach(() => {
      ui.setLayout("default");
    });

    it("should default to 'default'", () => {
      expect(ui.messageLayout).toBe("default");
    });

    it("should set layout explicitly", () => {
      ui.setLayout("compact");
      expect(ui.messageLayout).toBe("compact");

      ui.setLayout("stream");
      expect(ui.messageLayout).toBe("stream");
    });

    it("should cycle through layouts", () => {
      ui.setLayout("default");
      ui.cycleLayout();
      expect(ui.messageLayout).toBe("compact");

      ui.cycleLayout();
      expect(ui.messageLayout).toBe("stream");

      ui.cycleLayout();
      expect(ui.messageLayout).toBe("skim");

      ui.cycleLayout();
      expect(ui.messageLayout).toBe("default");
    });
  });

  describe("transcriptMode", () => {
    beforeEach(() => {
      ui.setTranscriptMode("normal");
    });

    it("should default to normal", () => {
      expect(ui.transcriptMode).toBe("normal");
    });

    it("should set transcript mode explicitly", () => {
      ui.setTranscriptMode("focused");
      expect(ui.transcriptMode).toBe("focused");
    });

    it("should persist transcript mode changes", async () => {
      const original = globalThis.localStorage;
      const setItem = vi.fn();
      const getItem = vi.fn(() => null);

      Object.defineProperty(globalThis, "localStorage", {
        value: { getItem, setItem },
        writable: true,
        configurable: true,
      });

      try {
        // @ts-expect-error -- cache bust for fresh UIStore
        const mod = await import("./ui.svelte.js?persistTranscriptMode");
        setItem.mockClear();
        mod.ui.setTranscriptMode("focused");
        await Promise.resolve();
        expect(setItem).toHaveBeenLastCalledWith("agentsview-transcript-mode", "focused");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });

    it("should fall back to normal for invalid stored transcript mode", async () => {
      const original = globalThis.localStorage;

      Object.defineProperty(globalThis, "localStorage", {
        value: {
          getItem: vi.fn((key: string) =>
            key === "agentsview-transcript-mode" ? "detailed" : null,
          ),
          setItem: vi.fn(),
        },
        writable: true,
        configurable: true,
      });
      try {
        // @ts-expect-error -- cache bust for fresh UIStore
        const mod = await import("./ui.svelte.js?badTranscriptMode");
        expect(mod.ui.transcriptMode).toBe("normal");
      } finally {
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });
  });

  describe("shared zoom", () => {
    const originalUrl = window.location.href;
    let stored: Map<string, string>;
    let storage: Pick<Storage, "getItem" | "setItem" | "removeItem">;

    beforeEach(() => {
      vi.resetModules();
      stored = new Map();
      storage = {
        getItem: vi.fn((key: string) => stored.get(key) ?? null),
        setItem: vi.fn((key: string, value: string) => { stored.set(key, value); }),
        removeItem: vi.fn((key: string) => { stored.delete(key); }),
      };
      vi.stubGlobal("localStorage", storage);
      vi.stubGlobal("__TAURI__", undefined);
      window.history.replaceState({}, "", "/");
    });

    afterEach(async () => {
      await tick();
      vi.restoreAllMocks();
      vi.unstubAllGlobals();
      window.history.replaceState({}, "", originalUrl);
      document.documentElement.style.removeProperty("zoom");
    });

    const percentages = [
      [67, "0.67"], [75, "0.75"], [80, "0.8"], [90, "0.9"],
      [100, "1"], [110, "1.1"], [120, "1.2"], [125, "1.25"],
      [130, "1.3"], [150, "1.5"], [175, "1.75"], [200, "2"],
    ] as const;

    it.each(percentages)("restores and applies canonical %i in the browser", async (level, factor) => {
      stored.set("agentsview-zoom-level", String(level));
      const { ui: zoom } = await import("./ui.svelte.js");
      await tick();
      expect(zoom.zoomLevel).toBe(level);
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe(factor);
      expect(stored.get("agentsview-zoom-level")).toBe(String(level));
    });

    describe.each(["/", "/?desktop"])("migration at %s", (url) => {
      it.each([
        [null, null, 100], ["100", "100", 100], [null, "90", 90],
        [null, "100", 100], [null, "110", 110], [null, "120", 120],
        [null, "130", 130], ["100", "120", 120], ["150", "120", 150],
        ["120", "130", 120], ["150", "100", 150], ["bad", "130", 130],
        ["133", "120", 120], ["0", "120", 120], ["NaN", "120", 120],
        ["Infinity", "120", 120], ["-100", "120", 120], ["", "120", 120],
        ["bad", "bad", 100], ["100", "150", 100], [null, "125", 100],
        [null, "0", 100], [null, "NaN", 100], [null, "", 100],
      ])("chooses %s plus legacy %s as %i", async (canonical, legacy, expected) => {
        window.history.replaceState({}, "", url);
        if (canonical !== null) stored.set("agentsview-zoom-level", String(canonical));
        if (legacy !== null) stored.set("agentsview-font-scale", String(legacy));
        const { ui: zoom } = await import("./ui.svelte.js");
        await tick();
        expect(zoom.zoomLevel).toBe(expected);
        expect(stored.get("agentsview-zoom-level")).toBe(String(expected));
        expect(stored.has("agentsview-font-scale")).toBe(false);
        expect(storage.setItem).toHaveBeenCalledWith("agentsview-zoom-level", String(expected));
        expect(storage.removeItem).toHaveBeenCalledWith("agentsview-font-scale");
      });
    });

    it.each([
      ["browser", "/", false, false],
      ["browser with bridge marker", "/", true, false],
      ["desktop without bridge", "/?desktop", false, false],
      ["native desktop", "/?desktop", true, true],
    ])("applies each selection once in %s", async (_mode, url, bridge, native) => {
      window.history.replaceState({}, "", url);
      const setZoom = vi.fn(() => Promise.resolve());
      if (bridge) vi.stubGlobal("__TAURI__", { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom }) } });
      const { ui: zoom } = await import("./ui.svelte.js");
      await tick();
      for (const [level, factor] of percentages) {
        setZoom.mockClear();
        zoom.setZoomLevel(level);
        await tick();
        await Promise.resolve();
        await new Promise((resolve) => setTimeout(resolve, 0));
        expect(zoom.zoomLevel).toBe(level);
        expect(document.documentElement.style.getPropertyValue("zoom")).toBe(native ? "1" : factor);
        expect(stored.get("agentsview-zoom-level")).toBe(String(level));
        if (native) expect(setZoom.mock.calls).toEqual([[Number(factor)]]);
        else expect(setZoom).not.toHaveBeenCalled();
      }
    });

    it("keeps step controls, endpoints, and reset on the shared percentage", async () => {
      const { ui: zoom } = await import("./ui.svelte.js");
      zoom.setZoomLevel(67);
      zoom.zoomOut();
      await tick();
      expect(stored.get("agentsview-zoom-level")).toBe("67");
      for (const [level] of percentages.slice(1)) {
        zoom.zoomIn();
        await tick();
        expect(stored.get("agentsview-zoom-level")).toBe(String(level));
      }
      zoom.zoomIn();
      expect(zoom.zoomLevel).toBe(200);
      for (const [level] of percentages.slice(0, -1).toReversed()) {
        zoom.zoomOut();
        await tick();
        expect(stored.get("agentsview-zoom-level")).toBe(String(level));
      }
      zoom.resetZoom();
      await tick();
      expect(stored.get("agentsview-zoom-level")).toBe("100");
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1");
    });

    it("ignores invalid setters without persisting or calling the bridge", async () => {
      window.history.replaceState({}, "", "/?desktop");
      const setZoom = vi.fn(() => Promise.resolve());
      vi.stubGlobal("__TAURI__", { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom }) } });
      const { ui: zoom } = await import("./ui.svelte.js");
      zoom.setZoomLevel(120);
      await tick();
      await Promise.resolve();
      await new Promise((resolve) => setTimeout(resolve, 0));
      setZoom.mockClear();
      vi.mocked(storage.setItem).mockClear();
      for (const level of [0, -100, 66, 133, 201, 120.5, NaN, Infinity]) {
        zoom.setZoomLevel(level);
        await tick();
        expect(zoom.zoomLevel).toBe(120);
      }
      expect(setZoom).not.toHaveBeenCalled();
      expect(storage.setItem).not.toHaveBeenCalled();
      expect(stored.get("agentsview-zoom-level")).toBe("120");
    });

    it("falls back to CSS zoom when native zoom rejects", async () => {
      window.history.replaceState({}, "", "/?desktop");
      stored.set("agentsview-font-scale", "120");
      const setZoom = vi.fn(() => Promise.reject(new Error("webview unavailable")));
      vi.stubGlobal("__TAURI__", { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom }) } });
      await import("./ui.svelte.js");
      await tick();
      await Promise.resolve();
      await tick();
      await Promise.resolve();
      await Promise.resolve();
      expect(setZoom.mock.calls).toEqual([[1.2], [1]]);
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.2");
      expect(stored.get("agentsview-zoom-level")).toBe("120");
    });

    it("falls back to CSS zoom when native zoom throws synchronously", async () => {
      window.history.replaceState({}, "", "/?desktop");
      stored.set("agentsview-font-scale", "120");
      const setZoom = vi.fn(() => { throw new Error("webview unavailable"); });
      vi.stubGlobal("__TAURI__", { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom }) } });
      await import("./ui.svelte.js");
      await tick();
      await Promise.resolve();
      await tick();
      await Promise.resolve();
      await Promise.resolve();
      expect(setZoom.mock.calls).toEqual([[1.2], [1]]);
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.2");
      expect(stored.get("agentsview-zoom-level")).toBe("120");
    });

    it("resets a previous native factor before CSS fallback", async () => {
      window.history.replaceState({}, "", "/?desktop");
      stored.set("agentsview-zoom-level", "150");
      const setZoom = vi.fn(async (factor: number) => {
        if (factor === 1.2) throw new Error("webview unavailable");
      });
      vi.stubGlobal("__TAURI__", { webviewWindow: { getCurrentWebviewWindow: () => ({ setZoom }) } });
      const { ui: zoom } = await import("./ui.svelte.js");
      await tick();
      zoom.setZoomLevel(120);
      await tick();
      await Promise.resolve();
      await tick();
      await Promise.resolve();
      await Promise.resolve();
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(setZoom.mock.calls).toEqual([[1.5], [1.2], [1]]);
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.2");
    });

    it("defaults when storage reads throw and still applies later selections", async () => {
      vi.mocked(storage.getItem).mockImplementation(() => { throw new Error("blocked"); });
      const { ui: zoom } = await import("./ui.svelte.js");
      await tick();
      expect(zoom.zoomLevel).toBe(100);
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1");
      zoom.setZoomLevel(130);
      await tick();
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.3");
    });

    it("retains legacy storage when the canonical write fails", async () => {
      stored.set("agentsview-font-scale", "120");
      vi.mocked(storage.setItem).mockImplementation(() => { throw new Error("quota"); });
      const { ui: zoom } = await import("./ui.svelte.js");
      await tick();
      expect(zoom.zoomLevel).toBe(120);
      zoom.setZoomLevel(130);
      await tick();
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.3");
      expect(stored.get("agentsview-font-scale")).toBe("120");
      expect(stored.has("agentsview-zoom-level")).toBe(false);
      expect(storage.removeItem).not.toHaveBeenCalled();
    });

    it("persists before cleanup and survives a failed legacy removal", async () => {
      stored.set("agentsview-font-scale", "120");
      let savedAtRemoval: string | undefined;
      vi.mocked(storage.removeItem).mockImplementation(() => {
        savedAtRemoval = stored.get("agentsview-zoom-level");
        throw new Error("blocked");
      });
      const { ui: zoom } = await import("./ui.svelte.js");
      zoom.setZoomLevel(130);
      await tick();
      expect(storage.removeItem).toHaveBeenCalledWith("agentsview-font-scale");
      expect(savedAtRemoval).toBe("130");
      expect(stored.get("agentsview-zoom-level")).toBe("130");
      expect(stored.get("agentsview-font-scale")).toBe("120");
      expect(document.documentElement.style.getPropertyValue("zoom")).toBe("1.3");
    });
  });

  describe("highContrast", () => {
    beforeEach(() => {
      if (ui.highContrast) ui.toggleHighContrast();
    });

    it("defaults to false", () => {
      expect(ui.highContrast).toBe(false);
    });

    it("toggles the value", () => {
      ui.toggleHighContrast();
      expect(ui.highContrast).toBe(true);
      ui.toggleHighContrast();
      expect(ui.highContrast).toBe(false);
    });

    it("toggles the root class and persists", async () => {
      const original = globalThis.localStorage;
      const setItem = vi.fn();
      Object.defineProperty(globalThis, "localStorage", {
        value: { getItem: vi.fn(() => null), setItem },
        writable: true,
        configurable: true,
      });
      try {
        // @ts-expect-error -- query string busts module cache
        const mod = await import("./ui.svelte.js?highContrastToggle");
        setItem.mockClear();
        mod.ui.toggleHighContrast();
        await tick();
        expect(document.documentElement.classList.contains("high-contrast")).toBe(true);
        // kit-ui's theme store persists high contrast under the key derived
        // from the app's "theme" storage key.
        expect(setItem).toHaveBeenCalledWith("theme-high-contrast", "true");
        mod.ui.toggleHighContrast();
        await tick();
        expect(document.documentElement.classList.contains("high-contrast")).toBe(false);
      } finally {
        document.documentElement.classList.remove("high-contrast");
        Object.defineProperty(globalThis, "localStorage", {
          value: original,
          writable: true,
          configurable: true,
        });
      }
    });
  });
});
