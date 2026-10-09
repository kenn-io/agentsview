// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";
import { setLocale } from "../../paraglide/runtime.js";
import { ApiError } from "../../api/runtime.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), close: vi.fn(), fetch: vi.fn() }));
const getBrowserHost = vi.hoisted(() => vi.fn());
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...(await original<typeof import("../../api/runtime.js")>()),
  isRemoteConnection: () => false,
}));
const syncState = vi.hoisted(() => ({ readOnly: false, isDesktop: false }));
vi.mock("../../stores/sync.svelte.js", () => ({ sync: syncState }));
const syncClaudeAI = vi.hoisted(() => vi.fn());
vi.mock("../../api/client.js", () => ({
  connectClaudeAI: (browser: typeof host) => browser.connect(),
  syncClaudeAI,
  importClaudeAI: vi.fn(),
  importChatGPT: vi.fn(),
}));
beforeEach(() => {
  getBrowserHost.mockReturnValue(host);
});
afterEach(() => { vi.resetAllMocks(); vi.restoreAllMocks(); syncState.readOnly = false; syncState.isDesktop = false; setLocale("en", { reload: false }); });

it("shows Chrome toolbar instructions with Sync", () => {
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.getByText("Click the AgentsView toolbar button on this tab, then Sync. Sync uses Chrome's Claude.ai sign-in.")).toBeTruthy();
  expect(screen.getByRole("button", { name: m.import_claude_sync() })).toBeTruthy();
});

it("shows desktop email-code instructions with a Chrome route for Google sign-in", () => {
  syncState.isDesktop = true;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.getByText("Sign in with an email code, close the window, then Sync. For Google sign-in, use AgentsView in Chrome with the extension.")).toBeTruthy();
  expect(screen.queryByText(/browser host/)).toBeNull();
});

it("shows the sign-in error message", async () => {
  host.connect.mockRejectedValue(new Error("Click the AgentsView toolbar button on this tab to allow Claude.ai Sync."));
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  await waitFor(() => expect(screen.getByText("Click the AgentsView toolbar button on this tab to allow Claude.ai Sync.")).toBeTruthy());
});

it("shows a desktop sign-in error rejected as a string", async () => {
  syncState.isDesktop = true;
  host.connect.mockRejectedValue("Could not open the Claude.ai sign-in window.");
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  await waitFor(() => expect(screen.getByText("Could not open the Claude.ai sign-in window.")).toBeTruthy());
});

it.each([false, true])("offers the extension download in writable Chrome only, readOnly=%s", (readOnly) => {
  getBrowserHost.mockReturnValue(undefined);
  syncState.readOnly = readOnly;
  vi.spyOn(navigator, "userAgent", "get").mockReturnValue("Mozilla/5.0 Chrome/140.0.0.0");
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  const link = screen.queryByRole("link", { name: "Download Chrome extension" });
  if (readOnly) expect(link).toBeNull();
  else {
    expect(link?.getAttribute("href")).toBe("/chrome-extension.zip");
    expect(screen.getByText(/Unzip the download, open chrome:\/\/extensions/)).toBeTruthy();
  }
});

it.each([
  ["claude_ai_auth_required", "Sign in to Claude.ai, then Sync again", "Connectez-vous à Claude.ai, puis relancez la synchronisation."],
])("shows localized recovery for %s", async (code, message, expected) => {
  setLocale("fr", { reload: false });
  syncClaudeAI.mockRejectedValue(new ApiError(0, message, code));
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  await waitFor(() => expect(screen.getByText(expected)).toBeTruthy());
  expect(screen.queryByText(message)).toBeNull();
});

it("offers sign in and sync without a sign-in probe", async () => {
  syncState.isDesktop = true;
  host.connect.mockResolvedValue(undefined);
  syncClaudeAI.mockImplementation(async (_host, callbacks) => {
    const stats = { imported: 1, updated: 0, skipped: 0, errors: 0 };
    callbacks.onProgress(stats);
    return stats;
  });
  const onimported = vi.fn();
  render(ImportModal, { open: true, onclose: vi.fn(), onimported });
  expect(screen.getByText(m.import_claude_help())).toBeTruthy();
  expect(screen.getByText(/email code/)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  expect(host.connect).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  expect(syncClaudeAI).toHaveBeenCalledWith(
    host,
    expect.objectContaining({ onProgress: expect.any(Function) }),
    expect.any(AbortSignal),
  );
  expect(onimported).toHaveBeenCalledOnce();
  expect(screen.getByText(m.import_processed({ count: 1 }))).toBeTruthy();
});

it.each(["cancel", "error"])("refreshes completed chats after sync %s", async (exit) => {
  let rejectSync!: (error: Error) => void;
  let signal!: AbortSignal;
  syncClaudeAI.mockImplementation(async (_host, callbacks, runSignal) => {
    signal = runSignal;
    callbacks.onProgress({ imported: 0, updated: 1, skipped: 0, errors: 0 });
    return await new Promise((_, reject) => { rejectSync = reject; });
  });
  const onimported = vi.fn();
  const onclose = vi.fn();
  const { rerender } = render(ImportModal, { open: true, onclose, onimported });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  if (exit === "cancel") {
    await fireEvent.click(screen.getByRole("button", { name: m.import_cancel() }));
    expect(signal.aborted).toBe(true);
    expect(onclose).toHaveBeenCalledOnce();
  }
  rejectSync(new Error("Interrupted sync"));
  await waitFor(() => expect(onimported).toHaveBeenCalledOnce());
  if (exit === "cancel") {
    await rerender({ open: true, onclose, onimported });
    expect((screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByText("Interrupted sync")).toBeNull();
  }
});

it("hides browser sync controls for a read-only archive", () => {
  syncState.readOnly = true;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.queryByRole("button", { name: m.import_claude_sync() })).toBeNull();
  expect(screen.queryByRole("button", { name: m.import_claude_connect() })).toBeNull();
});
