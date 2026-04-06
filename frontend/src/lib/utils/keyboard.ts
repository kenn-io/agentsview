import { ui } from "../stores/ui.svelte.js";
import { sessions } from "../stores/sessions.svelte.js";
import { starred } from "../stores/starred.svelte.js";
import { sync } from "../stores/sync.svelte.js";
import { router } from "../stores/router.svelte.js";
import { inSessionSearch } from "../stores/inSessionSearch.svelte.js";
import {
  getExportUrl,
  resumeSession,
} from "../api/client.js";
import {
  supportsResume,
  buildResumeCommand,
  formatResumeResponseCommand,
} from "./resume.js";
import { copyToClipboard } from "./clipboard.js";

function isInputFocused(): boolean {
  const el = document.activeElement;
  if (!el) return false;
  const tag = el.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    (el as HTMLElement).isContentEditable
  );
}

function isFindInput(): boolean {
  const el = document.activeElement;
  return (
    el instanceof HTMLInputElement &&
    el.getAttribute("aria-label") === "Search query"
  );
}

interface ShortcutOptions {
  navigateMessage: (delta: number) => void;
}

function handleEscape(): void {
  if (inSessionSearch.isOpen) {
    inSessionSearch.close();
    return;
  }
  if (ui.activeModal !== null) {
    ui.activeModal = null;
    return;
  }
  if (sessions.activeSessionId && !isInputFocused()) {
    sessions.deselectSession();
  }
}

/**
 * Register global keyboard shortcuts.
 * Returns a cleanup function to remove the listener.
 */
export function registerShortcuts(
  opts: ShortcutOptions,
): () => void {
  function handler(e: KeyboardEvent) {
    const meta = e.metaKey || e.ctrlKey;

    // Cmd+K — always works
    if (meta && e.key === "k") {
      e.preventDefault();
      ui.activeModal =
        ui.activeModal === "commandPalette"
          ? null
          : "commandPalette";
      return;
    }

    // Cmd+F — open in-session find when the session view is
    // active with a selected session. Allow from the find
    // input itself but not from other inputs (e.g. sidebar
    // typeahead) where native find should work normally.
    if (
      meta &&
      e.key === "f" &&
      router.route === "sessions" &&
      sessions.activeSessionId &&
      ui.activeModal === null &&
      (!isInputFocused() || isFindInput())
    ) {
      e.preventDefault();
      inSessionSearch.open();
      return;
    }

    // Cmd+G / Cmd+Shift+G — next/prev match while find is
    // open on the session view. Skip when a modal is open or
    // an unrelated input has focus.
    if (
      meta &&
      e.key === "g" &&
      router.route === "sessions" &&
      inSessionSearch.isOpen &&
      ui.activeModal === null &&
      (!isInputFocused() || isFindInput())
    ) {
      e.preventDefault();
      if (e.shiftKey) {
        inSessionSearch.prev();
      } else {
        inSessionSearch.next();
      }
      return;
    }

    // Zoom: Cmd+= / Cmd+- / Cmd+0 (desktop only)
    if (sync.isDesktop) {
      if (meta && (e.key === "=" || e.key === "+")) {
        e.preventDefault();
        ui.zoomIn();
        return;
      }
      if (meta && e.key === "-") {
        e.preventDefault();
        ui.zoomOut();
        return;
      }
      if (meta && e.key === "0") {
        e.preventDefault();
        ui.resetZoom();
        return;
      }
    }

    // Esc — always works
    if (e.key === "Escape") {
      handleEscape();
      return;
    }

    // All remaining shortcuts are plain single-key — skip if any modifier is held.
    // (Shift is allowed because "?" requires Shift on most layouts.)
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    // All other shortcuts: skip when modal open or input focused
    if (ui.activeModal !== null || isInputFocused()) return;

    const keyActions: Record<string, () => void> = {
      j: () => opts.navigateMessage(1),
      ArrowDown: () => opts.navigateMessage(1),
      k: () => opts.navigateMessage(-1),
      ArrowUp: () => opts.navigateMessage(-1),
      "]": () => {
        const filter = starred.filterOnly
          ? (s: { id: string }) => starred.isStarred(s.id)
          : undefined;
        sessions.navigateSession(1, filter);
      },
      "[": () => {
        const filter = starred.filterOnly
          ? (s: { id: string }) => starred.isStarred(s.id)
          : undefined;
        sessions.navigateSession(-1, filter);
      },
      o: () => ui.toggleSort(),
      l: () => ui.cycleLayout(),
      r: () => sync.triggerSync(),
      e: () => {
        if (sessions.activeSessionId) {
          window.open(
            getExportUrl(sessions.activeSessionId),
            "_blank",
          );
        }
      },
      p: () => {
        if (sessions.activeSessionId) {
          ui.activeModal = "publish";
        }
      },
      s: () => {
        if (sessions.activeSessionId) {
          starred.toggle(sessions.activeSessionId);
        }
      },
      c: () => {
        const session = sessions.activeSession;
        if (session && supportsResume(session.agent)) {
          // Copy a runnable resume command. Cursor needs the backend cwd
          // applied client-side so the copied command is self-contained.
          resumeSession(session.id, { command_only: true }).then((resp) => {
            const cmd = formatResumeResponseCommand(
              session.agent, resp,
            ) || buildResumeCommand(
              session.agent,
              session.id,
            );
            if (cmd) copyToClipboard(cmd);
          }).catch(() => {
            const cmd = buildResumeCommand(
              session.agent,
              session.id,
            );
            if (cmd) copyToClipboard(cmd);
          });
        }
      },
      "/": () => {
        if (sessions.activeSessionId) {
          inSessionSearch.open();
        }
      },
      Delete: () => {
        if (
          router.route === "sessions" &&
          sessions.activeSessionId
        ) {
          ui.activeModal = "confirmDelete";
        }
      },
      Backspace: () => {
        if (
          router.route === "sessions" &&
          sessions.activeSessionId
        ) {
          ui.activeModal = "confirmDelete";
        }
      },
      "?": () => {
        ui.activeModal = "shortcuts";
      },
      b: () => {
        if (router.route === "sessions") {
          ui.toggleSidebar();
        } else if (ui.isMobileViewport) {
          router.navigate("sessions");
          ui.sidebarOpen = true;
        } else {
          ui.toggleSidebar();
        }
      },
    };

    const action = keyActions[e.key];
    if (action) {
      e.preventDefault();
      action();
    }
  }

  document.addEventListener("keydown", handler);
  return () => document.removeEventListener("keydown", handler);
}
