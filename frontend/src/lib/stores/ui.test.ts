import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
} from "vite-plus/test";
import { tick } from "svelte";
import {
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_KEY,
  SIDEBAR_WIDTH_MIN,
  SIDEBAR_WIDTH_STORAGE_MAX,
} from "../components/layout/sidebar-width.js";
import { ui } from "./ui.svelte.js";

describe("UIStore", () => {
  beforeEach(() => {
    ui.activeModal = null;
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
        expect(getItem.mock.calls).toContainEqual([
          SIDEBAR_WIDTH_KEY,
        ]);
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
              key === SIDEBAR_WIDTH_KEY
                ? String(SIDEBAR_WIDTH_MIN - 50)
                : null,
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
              key === SIDEBAR_WIDTH_KEY
                ? String(SIDEBAR_WIDTH_STORAGE_MAX + 50)
                : null,
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
            getItem: vi.fn((key: string) =>
              key === SIDEBAR_WIDTH_KEY ? "300" : null,
            ),
            setItem: vi.fn(),
          },
          writable: true,
          configurable: true,
        });
        // @ts-expect-error -- query string busts module cache
        const stringMod = await import("./ui.svelte.js?sidebarWidthStoredString");

        expect(minMod.ui.sidebarWidth).toBe(SIDEBAR_WIDTH_MIN);
        expect(maxMod.ui.sidebarWidth).toBe(
          SIDEBAR_WIDTH_STORAGE_MAX,
        );
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
        expect(setItem).toHaveBeenLastCalledWith(
          SIDEBAR_WIDTH_KEY,
          String(SIDEBAR_WIDTH_MIN),
        );

        setItem.mockClear();
        mod.ui.setSidebarWidth(SIDEBAR_WIDTH_STORAGE_MAX + 10);
        await tick();
        expect(mod.ui.sidebarWidth).toBe(
          SIDEBAR_WIDTH_STORAGE_MAX,
        );
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
        expect(() =>
          mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 20),
        ).not.toThrow();
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
        expect(() =>
          mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 15),
        ).not.toThrow();
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
        expect(() =>
          mod.ui.setSidebarWidth(SIDEBAR_WIDTH_DEFAULT + 25),
        ).not.toThrow();
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
      window.matchMedia = vi.fn().mockReturnValue({
        matches: false,
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
      window.matchMedia = vi.fn().mockReturnValue({
        matches: true,
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
        expect(setItem).toHaveBeenLastCalledWith(
          "agentsview-transcript-mode",
          "focused",
        );
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
            key === "agentsview-transcript-mode"
              ? "detailed"
              : null,
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
});
