// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen } from "@testing-library/svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), disconnect: vi.fn(), fetch: vi.fn() }));
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost: () => host }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...(await original<typeof import("../../api/runtime.js")>()),
  isRemoteConnection: () => false,
}));
const syncClaudeAI = vi.hoisted(() => vi.fn());
vi.mock("../../api/client.js", () => ({
  syncClaudeAI,
  importClaudeAI: vi.fn(),
  importChatGPT: vi.fn(),
}));
afterEach(() => vi.resetAllMocks());

it("offers sign in, sync and disconnect without a sign-in probe", async () => {
  host.connect.mockResolvedValue(undefined);
  host.disconnect.mockResolvedValue(undefined);
  syncClaudeAI.mockResolvedValue({ imported: 1, updated: 0, skipped: 0, errors: 0 });
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
