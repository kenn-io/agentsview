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
const syncState = vi.hoisted(() => ({ readOnly: false, isDesktop: false, serverVersion: { claude_ai_chrome_host: false } }));
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
afterEach(() => { vi.resetAllMocks(); vi.restoreAllMocks(); syncState.readOnly = false; syncState.isDesktop = false; syncState.serverVersion.claude_ai_chrome_host = false; setLocale("en", { reload: false }); });

it("shows Chrome Sync from the server flag without a page host", async () => {
  getBrowserHost.mockReturnValue(undefined);
  syncState.serverVersion.claude_ai_chrome_host = true;
  syncClaudeAI.mockResolvedValue({ imported: 1, updated: 0, skipped: 0, errors: 0 });
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.getByText(m.import_claude_help_chrome())).toBeTruthy();
  expect(screen.getByRole("link", { name: m.import_claude_connect() }).getAttribute("href")).toBe("https://claude.ai/login?return_url=%2Fnew");
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  expect(syncClaudeAI).toHaveBeenCalledWith(undefined, expect.objectContaining({ onProgress: expect.any(Function) }), expect.any(AbortSignal));
});

it("shows desktop email-code instructions with a Chrome route for Google sign-in", () => {
  syncState.isDesktop = true;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.getByText("Sign in with an email code, close the window, then Sync. For Google sign-in, use AgentsView in Chrome with the extension.")).toBeTruthy();
  expect(screen.queryByText(/browser host/)).toBeNull();
});

it("shows the sign-in error message", async () => {
  host.connect.mockRejectedValue(new Error("Could not open the Claude.ai sign-in window."));
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  await waitFor(() => expect(screen.getByText("Could not open the Claude.ai sign-in window.")).toBeTruthy());
});

it("shows a desktop sign-in error rejected as a string", async () => {
  syncState.isDesktop = true;
  host.connect.mockRejectedValue("Could not open the Claude.ai sign-in window.");
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  await waitFor(() => expect(screen.getByText("Could not open the Claude.ai sign-in window.")).toBeTruthy());
});

it.each([false, true])("shows setup when the Chrome host is absent, readOnly=%s", (readOnly) => {
  getBrowserHost.mockReturnValue(undefined);
  syncState.readOnly = readOnly;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  const hint = screen.queryByText(m.import_claude_chrome_setup());
  if (readOnly) expect(hint).toBeNull();
  else expect(hint?.textContent).toContain("agentsview chrome setup");
  expect(screen.queryByRole("button", { name: m.import_claude_sync() })).toBeNull();
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
