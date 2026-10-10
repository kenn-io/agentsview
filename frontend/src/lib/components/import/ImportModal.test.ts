// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";
import { setLocale } from "../../paraglide/runtime.js";
import { ApiError } from "../../api/runtime.js";
import { sync as syncState } from "../../stores/sync.svelte.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), close: vi.fn(), fetch: vi.fn() }));
const getBrowserHost = vi.hoisted(() => vi.fn());
const isRemoteConnection = vi.hoisted(() => vi.fn());
const chromeStatus = vi.hoisted(() => vi.fn());
const syncClaudeAI = vi.hoisted(() => vi.fn());
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...(await original<typeof import("../../api/runtime.js")>()), isRemoteConnection,
}));
vi.mock("../../api/generated/index.js", async (original) => ({
  ...(await original<typeof import("../../api/generated/index.js")>()),
  ImportService: { getApiV1ImportClaudeAiChrome: chromeStatus },
}));
vi.mock("../../api/client.js", () => ({
  connectClaudeAI: (browser: typeof host) => browser.connect(), syncClaudeAI,
  importClaudeAI: vi.fn(), importChatGPT: vi.fn(),
}));
const ready = { connected: true };
const stats = { imported: 12, updated: 3, skipped: 297, errors: 0 };
const props = () => ({ open: true, onclose: vi.fn(), onimported: vi.fn() });
const syncButton = () => screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement;
async function clickSync() {
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  await fireEvent.click(syncButton());
}
beforeEach(() => {
  vi.spyOn(syncState, "readOnly", "get").mockReturnValue(false);
  isRemoteConnection.mockReturnValue(false);
  getBrowserHost.mockReturnValue(undefined);
  chromeStatus.mockResolvedValue(ready);
  syncClaudeAI.mockResolvedValue(stats);
  setLocale("en", { reload: false });
});
afterEach(() => {
  vi.useRealTimers(); vi.resetAllMocks(); vi.restoreAllMocks(); setLocale("en", { reload: false });
});

it("disables Sync while checking, then shows the live connection", async () => {
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockReturnValue(new Promise((r) => { resolve = r; }));
  render(ImportModal, props());
  expect(syncButton().disabled).toBe(true);
  expect(screen.queryByText("Connected")).toBeNull();
  resolve(ready);
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  expect(screen.getByText("Connected")).toBeTruthy();
  expect(screen.getByText("Sync opens a background claude.ai tab if none is open. The tab stays open after Sync.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
});

it("shows setup instructions when disconnected", async () => {
  chromeStatus.mockResolvedValue({ connected: false });
  render(ImportModal, props());
  await waitFor(() => expect(screen.getByText("Not connected")).toBeTruthy());
  expect(syncButton().disabled).toBe(true);
  expect(screen.getByText("Open Chrome to connect.")).toBeTruthy();
  expect(screen.getByText("Terminal")).toBeTruthy();
  expect(screen.getByText("agentsview chrome setup")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Copy command" })).toBeTruthy();
  expect(screen.getByText(m.import_claude_setup_load({ url: "chrome://extensions" }))).toBeTruthy();
});

it.each(["stop", "cancel"])("shows progress and refreshes completed chats after %s", async (exit) => {
  let reject!: (error: Error) => void;
  let signal!: AbortSignal;
  let progress!: (value: typeof stats) => void;
  syncClaudeAI.mockImplementation((_host, callbacks, runSignal) => {
    signal = runSignal; progress = callbacks.onProgress;
    return new Promise((_, r) => { reject = r; });
  });
  const p = props();
  render(ImportModal, p);
  await clickSync();
  expect(screen.getByText("Syncing...")).toBeTruthy();
  expect(screen.queryByText(m.import_drop_here())).toBeNull();
  progress({ imported: 128, updated: 0, skipped: 0, errors: 0 });
  await tick();
  expect(screen.getByText("128 conversations processed...")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: exit === "stop" ? "Stop" : "Cancel" }));
  expect(signal.aborted).toBe(true);
  expect(p.onclose.mock.calls.length).toBe(exit === "stop" ? 0 : 1);
  reject(new Error("Stopped"));
  await waitFor(() => expect(p.onimported).toHaveBeenCalledOnce());
  if (exit === "stop") expect(screen.getByRole("dialog")).toBeTruthy();
  expect(screen.queryByText("Stopped")).toBeNull();
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

it("offers browser sign-in after auth failure", async () => {
  syncClaudeAI.mockRejectedValueOnce(new ApiError(0, "Server auth text", "claude_ai_auth_required"));
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  expect(screen.getByText("Sign in to claude.ai in the Chrome profile with the AgentsView extension. Enable the extension in only one Chrome profile. Sync uses the first profile that connects.")).toBeTruthy();
  expect(screen.queryByText("Server auth text")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Sign in to claude.ai in the Chrome profile with the AgentsView extension. Enable the extension in only one Chrome profile. Sync uses the first profile that connects." }));
  expect(open).toHaveBeenCalledExactlyOnceWith("https://claude.ai/login?return_url=%2Fnew", "_blank", "noopener,noreferrer");
});

it.each(["claude_ai_auth_required", "claude_ai_chrome_host_update_required"])("recovers from %s without a reload", async (code) => {
  syncClaudeAI.mockRejectedValueOnce(new ApiError(0, "Server error text", code));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText(code === "claude_ai_auth_required" ? "Signed out" : "Update needed")).toBeTruthy());
  if (code === "claude_ai_chrome_host_update_required") {
    chromeStatus.mockResolvedValue({ connected: false });
    await fireEvent.focus(window);
    await tick();
    expect(screen.getByText("Restart AgentsView if you upgraded it.")).toBeTruthy();
    expect(screen.getByText("agentsview chrome setup")).toBeTruthy();
  }
  expect(syncButton().disabled).toBe(false);
  await clickSync();
  await waitFor(() => expect(screen.getByText("312 conversations processed")).toBeTruthy());
  expect(screen.queryByText("Signed out")).toBeNull();
  expect(screen.queryByText("Update needed")).toBeNull();
});

it("shows an overlapping Chrome Sync refusal", async () => {
  syncClaudeAI.mockRejectedValueOnce(new ApiError(409, "Server conflict text", "claude_ai_sync_running"));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Chrome Sync is already running."));
  expect(screen.queryByText("Server conflict text")).toBeNull();
  expect(syncButton().disabled).toBe(false);
});

it("clears a Sync conflict after closing while status hangs", async () => {
  syncClaudeAI.mockRejectedValueOnce(new ApiError(409, "Server conflict text", "claude_ai_sync_running"));
  const p = props();
  const view = render(ImportModal, p);
  await waitFor(() => expect(syncButton().disabled).toBe(false));
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockReturnValue(new Promise((r) => { resolve = r; }));
  await fireEvent.click(syncButton());
  await waitFor(() => expect(chromeStatus).toHaveBeenCalledTimes(2));
  await view.rerender({ ...p, open: false });
  await view.rerender({ ...p, open: true });
  resolve(ready);
  await waitFor(() => expect(p.onimported).toHaveBeenCalledOnce());
  await tick();
  expect(screen.queryByText("Chrome Sync is already running.")).toBeNull();
});

it.each([new Error("Network interrupted"), new ApiError(0, "Auth required", "claude_ai_auth_required")])("keeps processed counts after a partial Sync fails with %s", async (error) => {
  syncClaudeAI.mockImplementationOnce(async (_host, callbacks) => {
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
  expect(screen.getByText("Sign in takes an email code. For Google accounts, Sync from the web UI in Chrome.")).toBeTruthy();
  expect(screen.queryByText("Connected")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(host.connect).toHaveBeenCalledOnce();
  await clickSync();
  expect(syncClaudeAI).toHaveBeenCalledWith(host, expect.objectContaining({ onProgress: expect.any(Function) }), expect.any(AbortSignal));
  expect(chromeStatus).not.toHaveBeenCalled();
});

it("shows desktop signed-out state", async () => {
  getBrowserHost.mockReturnValue(host);
  syncClaudeAI.mockRejectedValueOnce(new ApiError(0, "Auth required", "claude_ai_auth_required"));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  expect(screen.getByText(m.import_claude_desktop_note())).toBeTruthy();
  expect(chromeStatus).not.toHaveBeenCalled();
});

it.each([new Error("Could not open sign-in"), "Could not open sign-in"])("shows desktop sign-in failure for %s", async (error) => {
  getBrowserHost.mockReturnValue(host);
  host.connect.mockRejectedValue(error);
  render(ImportModal, props());
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Sign-in failed"));
  expect(screen.getByRole("alert").textContent).toContain("Could not open sign-in");
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
});

it.each(["read-only", "remote"])("keeps zip import alone for %s", (mode) => {
  if (mode === "read-only") vi.spyOn(syncState, "readOnly", "get").mockReturnValue(true);
  else isRemoteConnection.mockReturnValue(true);
  render(ImportModal, props());
  expect(screen.queryByRole("button", { name: "Sync" })).toBeNull();
  expect(screen.getByText(m.import_drop_here())).toBeTruthy();
  expect(chromeStatus).not.toHaveBeenCalled();
});

it.each([403, 501])("keeps upload alone and stops polling after status refusal %s", async (status) => {
  vi.useFakeTimers();
  chromeStatus.mockRejectedValueOnce(new ApiError(status, "Chrome Sync unavailable"));
  const p = props();
  const { rerender } = render(ImportModal, p);
  await tick(); await vi.advanceTimersByTimeAsync(0);
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
});

it("retries transient status failures", async () => {
  vi.useFakeTimers();
  chromeStatus.mockRejectedValueOnce(new ApiError(503, "Temporarily unavailable"));
  render(ImportModal, props());
  await tick(); await vi.advanceTimersByTimeAsync(0);
  expect(syncButton().disabled).toBe(true);
  await vi.advanceTimersByTimeAsync(2000);
  expect(screen.getByText("Connected")).toBeTruthy();
  expect(syncButton().disabled).toBe(false);
  expect(chromeStatus).toHaveBeenCalledTimes(2);
});

it.each([new ApiError(503, "Temporarily unavailable"), new Error("Network interrupted")])("keeps the last connection state through failed status polls and recovers for %s", async (error) => {
  vi.useFakeTimers();
  render(ImportModal, props());
  await tick(); await vi.advanceTimersByTimeAsync(0);
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
});

it("localizes auth state from its code", async () => {
  setLocale("fr", { reload: false });
  syncClaudeAI.mockRejectedValueOnce(new ApiError(0, "English auth message", "claude_ai_auth_required"));
  render(ImportModal, props());
  await clickSync();
  await waitFor(() => expect(screen.getByText("Déconnecté")).toBeTruthy());
  expect(screen.getByText("Connectez-vous à claude.ai dans le profil Chrome avec l'extension AgentsView. Activez l'extension dans un seul profil Chrome. La synchronisation utilise le premier profil qui se connecte.")).toBeTruthy();
  expect(screen.queryByText("English auth message")).toBeNull();
});

it("polls every two seconds and pauses on hidden page, sync, tab change and close", async () => {
  vi.useFakeTimers();
  const p = props();
  const { rerender } = render(ImportModal, p);
  await tick(); await vi.advanceTimersByTimeAsync(0);
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
  expect(chromeStatus).toHaveBeenCalledTimes(4);
  await fireEvent.click(screen.getByRole("button", { name: "Claude.ai" }));
  expect(chromeStatus).toHaveBeenCalledTimes(5);
  let reject!: (error: Error) => void;
  syncClaudeAI.mockImplementation(() => new Promise((_, r) => { reject = r; }));
  await fireEvent.click(syncButton());
  await vi.advanceTimersByTimeAsync(4000);
  expect(chromeStatus).toHaveBeenCalledTimes(5);
  await fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  reject(new Error("Stopped"));
  await tick(); await vi.advanceTimersByTimeAsync(0);
  expect(chromeStatus).toHaveBeenCalledTimes(6);
  await rerender({ ...p, open: false });
  await vi.advanceTimersByTimeAsync(4000);
  expect(chromeStatus).toHaveBeenCalledTimes(6);
});

it("cancels stale reads and clears auth on close and reopen", async () => {
  const p = props();
  const { rerender } = render(ImportModal, p);
  syncClaudeAI.mockRejectedValueOnce(new ApiError(0, "Auth", "claude_ai_auth_required"));
  await clickSync();
  await waitFor(() => expect(screen.getByText("Signed out")).toBeTruthy());
  let resolve!: (status: typeof ready) => void;
  chromeStatus.mockImplementationOnce(() => new Promise((r) => { resolve = r; }));
  await fireEvent.focus(window);
  const signal = chromeStatus.mock.calls.at(-1)![0].signal;
  await rerender({ ...p, open: false });
  expect(signal.aborted).toBe(true);
  resolve({ ...ready, connected: false });
  await tick(); await rerender(p);
  await waitFor(() => expect(screen.getByText("Connected")).toBeTruthy());
  expect(screen.queryByText("Signed out")).toBeNull();
});
