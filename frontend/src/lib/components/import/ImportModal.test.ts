// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";
import { setLocale } from "../../paraglide/runtime.js";
import { ApiError } from "../../api/runtime.js";

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
afterEach(() => { vi.resetAllMocks(); syncState.readOnly = false; setLocale("en", { reload: false }); });

it.each([
  ["claude_ai_auth_required", "Sign in to Claude.ai, then Sync again", "Connectez-vous à Claude.ai, puis relancez la synchronisation."],
  ["claude_ai_sign_in_pending", "Claude sign-in is still pending", "Terminez la connexion à Claude.ai, fermez la fenêtre, puis relancez la synchronisation."],
  ["claude_ai_archive_upgrade_required", "Let the archive finish upgrading, then Sync again.", "Attendez la fin de la mise à niveau de l'archive, puis relancez la synchronisation."],
])("shows localized recovery for %s", async (code, message, expected) => {
  setLocale("fr", { reload: false });
  syncClaudeAI.mockRejectedValue(new ApiError(0, message, code));
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  await waitFor(() => expect(screen.getByText(expected)).toBeTruthy());
  expect(screen.queryByText(message)).toBeNull();
});

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
  let finishClose!: () => void;
  host.close.mockImplementation(() => new Promise<void>((resolve) => { finishClose = resolve; }));
  host.disconnect.mockResolvedValue(undefined);
  let signal!: AbortSignal;
  syncClaudeAI.mockImplementation(async (_host, callbacks, runSignal) => {
    signal = runSignal;
    callbacks.onProgress({ imported: 0, updated: 1, skipped: 0, errors: 0 });
    try {
      return await new Promise((_, reject) => {
        rejectSync = reject;
        if (exit === "disconnect") {
          runSignal.addEventListener("abort", () => reject(new Error("Cancelled")), { once: true });
        }
      });
    } finally {
      if (exit === "disconnect") await host.close();
    }
  });
  const onimported = vi.fn();
  const onclose = vi.fn();
  const { rerender } = render(ImportModal, { open: true, onclose, onimported });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  if (exit !== "error") {
    await fireEvent.click(screen.getByRole("button", { name: exit === "cancel" ? m.import_cancel() : m.import_claude_disconnect() }));
    expect(signal.aborted).toBe(true);
  }
  if (exit === "disconnect") {
    expect(host.close).toHaveBeenCalledOnce();
    expect(host.disconnect).not.toHaveBeenCalled();
    finishClose();
    await waitFor(() => expect(host.disconnect).toHaveBeenCalledOnce());
    expect(screen.queryByText("Cancelled")).toBeNull();
  } else {
    rejectSync(new Error("Interrupted sync"));
  }
  if (exit === "cancel") {
    expect(onclose).toHaveBeenCalledOnce();
    await rerender({ open: true, onclose, onimported });
    await waitFor(() => expect((screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByText("Interrupted sync")).toBeNull();
  }
  await waitFor(() => expect(onimported).toHaveBeenCalledOnce());
});

it("keeps the dialog cleared after cancelling completed sync during browser cleanup", async () => {
  let finishClose!: () => void;
  host.close.mockImplementation(() => new Promise<void>((resolve) => { finishClose = resolve; }));
  syncClaudeAI.mockImplementation(async (_host, callbacks) => {
    const stats = { imported: 1, updated: 0, skipped: 0, errors: 0 };
    callbacks.onProgress(stats);
    await host.close();
    return stats;
  });
  const onimported = vi.fn();
  const onclose = vi.fn();
  const { rerender } = render(ImportModal, { open: true, onclose, onimported });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  await waitFor(() => expect(host.close).toHaveBeenCalledOnce());
  await fireEvent.click(screen.getByRole("button", { name: m.import_cancel() }));
  expect(onclose).toHaveBeenCalledOnce();
  expect(syncClaudeAI.mock.calls[0]?.[2]?.aborted).toBe(true);
  await rerender({ open: false, onclose, onimported });
  finishClose();
  await waitFor(() => expect(onimported).toHaveBeenCalledOnce());
  await rerender({ open: true, onclose, onimported });
  expect(screen.queryByText(m.import_processed({ count: 1 }))).toBeNull();
  expect((screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement).disabled).toBe(false);
});

it("hides browser sync controls for a read-only archive", () => {
  syncState.readOnly = true;
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  expect(screen.queryByRole("button", { name: m.import_claude_sync() })).toBeNull();
  expect(screen.queryByRole("button", { name: m.import_claude_connect() })).toBeNull();
  expect(screen.queryByRole("button", { name: m.import_claude_disconnect() })).toBeNull();
});

it.each(["success", "error"])("blocks browser actions while disconnect is pending, then recovers after %s", async (outcome) => {
  let resolveDisconnect!: () => void;
  let rejectDisconnect!: (error: Error) => void;
  host.disconnect.mockImplementation(() => new Promise<void>((resolve, reject) => {
    resolveDisconnect = resolve;
    rejectDisconnect = reject;
  }));
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  const signIn = screen.getByRole("button", { name: m.import_claude_connect() }) as HTMLButtonElement;
  const sync = screen.getByRole("button", { name: m.import_claude_sync() }) as HTMLButtonElement;
  const disconnect = screen.getByRole("button", { name: m.import_claude_disconnect() }) as HTMLButtonElement;
  await fireEvent.click(disconnect);
  await waitFor(() => expect(host.disconnect).toHaveBeenCalledOnce());
  await fireEvent.click(sync);
  expect(syncClaudeAI).not.toHaveBeenCalled();
  await fireEvent.click(signIn);
  await fireEvent.click(disconnect);
  expect(host.connect).not.toHaveBeenCalled();
  expect(host.disconnect).toHaveBeenCalledOnce();
  expect(signIn.disabled).toBe(true);
  expect(sync.disabled).toBe(true);
  expect(disconnect.disabled).toBe(true);
  if (outcome === "success") resolveDisconnect();
  else rejectDisconnect(new Error("Disconnect failed"));
  await waitFor(() => expect(sync.disabled).toBe(false));
  expect(signIn.disabled).toBe(false);
  expect(disconnect.disabled).toBe(false);
  if (outcome === "error") expect(screen.getByText("Error: Disconnect failed")).toBeTruthy();
});

it("shows localized recovery when an archived session requires a newer AgentsView version", async () => {
  setLocale("fr", { reload: false });
  syncClaudeAI.mockResolvedValue({
    imported: 0, updated: 0, skipped: 0, errors: 1,
    refusals: [{ session_id: "claude-ai:chat", reason: "newer_marker" }],
  });
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_sync() }));
  await waitFor(() => expect(screen.getByRole("alert").textContent?.trim()).toBe(
    "La session archivée claude-ai:chat nécessite une version plus récente d'AgentsView. Mettez AgentsView à jour, puis relancez la synchronisation.",
  ));
});
