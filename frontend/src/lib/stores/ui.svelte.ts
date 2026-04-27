import {
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_KEY,
  clampStoredSidebarWidth,
} from "../components/layout/sidebar-width.js";

type Theme = "light" | "dark";
export type MessageLayout = "default" | "compact" | "stream";
export type TranscriptMode = "normal" | "focused";
type ModalType =
  | "about"
  | "commandPalette"
  | "shortcuts"
  | "publish"
  | "resync"
  | "update"
  | "confirmDelete"
  | null;

/** Block types that can be toggled visible/hidden. */
export type BlockType =
  | "user"
  | "assistant"
  | "thinking"
  | "tool"
  | "code";

export const ALL_BLOCK_TYPES: BlockType[] = [
  "user",
  "assistant",
  "thinking",
  "tool",
  "code",
];

const BLOCK_FILTER_KEY = "agentsview-block-filters";
const TRANSCRIPT_MODE_KEY = "agentsview-transcript-mode";
const VITALS_KEY = "agentsview-session-vitals";
const SIGNAL_PANEL_KEY = "agentsview-signal-panel";

function readBlockFilters(): Set<BlockType> {
  try {
    const raw = localStorage?.getItem(BLOCK_FILTER_KEY);
    if (raw) {
      const arr = JSON.parse(raw);
      if (Array.isArray(arr)) {
        return new Set(
          arr.filter((t: string) =>
            ALL_BLOCK_TYPES.includes(t as BlockType),
          ) as BlockType[],
        );
      }
    }
  } catch {
    // ignore
  }
  return new Set(ALL_BLOCK_TYPES);
}

const LAYOUT_KEY = "agentsview-message-layout";
const ZOOM_KEY = "agentsview-zoom-level";
const VALID_TRANSCRIPT_MODES: TranscriptMode[] = [
  "normal",
  "focused",
];

const IS_DESKTOP =
  typeof window !== "undefined" &&
  new URLSearchParams(window.location.search).has(
    "desktop",
  );

const ZOOM_STEPS = [
  67, 75, 80, 90, 100, 110, 125, 150, 175, 200,
];
const ZOOM_DEFAULT = 100;

function readStoredZoom(): number {
  if (!IS_DESKTOP) return ZOOM_DEFAULT;
  try {
    const raw = localStorage?.getItem(ZOOM_KEY);
    if (raw) {
      const val = Number(raw);
      if (ZOOM_STEPS.includes(val)) return val;
    }
  } catch {
    // ignore
  }
  return ZOOM_DEFAULT;
}
const VALID_LAYOUTS: MessageLayout[] = [
  "default",
  "compact",
  "stream",
];
function readStoredTheme(): Theme | null {
  if (
    typeof localStorage !== "undefined" &&
    localStorage != null &&
    typeof localStorage.getItem === "function"
  ) {
    return localStorage.getItem("theme") as Theme;
  }
  return null;
}

function readStoredLayout(): MessageLayout {
  try {
    const raw = localStorage?.getItem(LAYOUT_KEY);
    if (
      raw &&
      VALID_LAYOUTS.includes(raw as MessageLayout)
    ) {
      return raw as MessageLayout;
    }
  } catch {
    // ignore
  }
  return "default";
}

function readStoredTranscriptMode(): TranscriptMode {
  try {
    const raw = localStorage?.getItem(TRANSCRIPT_MODE_KEY);
    if (
      raw &&
      VALID_TRANSCRIPT_MODES.includes(raw as TranscriptMode)
    ) {
      return raw as TranscriptMode;
    }
  } catch {
    // ignore
  }
  return "normal";
}

function readStoredSidebarWidth(): number {
  try {
    return clampStoredSidebarWidth(
      localStorage?.getItem(SIDEBAR_WIDTH_KEY),
    );
  } catch {
    return SIDEBAR_WIDTH_DEFAULT;
  }
}

function readStoredBool(key: string, fallback: boolean): boolean {
  try {
    const raw = localStorage?.getItem(key);
    if (raw === "true") return true;
    if (raw === "false") return false;
  } catch {
    // ignore
  }
  return fallback;
}
class UIStore {
  theme: Theme = $state(readStoredTheme() || "light");
  sortNewestFirst: boolean = $state(false);
  messageLayout: MessageLayout = $state(readStoredLayout());
  transcriptMode: TranscriptMode = $state(
    readStoredTranscriptMode(),
  );
  sidebarWidth: number = $state(readStoredSidebarWidth());
  activeModal: ModalType = $state(null);
  selectedOrdinal: number | null = $state(null);
  pendingScrollOrdinal: number | null = $state(null);
  pendingScrollSession: string | null = $state(null);

  zoomLevel: number = $state(readStoredZoom());

  sidebarOpen: boolean = $state(true);
  isMobileViewport: boolean = $state(false);
  vitalsOpen: boolean = $state(
    readStoredBool(VITALS_KEY, false),
  );
  signalPanelOpen: boolean = $state(
    readStoredBool(SIGNAL_PANEL_KEY, false),
  );

  /** Set of block types currently visible. */
  visibleBlocks: Set<BlockType> = $state(readBlockFilters());

  constructor() {
    $effect.root(() => {
      $effect(() => {
        const root = document.documentElement;
        if (this.theme === "dark") {
          root.classList.add("dark");
        } else {
          root.classList.remove("dark");
        }
        if (
          typeof localStorage !== "undefined" &&
          localStorage != null &&
          typeof localStorage.setItem === "function"
        ) {
          localStorage.setItem("theme", this.theme);
        }
      });

      $effect(() => {
        try {
          localStorage?.setItem(
            LAYOUT_KEY,
            this.messageLayout,
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        try {
          localStorage?.setItem(
            TRANSCRIPT_MODE_KEY,
            this.transcriptMode,
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        try {
          localStorage?.setItem(
            SIDEBAR_WIDTH_KEY,
            String(this.sidebarWidth),
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        if (!IS_DESKTOP) return;
        // "zoom" is non-standard but supported in WebKit/Chromium
        (
          document.documentElement.style as unknown as
            Record<string, string>
        ).zoom = String(this.zoomLevel / 100);
        try {
          localStorage?.setItem(
            ZOOM_KEY,
            String(this.zoomLevel),
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        try {
          localStorage?.setItem(
            VITALS_KEY,
            String(this.vitalsOpen),
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        try {
          localStorage?.setItem(
            SIGNAL_PANEL_KEY,
            String(this.signalPanelOpen),
          );
        } catch {
          // ignore
        }
      });

      // Initialize sidebar based on viewport width
      if (typeof window !== "undefined" && typeof window.matchMedia === "function") {
        const mq = window.matchMedia("(min-width: 768px)");
        this.sidebarOpen = mq.matches;
        this.isMobileViewport = !mq.matches;
        const onChange = (e: MediaQueryListEvent) => {
          this.sidebarOpen = e.matches;
          this.isMobileViewport = !e.matches;
        };
        if (mq.addEventListener) {
          mq.addEventListener("change", onChange);
        } else {
          mq.addListener(onChange);
        }
      }
    });

    // Allow parent windows to control theme via postMessage
    if (typeof window !== "undefined") {
      window.addEventListener("message", (event: MessageEvent) => {
        if (
          event.data &&
          event.data.type === "theme:set" &&
          (event.data.theme === "light" || event.data.theme === "dark")
        ) {
          this.theme = event.data.theme;
        }
      });
    }
  }

  toggleTheme() {
    this.theme = this.theme === "light" ? "dark" : "light";
  }

  isBlockVisible(type: BlockType): boolean {
    return this.visibleBlocks.has(type);
  }

  setBlockVisible(type: BlockType, visible: boolean) {
    const next = new Set(this.visibleBlocks);
    if (visible) {
      next.add(type);
    } else {
      next.delete(type);
    }
    this.visibleBlocks = next;
    this.persistBlockFilters();
  }

  toggleBlock(type: BlockType) {
    const next = new Set(this.visibleBlocks);
    if (next.has(type)) {
      next.delete(type);
    } else {
      next.add(type);
    }
    this.visibleBlocks = next;
    this.persistBlockFilters();
  }

  showAllBlocks() {
    this.visibleBlocks = new Set(ALL_BLOCK_TYPES);
    this.persistBlockFilters();
  }

  get hiddenBlockCount(): number {
    return ALL_BLOCK_TYPES.length - this.visibleBlocks.size;
  }

  get hasBlockFilters(): boolean {
    return this.visibleBlocks.size < ALL_BLOCK_TYPES.length;
  }

  private persistBlockFilters() {
    try {
      localStorage?.setItem(
        BLOCK_FILTER_KEY,
        JSON.stringify([...this.visibleBlocks]),
      );
    } catch {
      // ignore
    }
  }

  toggleSort() {
    this.sortNewestFirst = !this.sortNewestFirst;
  }

  cycleLayout() {
    const idx = VALID_LAYOUTS.indexOf(this.messageLayout);
    this.messageLayout =
      VALID_LAYOUTS[(idx + 1) % VALID_LAYOUTS.length]!;
  }

  setLayout(layout: MessageLayout) {
    this.messageLayout = layout;
  }

  setTranscriptMode(mode: TranscriptMode) {
    this.transcriptMode = mode;
  }

  setSidebarWidth(width: number) {
    this.sidebarWidth = clampStoredSidebarWidth(width);
  }

  selectOrdinal(ordinal: number) {
    this.selectedOrdinal = ordinal;
  }

  clearSelection() {
    this.selectedOrdinal = null;
  }

  clearScrollState() {
    this.selectedOrdinal = null;
    this.pendingScrollOrdinal = null;
    this.pendingScrollSession = null;
  }

  scrollToOrdinal(ordinal: number, sessionId?: string) {
    this.selectedOrdinal = ordinal;
    this.pendingScrollOrdinal = ordinal;
    this.pendingScrollSession = sessionId ?? null;
  }

  zoomIn() {
    const idx = ZOOM_STEPS.indexOf(this.zoomLevel);
    if (idx < ZOOM_STEPS.length - 1) {
      this.zoomLevel = ZOOM_STEPS[idx + 1]!;
    }
  }

  zoomOut() {
    const idx = ZOOM_STEPS.indexOf(this.zoomLevel);
    if (idx > 0) {
      this.zoomLevel = ZOOM_STEPS[idx - 1]!;
    }
  }

  resetZoom() {
    this.zoomLevel = ZOOM_DEFAULT;
  }

  toggleSidebar() {
    this.sidebarOpen = !this.sidebarOpen;
  }

  closeSidebar() {
    this.sidebarOpen = false;
  }

  toggleVitals() {
    this.vitalsOpen = !this.vitalsOpen;
  }

  toggleSignalPanel() {
    this.signalPanelOpen = !this.signalPanelOpen;
  }

  closeAll() {
    this.activeModal = null;
  }
}

export const ui = new UIStore();
