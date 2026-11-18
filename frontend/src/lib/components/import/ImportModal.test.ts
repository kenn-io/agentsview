import { reportSyncFailed } from "../../utils/telemetry.js";
vi.mock("../../utils/telemetry.js", () => ({
  reportSyncFailed: vi.fn(),
}));
// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";
import { setLocale } from "../../paraglide/runtime.js";
import { ApiError } from "../../api/runtime.js";
import { sync as syncState } from "../../stores/sync.svelte.js";
import { browserSync } from "../../stores/browserSync.svelte.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), close: vi.fn(), fetch: vi.fn() }));
const getBrowserHost = vi.hoisted(() => vi.fn());
const isRemoteConnection = vi.hoisted(() => vi.fn());
const chromeStatus = vi.hoisted(() => vi.fn());
const syncBrowser = vi.hoisted(() => vi.fn());
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...(await original<typeof import("../../api/runtime.js")>()),
  isRemoteConnection,
}));
vi.mock("../../api/generated/index.js", async (original) => ({
  ...(await original<typeof import("../../api/generated/index.js")>()),
  ImportService: { getApiV1ImportClaudeAiChrome: chromeStatus },
}));
vi.mock("../../api/client.js", async (original) => ({
  relayHost: (await original<typeof import("../../api/client.js")>()).relayHost,
  connectClaudeAI: (browser: typeof host) => browser.connect(),
  syncBrowser,
  importClaudeAI: vi.fn(),
  importChatGPT: vi.fn(),
}));
const ready = { connected: true };
const stats = { imported: 12, updated: 3, skipped: 297, errors: 0 };
const props = () => ({ open: true, onclose: vi.fn(), onimported: vi.fn() });
const syncButton = () => screen.getByRole("button", { name: m.import_sync() }) as HTMLButtonElement;
async function clickSync() {
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  await fireEvent.click(syncButton());
}
beforeEach(() => {
  vi.spyOn(syncState, "readOnly", "get").mockReturnValue(false);
  isRemoteConnection.mockReturnValue(false);
  getBrowserHost.mockReturnValue(undefined);
  chromeStatus.mockResolvedValue(ready);
  syncBrowser.mockResolvedValue(stats);
  void setLocale("en", { reload: false });
});
afterEach(() => {
  browserSync.stop();
  browserSync.dismiss();
  vi.useRealTimers();
  vi.resetAllMocks();
  vi.restoreAllMocks();
  void setLocale("en", { reload: false });
});

it("disables Sync while checking, then shows the live connection", async () => {
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockReturnValue(
    new Promise((r) => {
      resolve = r;
    }),
  );
  render(ImportModal, props());
  expect(syncButton().disabled).toBe(true);
  expect(screen.queryByText("Connected")).toBeNull();
  resolve(ready);
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  expect(screen.getByText("Connected")).toBeTruthy();
  expect(
    screen.getByText(
      "Sync opens a background claude.ai tab if none is open. The tab stays open after Sync.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
});

it("shows setup instructions when disconnected", async () => {
  chromeStatus.mockResolvedValue({ connected: false });
  render(ImportModal, props());
  await waitFor(() => expect(screen.getByText("Not connected")).toBeTruthy());
  expect(syncButton().disabled).toBe(true);
  expect(
    screen.getByText(
      "Open Chrome with the AgentsView extension enabled. If you ran setup after starting AgentsView, restart AgentsView.",
    ),
  ).toBeTruthy();
  expect(screen.getByText("Terminal")).toBeTruthy();
  expect(screen.getByText("agentsview source chrome setup")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Copy command" })).toBeTruthy();
  expect(screen.getByText(m.import_sync_setup_load({ url: "chrome://extensions" }))).toBeTruthy();
});

it("syncs ChatGPT through Chrome above the ZIP upload and opens ChatGPT sign-in after auth failure", async () => {
  syncBrowser.mockRejectedValueOnce(
    new ApiError(0, "Server auth text", "browser_sync_auth_required"),
  );
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  render(ImportModal, props());
  await fireEvent.click(screen.getByRole("button", { name: "ChatGPT" }));
  await waitFor(() => expect(screen.getByText("Connected")).toBeTruthy());
  const tabNote = screen.getByText(
    "Sync opens a background chatgpt.com tab if none is open. The tab stays open after Sync.",
  );
  const upload = screen.getByText(m.import_drop_here());
  expect(tabNote.compareDocumentPosition(upload) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await clickSync();
  expect(syncBrowser).toHaveBeenCalledExactlyOnceWith(
    "chatgpt",
    undefined,
    expect.objectContaining({ onProgress: expect.any(Function) }),
    expect.any(AbortSignal),
  );
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  const signedOut =
    "Sign in to chatgpt.com in the Chrome profile with the AgentsView extension. Enable the extension in only one Chrome profile. Sync uses the first profile that connects.";
  expect(screen.getByText(signedOut)).toBeTruthy();
  expect(screen.queryByText("Server auth text")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: signedOut }));
  expect(open).toHaveBeenCalledExactlyOnceWith(
    "https://chatgpt.com/",
    "_blank",
    "noopener,noreferrer",
  );
});

it("uses Chrome for ChatGPT in the desktop app and polls its status", async () => {
  getBrowserHost.mockReturnValue(host);
  chromeStatus.mockResolvedValue({ connected: false });
  render(ImportModal, props());
  expect(screen.getByText("Account")).toBeTruthy();
  expect(chromeStatus).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "ChatGPT" }));
  await waitFor(() => expect(screen.getByText("Not connected")).toBeTruthy());
  expect(screen.getByText("Chrome")).toBeTruthy();
  expect(screen.queryByText("Account")).toBeNull();
  expect(screen.queryByText(m.import_claude_desktop_note())).toBeNull();
  expect(screen.getByText("agentsview source chrome setup")).toBeTruthy();
  expect(syncButton().disabled).toBe(true);
  chromeStatus.mockResolvedValue(ready);
  await fireEvent.focus(window);
  await clickSync();
  expect(syncBrowser).toHaveBeenCalledExactlyOnceWith(
    "chatgpt",
    undefined,
    expect.objectContaining({ onProgress: expect.any(Function) }),
    expect.any(AbortSignal),
  );
  expect(host.connect).not.toHaveBeenCalled();
});

it("clears a failed Sync when switching providers so Retry cannot cross providers", async () => {
  syncBrowser.mockRejectedValueOnce(new Error("Claude.ai failed"));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Claude.ai failed")).toBeTruthy());
  await fireEvent.click(screen.getByRole("button", { name: "ChatGPT" }));
  expect(screen.queryByText("Claude.ai failed")).toBeNull();
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  await clickSync();
  expect(syncBrowser.mock.calls.map((call) => call[0])).toEqual(["claude-ai", "chatgpt"]);
});

function pendingSync() {
  const run = {} as {
    signal: AbortSignal;
    progress: (value: typeof stats) => void;
    resolve: (value: typeof stats) => void;
    reject: (error: Error) => void;
  };
  syncBrowser.mockImplementation((_platform, _host, callbacks, signal) => {
    run.signal = signal;
    run.progress = callbacks.onProgress;
    return new Promise((resolve, reject) => {
      run.resolve = resolve;
      run.reject = reject;
    });
  });
  return run;
}

it("cancels Sync with Stop and refreshes completed chats", async () => {
  const run = pendingSync();
  const p = props();
  render(ImportModal, p);
  await clickSync();
  expect(screen.getByText("Syncing...")).toBeTruthy();
  expect(screen.getByText(m.import_sync_background())).toBeTruthy();
  expect(screen.queryByText(m.import_drop_here())).toBeNull();
  run.progress({ imported: 128, updated: 0, skipped: 0, errors: 0 });
  await tick();
  expect(screen.getByText("128 conversations processed...")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  expect(run.signal.aborted).toBe(true);
  expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  expect(screen.queryByText("128 conversations processed...")).toBeNull();
  expect(syncButton().disabled).toBe(false);
  run.reject(new Error("Stopped"));
  await waitFor(() => expect(p.onimported).toHaveBeenCalledOnce());
  expect(p.onclose).not.toHaveBeenCalled();
  expect(screen.queryByText("Stopped")).toBeNull();
});

it.each(["close button", "dialog X", "overlay", "escape"])(
  "keeps Sync running after closing with %s and shows its progress on reopen",
  async (exit) => {
    const run = pendingSync();
    const p = props();
    const view = render(ImportModal, p);
    await clickSync();
    run.progress({ imported: 128, updated: 0, skipped: 0, errors: 0 });
    await tick();
    if (exit === "overlay") {
      const overlay = screen.getByRole("dialog").parentElement!;
      await fireEvent.pointerDown(overlay);
      await fireEvent.pointerUp(overlay);
      await fireEvent.click(overlay);
    } else if (exit === "escape") {
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    } else {
      const name = exit === "close button" ? "Close" : m.import_close();
      await fireEvent.click(screen.getByRole("button", { name }));
    }
    expect(p.onclose).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(run.signal.aborted).toBe(false);
    run.progress({ imported: 200, updated: 0, skipped: 0, errors: 0 });
    await view.rerender({ ...p, open: true });
    expect(screen.getByText("200 conversations processed...")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    run.resolve(stats);
    await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
    expect(p.onimported).toHaveBeenCalledOnce();
  },
);

it("shows a result or failure that arrived while the dialog was closed, until the next close", async () => {
  const run = pendingSync();
  const p = props();
  const view = render(ImportModal, p);
  await clickSync();
  await view.rerender({ ...p, open: false });
  run.reject(new Error("Network interrupted"));
  await waitFor(() => expect(p.onimported).toHaveBeenCalledOnce());
  await view.rerender({ ...p, open: true });
  expect(screen.getByRole("alert").textContent).toContain("Network interrupted");
  await view.rerender({ ...p, open: false });
  await view.rerender({ ...p, open: true });
  expect(screen.queryByRole("alert")).toBeNull();
});

it("reuses results and returns to Ready with Import more", async () => {
  const p = props();
  render(ImportModal, p);
  await clickSync();
  await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
  for (const count of ["12", "3", "297"]) expect(screen.getByText(count)).toBeTruthy();
  expect(p.onimported).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Import more" }));
  expect(screen.getByText("Connected")).toBeTruthy();
  expect(syncButton().disabled).toBe(false);
});

it("queues a new Sync behind a stopped run and ignores the old run's progress and cleanup", async () => {
  let rejectOld!: (error: Error) => void;
  let rejectQueued!: (error: Error) => void;
  let resolveRetry!: (result: typeof stats) => void;
  let queuedSignal!: AbortSignal;
  const progress: ((value: typeof stats) => void)[] = [];
  // The client starts a new run only after the stopped run settles, so the
  // queued call stays pending while the old run finishes.
  syncBrowser
    .mockImplementationOnce((_platform, _host, callbacks) => {
      progress.push(callbacks.onProgress);
      return new Promise((_, reject) => {
        rejectOld = reject;
      });
    })
    .mockImplementationOnce((_platform, _host, _callbacks, signal) => {
      queuedSignal = signal;
      return new Promise((_, reject) => {
        rejectQueued = reject;
      });
    })
    .mockImplementationOnce((_platform, _host, callbacks) => {
      progress.push(callbacks.onProgress);
      return new Promise<typeof stats>((resolve) => {
        resolveRetry = resolve;
      });
    });
  const p = props();
  render(ImportModal, p);
  await clickSync();
  await fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  await clickSync();
  expect(syncBrowser).toHaveBeenCalledTimes(2);
  progress[0]!({ imported: 128, updated: 0, skipped: 0, errors: 0 });
  rejectOld(new Error("Stopped old run"));
  await waitFor(() => expect(p.onimported).toHaveBeenCalledOnce());
  expect(screen.queryByText("128 conversations processed...")).toBeNull();
  expect(screen.getByText("Syncing...")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  expect(queuedSignal.aborted).toBe(false);
  rejectQueued(new ApiError(409, "Server conflict text", "chrome_sync_running"));
  await waitFor(() =>
    expect(screen.getByRole("alert").textContent).toContain("Chrome Sync is already running."),
  );
  expect(p.onimported).toHaveBeenCalledOnce();
  expect(screen.queryByText("Server conflict text")).toBeNull();
  expect(reportSyncFailed).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ code: "chrome_sync_running" }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(syncBrowser).toHaveBeenCalledTimes(3);
  progress[1]!({ imported: 7, updated: 0, skipped: 0, errors: 0 });
  await tick();
  expect(screen.getByText("7 conversations processed...")).toBeTruthy();
  resolveRetry(stats);
  await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
  expect(p.onimported).toHaveBeenCalledTimes(2);
});

it("offers browser sign-in after auth failure", async () => {
  syncBrowser.mockRejectedValueOnce(
    new ApiError(0, "Server auth text", "browser_sync_auth_required"),
  );
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  expect(
    screen.getByText(
      "Sign in to claude.ai in the Chrome profile with the AgentsView extension. Enable the extension in only one Chrome profile. Sync uses the first profile that connects.",
    ),
  ).toBeTruthy();
  expect(screen.queryByText("Server auth text")).toBeNull();
  await fireEvent.click(
    screen.getByRole("button", {
      name: "Sign in to claude.ai in the Chrome profile with the AgentsView extension. Enable the extension in only one Chrome profile. Sync uses the first profile that connects.",
    }),
  );
  expect(open).toHaveBeenCalledExactlyOnceWith(
    "https://claude.ai/login?return_url=%2Fnew",
    "_blank",
    "noopener,noreferrer",
  );
});

it.each(["browser_sync_auth_required", "chrome_host_update_required"])(
  "recovers from %s without a reload",
  async (code) => {
    syncBrowser.mockRejectedValueOnce(new ApiError(0, "Server error text", code));
    render(ImportModal, props());
    await clickSync();
    await waitFor(() =>
      expect(
        screen.getByText(code === "browser_sync_auth_required" ? "Signed out" : "Update needed"),
      ).toBeTruthy(),
    );
    if (code === "chrome_host_update_required") {
      chromeStatus.mockResolvedValue({ connected: false });
      await fireEvent.focus(window);
      await tick();
      expect(screen.getByText("Restart AgentsView if you upgraded it.")).toBeTruthy();
      expect(screen.getByText("agentsview source chrome setup")).toBeTruthy();
    }
    expect(syncButton().disabled).toBe(false);
    await clickSync();
    await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
    expect(screen.queryByText("Signed out")).toBeNull();
    expect(screen.queryByText("Update needed")).toBeNull();
  },
);

it("clears a Sync conflict after closing while status hangs", async () => {
  syncBrowser.mockRejectedValueOnce(
    new ApiError(409, "Server conflict text", "chrome_sync_running"),
  );
  const p = props();
  const view = render(ImportModal, p);
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockReturnValue(
    new Promise((r) => {
      resolve = r;
    }),
  );
  await fireEvent.click(syncButton());
  await waitFor(() =>
    expect(screen.getByRole("alert").textContent).toContain("Chrome Sync is already running."),
  );
  await waitFor(() => expect(chromeStatus).toHaveBeenCalledTimes(2));
  await view.rerender({ ...p, open: false });
  await view.rerender({ ...p, open: true });
  resolve({ connected: false });
  await waitFor(() => expect(screen.getByText("Not connected")).toBeTruthy());
  expect(screen.queryByText("Chrome Sync is already running.")).toBeNull();
});

it.each([
  new Error("Network interrupted"),
  new ApiError(0, "Auth required", "browser_sync_auth_required"),
])("keeps processed counts after a partial Sync fails with %s", async (error) => {
  syncBrowser.mockImplementationOnce(async (_platform, _host, callbacks) => {
    callbacks.onProgress({ imported: 128, updated: 3, skipped: 7, errors: 1 });
    throw error;
  });
  const p = props();
  render(ImportModal, p);
  await clickSync();
  await waitFor(() => expect(screen.getByText("139 conversations processed")).toBeTruthy());
  if (error instanceof ApiError) expect(screen.getByText("Signed out")).toBeTruthy();
  else {
    expect(screen.getByRole("alert").textContent).toContain("Sync failed");
    expect(screen.getByRole("alert").textContent).toContain("Network interrupted");
    expect(screen.getByText("Connected")).toBeTruthy();
  }
  expect(p.onimported).toHaveBeenCalledOnce();
  if (!(error instanceof ApiError)) {
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
    expect(screen.queryByText("139 conversations processed")).toBeNull();
  }
});

it("uses the desktop account and email-code sign-in without polling Chrome", async () => {
  getBrowserHost.mockReturnValue(host);
  host.connect.mockResolvedValue(undefined);
  render(ImportModal, props());
  expect(screen.getByText("Account")).toBeTruthy();
  expect(
    screen.getByText(
      "Sign in takes an email code. For Google accounts, Sync from the web UI in Chrome.",
    ),
  ).toBeTruthy();
  expect(screen.queryByText("Connected")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(host.connect).toHaveBeenCalledOnce();
  await clickSync();
  expect(syncBrowser).toHaveBeenCalledWith(
    "claude-ai",
    host,
    expect.objectContaining({ onProgress: expect.any(Function) }),
    expect.any(AbortSignal),
  );
  expect(chromeStatus).not.toHaveBeenCalled();
});

it("shows desktop signed-out state", async () => {
  getBrowserHost.mockReturnValue(host);
  syncBrowser.mockRejectedValueOnce(new ApiError(0, "Auth required", "browser_sync_auth_required"));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  expect(screen.getByText(m.import_claude_desktop_note())).toBeTruthy();
  expect(chromeStatus).not.toHaveBeenCalled();
});

it.each([new Error("Could not open sign-in"), "Could not open sign-in"])(
  "shows desktop sign-in failure for %s",
  async (error) => {
    getBrowserHost.mockReturnValue(host);
    host.connect.mockRejectedValue(error);
    render(ImportModal, props());
    await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Sign-in failed"));
    expect(screen.getByRole("alert").textContent).toContain("Could not open sign-in");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  },
);

it.each(["read-only", "remote"])("keeps zip import alone for %s", (mode) => {
  if (mode === "read-only") vi.spyOn(syncState, "readOnly", "get").mockReturnValue(true);
  else isRemoteConnection.mockReturnValue(true);
  render(ImportModal, props());
  expect(screen.queryByRole("button", { name: "Sync" })).toBeNull();
  expect(screen.getByText(m.import_drop_here())).toBeTruthy();
  expect(chromeStatus).not.toHaveBeenCalled();
});

it.each([403, 501])(
  "keeps upload alone and stops polling after status refusal %s",
  async (status) => {
    vi.useFakeTimers();
    chromeStatus.mockRejectedValueOnce(new ApiError(status, "Chrome Sync unavailable"));
    const p = props();
    const { rerender } = render(ImportModal, p);
    await tick();
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText("Chrome")).toBeNull();
    expect(screen.queryByRole("button", { name: "Sync" })).toBeNull();
    expect(screen.getByText(m.import_drop_here())).toBeTruthy();
    await vi.advanceTimersByTimeAsync(6000);
    await fireEvent.focus(window);
    expect(chromeStatus).toHaveBeenCalledOnce();
    await rerender({ ...p, open: false });
    await rerender(p);
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText("Connected")).toBeTruthy();
    expect(chromeStatus).toHaveBeenCalledTimes(2);
  },
);

it("retries transient status failures", async () => {
  vi.useFakeTimers();
  chromeStatus.mockRejectedValueOnce(new ApiError(503, "Temporarily unavailable"));
  render(ImportModal, props());
  await tick();
  await vi.advanceTimersByTimeAsync(0);
  expect(syncButton().disabled).toBe(true);
  await vi.advanceTimersByTimeAsync(2000);
  expect(screen.getByText("Connected")).toBeTruthy();
  expect(syncButton().disabled).toBe(false);
  expect(chromeStatus).toHaveBeenCalledTimes(2);
});

it.each([new ApiError(503, "Temporarily unavailable"), new Error("Network interrupted")])(
  "keeps the last connection state through failed status polls and recovers for %s",
  async (error) => {
    vi.useFakeTimers();
    render(ImportModal, props());
    await tick();
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.getByText("Connected")).toBeTruthy();
    chromeStatus.mockRejectedValue(error);
    await vi.advanceTimersByTimeAsync(6000);
    expect(chromeStatus).toHaveBeenCalledTimes(4);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("Connected")).toBeTruthy();
    expect(syncButton().disabled).toBe(false);
    chromeStatus.mockResolvedValue({ connected: false });
    await vi.advanceTimersByTimeAsync(2000);
    expect(chromeStatus).toHaveBeenCalledTimes(5);
    expect(screen.getByText("Not connected")).toBeTruthy();
    expect(syncButton().disabled).toBe(true);
    expect(screen.queryByRole("alert")).toBeNull();
  },
);

it("localizes auth state from its code", async () => {
  void setLocale("fr", { reload: false });
  syncBrowser.mockRejectedValueOnce(
    new ApiError(0, "English auth message", "browser_sync_auth_required"),
  );
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Déconnecté")).toBeTruthy());
  expect(
    screen.getByText(
      "Connectez-vous à claude.ai dans le profil Chrome avec l'extension AgentsView. Activez l'extension dans un seul profil Chrome. La synchronisation utilise le premier profil qui se connecte.",
    ),
  ).toBeTruthy();
  expect(screen.queryByText("English auth message")).toBeNull();
});

it("polls every two seconds across provider tabs and pauses while hidden, syncing, or closed", async () => {
  vi.useFakeTimers();
  const p = props();
  const { rerender } = render(ImportModal, p);
  await tick();
  await vi.advanceTimersByTimeAsync(0);
  expect(chromeStatus).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(2000);
  expect(chromeStatus).toHaveBeenCalledTimes(2);
  const hidden = vi.spyOn(document, "hidden", "get").mockReturnValue(true);
  await fireEvent(document, new Event("visibilitychange"));
  await vi.advanceTimersByTimeAsync(6000);
  expect(chromeStatus).toHaveBeenCalledTimes(2);
  hidden.mockReturnValue(false);
  await fireEvent(document, new Event("visibilitychange"));
  expect(chromeStatus).toHaveBeenCalledTimes(3);
  await fireEvent.focus(window);
  expect(chromeStatus).toHaveBeenCalledTimes(4);
  await fireEvent.click(screen.getByRole("button", { name: "ChatGPT" }));
  await vi.advanceTimersByTimeAsync(4000);
  expect(chromeStatus).toHaveBeenCalledTimes(6);
  await fireEvent.click(screen.getByRole("button", { name: "Claude.ai" }));
  await vi.advanceTimersByTimeAsync(0);
  expect(chromeStatus).toHaveBeenCalledTimes(6);
  let reject!: (error: Error) => void;
  syncBrowser.mockImplementation(
    () =>
      new Promise((_, r) => {
        reject = r;
      }),
  );
  await fireEvent.click(syncButton());
  await vi.advanceTimersByTimeAsync(4000);
  expect(chromeStatus).toHaveBeenCalledTimes(6);
  await fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  reject(new Error("Stopped"));
  await tick();
  await vi.advanceTimersByTimeAsync(0);
  expect(chromeStatus).toHaveBeenCalledTimes(7);
  await rerender({ ...p, open: false });
  await vi.advanceTimersByTimeAsync(4000);
  expect(chromeStatus).toHaveBeenCalledTimes(7);
});

it("cancels stale reads and clears auth on close and reopen", async () => {
  const p = props();
  const { rerender } = render(ImportModal, p);
  syncBrowser.mockRejectedValueOnce(new ApiError(0, "Auth", "browser_sync_auth_required"));
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockImplementationOnce(
    () =>
      new Promise((r) => {
        resolve = r;
      }),
  );
  await fireEvent.focus(window);
  const signal = chromeStatus.mock.calls.at(-1)![0].signal;
  await rerender({ ...p, open: false });
  expect(signal.aborted).toBe(true);
  resolve({ ...ready, connected: false });
  await tick();
  await rerender(p);
  await waitFor(() => expect(screen.getByText("Connected")).toBeTruthy());
  expect(screen.queryByText("Signed out")).toBeNull();
});
