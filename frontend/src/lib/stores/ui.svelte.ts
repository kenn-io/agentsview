import {
  getHighContrast,
  initTheme,
  isDark,
  setHighContrast,
  setThemeMode,
} from "@kenn-io/kit-ui";
import {
  SIDEBAR_WIDTH_DEFAULT,
  SIDEBAR_WIDTH_KEY,
  clampStoredSidebarWidth,
} from "../components/layout/sidebar-width.js";

type Theme = "light" | "dark";
export type MessageLayout = "default" | "compact" | "stream" | "skim";
export type TranscriptMode = "normal" | "focused";
export type PublishTarget =
  | { kind: "session"; id: string }
  | { kind: "insight"; id: number }
  | null;
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
const FOLLOW_LATEST_KEY = "agentsview-follow-latest";

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
const FONT_SCALE_KEY = "agentsview-font-scale";
const HIGH_CONTRAST_KEY = "agentsview-high-contrast";
export const FONT_SCALE_STEPS = [90, 100, 110, 120, 130];
const FONT_SCALE_DEFAULT = 100;

type DesktopTauriWebviewWindow = {
  setZoom(scaleFactor: number): Promise<void>;
};

type DesktopTauriBridge = {
  webviewWindow?: {
    getCurrentWebviewWindow?: () => DesktopTauriWebviewWindow;
  };
};

function currentDesktopWebviewWindow():
  | DesktopTauriWebviewWindow
  | undefined {
  if (!IS_DESKTOP || typeof window === "undefined") return;
  const tauri =
    (window as Window & { __TAURI__?: DesktopTauriBridge })
      .__TAURI__;
  return tauri?.webviewWindow?.getCurrentWebviewWindow?.();
}

function syncDesktopZoom(scaleFactor: number): boolean {
  const webview = currentDesktopWebviewWindow();
  if (!webview) return false;
  void webview.setZoom(scaleFactor).catch(() => {
    // ignore
  });
  return true;
}

function composedRootZoom(
  fontScale: number, zoomLevel: number,
): string {
  let scale = fontScale / 100;
  if (IS_DESKTOP && !currentDesktopWebviewWindow()) {
    scale *= zoomLevel / 100;
  }
  return String(scale);
}

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

function readStoredFontScale(): number {
  try {
    const raw = localStorage?.getItem(FONT_SCALE_KEY);
    if (raw) {
      const val = Number(raw);
      if (FONT_SCALE_STEPS.includes(val)) return val;
    }
  } catch {
    // ignore
  }
  return FONT_SCALE_DEFAULT;
}

const VALID_LAYOUTS: MessageLayout[] = [
  "default",
  "compact",
  "stream",
  "skim",
];
// Theme state lives in kit-ui's theme store (mode/high-contrast persistence,
// root class management, and OS-preference tracking in "system" mode). Reuse
// the app's historical "theme" storage key — its stored "light"/"dark" values
// are valid kit-ui modes — and migrate the legacy high-contrast key to the
// derived key kit-ui persists under.
function migrateHighContrastKey(): void {
  try {
    if (
      typeof localStorage === "undefined" ||
      localStorage == null ||
      typeof localStorage.getItem !== "function"
    ) {
      return;
    }
    const legacy = localStorage.getItem(HIGH_CONTRAST_KEY);
    if (legacy !== null && localStorage.getItem("theme-high-contrast") === null) {
      localStorage.setItem("theme-high-contrast", legacy);
    }
  } catch {
    // Storage blocked — kit-ui falls back to in-memory state.
  }
}

migrateHighContrastKey();
initTheme({ storageKey: "theme" });

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
  /** Resolved appearance from kit-ui's theme store; in "system" mode this
   * tracks the OS preference. Assigning pins an explicit mode. */
  get theme(): Theme {
    return isDark() ? "dark" : "light";
  }

  set theme(value: Theme) {
    setThemeMode(value);
  }

  get highContrast(): boolean {
    return getHighContrast();
  }

  set highContrast(value: boolean) {
    setHighContrast(value);
  }

  sortNewestFirst: boolean = $state(false);
  messageLayout: MessageLayout = $state(readStoredLayout());
  transcriptMode: TranscriptMode = $state(
    readStoredTranscriptMode(),
  );
  sidebarWidth: number = $state(readStoredSidebarWidth());
  activeModal: ModalType = $state(null);
  /** Whether the next gist publish should be secret instead of public. */
  publishSecret: boolean = $state(false);
  publishTarget: PublishTarget = $state(null);
  selectedOrdinal: number | null = $state(null);
  pendingScrollOrdinal: number | null = $state(null);
  pendingScrollSession: string | null = $state(null);

  zoomLevel: number = $state(readStoredZoom());
  fontScale: number = $state(readStoredFontScale());

  sidebarOpen: boolean = $state(true);
  isMobileViewport: boolean = $state(false);
  vitalsOpen: boolean = $state(
    readStoredBool(VITALS_KEY, false),
  );
  signalPanelOpen: boolean = $state(
    readStoredBool(SIGNAL_PANEL_KEY, false),
  );
  followLatest: boolean = $state(
    readStoredBool(FOLLOW_LATEST_KEY, false),
  );
  followLatestRequest: number = $state(0);

  /** Set of block types currently visible. */
  visibleBlocks: Set<BlockType> = $state(readBlockFilters());

  constructor() {
    $effect.root(() => {
      // Theme and high-contrast classes/persistence are owned by kit-ui's
      // theme store (initTheme above); no effects needed here.
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

      // Apply the root font scale in the document; desktop zoom
      // falls back to CSS when the native webview bridge is absent.
      $effect(() => {
        (
          document.documentElement.style as unknown as
            Record<string, string>
        ).zoom = composedRootZoom(
          this.fontScale,
          this.zoomLevel,
        );
      });

      // Persist the desktop window zoom (desktop only).
      $effect(() => {
        if (IS_DESKTOP) {
          syncDesktopZoom(this.zoomLevel / 100);
          try {
            localStorage?.setItem(
              ZOOM_KEY,
              String(this.zoomLevel),
            );
          } catch {
            // ignore
          }
        }
      });

      // Persist the font scale (web and desktop).
      $effect(() => {
        try {
          localStorage?.setItem(
            FONT_SCALE_KEY,
            String(this.fontScale),
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

      $effect(() => {
        try {
          localStorage?.setItem(
            FOLLOW_LATEST_KEY,
            String(this.followLatest),
          );
        } catch {
          // ignore
        }
      });

      $effect(() => {
        if (this.activeModal !== "publish") {
          this.publishTarget = null;
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

  setPublishTarget(target: Exclude<PublishTarget, null>) {
    this.publishTarget = target;
  }

  clearPublishTarget() {
    this.publishTarget = null;
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
    this.followLatest = false;
    this.selectedOrdinal = ordinal;
    this.pendingScrollOrdinal = ordinal;
    this.pendingScrollSession = sessionId ?? null;
  }

  setFollowLatest(enabled: boolean) {
    this.followLatest = enabled;
    if (enabled) {
      this.followLatestRequest += 1;
      this.selectedOrdinal = null;
      this.pendingScrollOrdinal = null;
      this.pendingScrollSession = null;
    }
  }

  toggleFollowLatest() {
    this.setFollowLatest(!this.followLatest);
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

  setFontScale(scale: number) {
    if (FONT_SCALE_STEPS.includes(scale)) {
      this.fontScale = scale;
    }
  }

  toggleHighContrast() {
    this.highContrast = !this.highContrast;
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

  closeVitals() {
    this.vitalsOpen = false;
  }

  toggleSignalPanel() {
    this.signalPanelOpen = !this.signalPanelOpen;
  }

  closeAll() {
    this.activeModal = null;
  }
}

export const ui = new UIStore();
