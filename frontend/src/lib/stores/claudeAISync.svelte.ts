import type { ImporterImportStats as ImportStats } from "../api/generated/index.js";
import type { BrowserHost } from "../api/browserHost.js";
import { syncClaudeAI } from "../api/client.js";
import { ApiError } from "../api/runtime.js";
import { m } from "../i18n/index.js";
import { reportSyncFailed } from "../utils/telemetry.js";

// Claude.ai Sync outlives the import dialog: closing the dialog hides it,
// and only stop() cancels the run. The outcome stays until dismiss().
class ClaudeAISyncStore {
  running = $state(false);
  progress = $state<ImportStats | null>(null);
  result = $state<ImportStats | null>(null);
  error = $state<string | null>(null);
  // The error code the dialog shows as a connection state instead of an error.
  flag = $state<string | null>(null);
  private controller: AbortController | undefined;

  async start(host: BrowserHost | undefined, onimported: () => void) {
    if (this.running) return;
    this.dismiss();
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
      const stats = await syncClaudeAI(host, callbacks, controller.signal);
      if (!controller.signal.aborted) this.result = stats;
    } catch (e) {
      if (controller.signal.aborted) return;
      reportSyncFailed(e);
      if (e instanceof ApiError && e.code === "claude_ai_chrome_host_required") {
        return;
      } else if (
        e instanceof ApiError &&
        ["claude_ai_auth_required", "claude_ai_chrome_host_update_required"].includes(e.code ?? "")
      ) {
        this.flag = e.code ?? null;
      } else if (e instanceof ApiError && e.code === "claude_ai_sync_running") {
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
