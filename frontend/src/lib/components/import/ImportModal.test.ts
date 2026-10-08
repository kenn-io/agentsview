// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), disconnect: vi.fn(), close: vi.fn(), fetch: vi.fn() }));
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost: () => host }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...(await original<typeof import("../../api/runtime.js")>()),
  isRemoteConnection: () => false,
}));
const syncState = vi.hoisted(() => ({ readOnly: false }));
vi.mock("../../stores/sync.svelte.js", () => ({ sync: syncState }));
const syncClaudeAI = vi.hoisted(() => vi.fn());
vi.mock("../../api/client.js", () => ({
  syncClaudeAI,
  importClaudeAI: vi.fn(),
  importChatGPT: vi.fn(),
}));
afterEach(() => { vi.resetAllMocks(); syncState.readOnly = false; });

it("offers sign in, sync and disconnect without a sign-in probe", async () => {
  host.connect.mockResolvedValue(undefined);
  host.disconnect.mockResolvedValue(undefined);
  syncClaudeAI.mockImplementation(async (_host, callbacks) => {
    const stats = { imported: 1, updated: 0, skipped: 0, errors: 0 };
    callbacks.onProgress(stats);
    return stats;
  });
  const onimported = vi.fn();
  render(ImportModal, { open: true, onclose: vi.fn(), onimported });
  expect(screen.getByText(m.import_claude_help())).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  expect(host.connect).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_disconnect() }));
  expect(host.disconnect).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  expect(syncClaudeAI).toHaveBeenCalledWith(
    host,
    expect.objectContaining({ onProgress: expect.any(Function) }),
    expect.any(AbortSignal),
  );
  expect(onimported).toHaveBeenCalledOnce();
  expect(screen.getByText(m.import_processed({ count: 1 }))).toBeTruthy();
});

it.each(["cancel", "disconnect", "error"])("refreshes completed chats after sync %s", async (exit) => {
  let rejectSync!: (error: Error) => void;
  let signal!: AbortSignal;
  syncClaudeAI.mockImplementation((_host, callbacks, runSignal) => {
    signal = runSignal;
    callbacks.onProgress({ imported: 0, updated: 1, skipped: 0, errors: 0 });
    return new Promise((_, reject) => { rejectSync = reject; });
  });
  const onimported = vi.fn();
  const onclose = vi.fn();
  const { rerender } = render(ImportModal, { open: true, onclose, onimported });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  if (exit !== "error") {
    await fireEvent.click(screen.getByRole("button", { name: exit === "cancel" ? m.import_cancel() : m.import_claude_disconnect() }));
    expect(signal.aborted).toBe(true);
  }
  rejectSync(new Error("Interrupted sync"));
  if (exit === "cancel") {
    expect(onclose).toHaveBeenCalledOnce();
    await rerender({ open: true, onclose, onimported });
    await waitFor(() => expect((screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByText("Interrupted sync")).toBeNull();
  }
  await waitFor(() => expect(onimported).toHaveBeenCalledOnce());
});

it("hides browser sync controls for a read-only archive", () => {
  syncState.readOnly = true;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.queryByRole("button", { name: m.import_claude_sync() })).toBeNull();
  expect(screen.queryByRole("button", { name: m.import_claude_connect() })).toBeNull();
  expect(screen.queryByRole("button", { name: m.import_claude_disconnect() })).toBeNull();
});

it("disconnect waits for sync to close the browser before deleting cookies", async () => {
  let finishClose!: () => void;
  let signal!: AbortSignal;
  host.close.mockImplementation(() => new Promise<void>((resolve) => { finishClose = resolve; }));
  host.disconnect.mockResolvedValue(undefined);
  syncClaudeAI.mockImplementation(async (_host, _callbacks, runSignal) => {
    signal = runSignal;
    try {
      await new Promise((_, reject) => {
        runSignal.addEventListener("abort", () => reject(new Error("Cancelled")), { once: true });
      });
    } finally {
      await host.close();
    }
  });
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_disconnect() }));
  expect(signal.aborted).toBe(true);
  expect(host.close).toHaveBeenCalledOnce();
  expect(host.disconnect).not.toHaveBeenCalled();
  finishClose();
  await waitFor(() => expect(host.disconnect).toHaveBeenCalledOnce());
  expect(screen.queryByText("Cancelled")).toBeNull();
});
