import type { ImporterImportStats as ImportStats } from "../api/generated/index.js";
import type { BrowserHost } from "../api/browserHost.js";
import { syncChatGPTChrome, syncClaudeAI } from "../api/client.js";
import { ApiError } from "../api/runtime.js";
import { m } from "../i18n/index.js";
import { reportSyncFailed } from "../utils/telemetry.js";

export type BrowserSyncProvider = "claude-ai" | "chatgpt";

// Claude.ai and ChatGPT report the same conditions under different codes.
const hostRequiredCodes = ["claude_ai_chrome_host_required", "chrome_host_required"];
const flagCodes = [
  "claude_ai_auth_required",
  "claude_ai_chrome_host_update_required",
  "chatgpt_auth_required",
  "chrome_host_update_required",
];
const runningCodes = ["claude_ai_sync_running", "chrome_sync_running"];

// Browser Sync outlives the import dialog: closing the dialog hides it,
// and only stop() cancels the run. The outcome stays until dismiss().
class ClaudeAISyncStore {
  running = $state(false);
  // The provider of the current run, or of the outcome shown after it.
  provider = $state<BrowserSyncProvider>("claude-ai");
  progress = $state<ImportStats | null>(null);
  result = $state<ImportStats | null>(null);
  error = $state<string | null>(null);
  // The error code the dialog shows as a connection state instead of an error.
  flag = $state<string | null>(null);
  private controller: AbortController | undefined;

  async start(
    provider: BrowserSyncProvider,
    host: BrowserHost | undefined,
    onimported: () => void,
  ) {
    if (this.running) return;
    this.dismiss();
    this.provider = provider;
    this.running = true;
    const controller = new AbortController();
    this.controller = controller;
    const callbacks = {
      onProgress: (stats: ImportStats) => {
        if (controller === this.controller) this.progress = stats;
      },
    };
    let refreshAfterSync = true;
    try {
      const stats =
        provider === "chatgpt"
          ? await syncChatGPTChrome(callbacks, controller.signal)
          : await syncClaudeAI(host, callbacks, controller.signal);
      if (!controller.signal.aborted) this.result = stats;
    } catch (e) {
      if (controller.signal.aborted) return;
      reportSyncFailed(e);
      if (e instanceof ApiError && hostRequiredCodes.includes(e.code ?? "")) {
        return;
      } else if (e instanceof ApiError && flagCodes.includes(e.code ?? "")) {
        this.flag = e.code ?? null;
      } else if (e instanceof ApiError && runningCodes.includes(e.code ?? "")) {
        this.error = m.import_claude_sync_running();
        refreshAfterSync = false;
      } else {
        this.error = e instanceof Error ? e.message : m.import_failed();
      }
    } finally {
      this.end(controller);
      if (refreshAfterSync) onimported();
    }
  }

  // Stopping releases the visible state while cancellation settles.
  stop() {
    this.controller?.abort();
    this.end(this.controller);
  }

  dismiss() {
    this.progress = null;
    this.result = null;
    this.error = null;
    this.flag = null;
  }

  private end(controller?: AbortController) {
    if (controller !== this.controller) return;
    this.running = false;
    this.controller = undefined;
  }
}

export const claudeAISync = new ClaudeAISyncStore();
