// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { fireEvent, render, screen } from "@testing-library/svelte";
import { flushSync } from "svelte";
import ImportModal from "./ImportModal.svelte";
import { m } from "../../i18n/index.js";

const host = vi.hoisted(() => ({ connect: vi.fn(), status: vi.fn(), disconnect: vi.fn(), fetch: vi.fn() }));
vi.mock("../../api/browserHost.js", () => ({ getBrowserHost: () => host }));
vi.mock("../../api/runtime.js", async (original) => ({
  ...await original<typeof import("../../api/runtime.js")>(), isRemoteConnection: () => false,
}));

afterEach(() => { vi.useRealTimers(); vi.resetAllMocks(); });

it("lets the user connect again after closing the sign-in window", async () => {
  vi.useFakeTimers();
  host.connect.mockResolvedValue(undefined);
  host.status.mockResolvedValue({ connected: false, pending: false, organization: null });
  render(ImportModal, { open: true, onclose: vi.fn(), onimported: vi.fn() });
  await Promise.resolve();
  flushSync();
  host.status.mockResolvedValue({ connected: false, pending: true, organization: null });
  await fireEvent.click(screen.getByRole("button", { name: m.import_claude_connect() }));
  await vi.advanceTimersByTimeAsync(1000);
  flushSync();
  expect((screen.getByRole("button", { name: m.import_claude_connecting() }) as HTMLButtonElement).disabled).toBe(true);
  host.status.mockResolvedValue({ connected: false, pending: false, organization: null });
  await vi.advanceTimersByTimeAsync(1000);
  flushSync();
  const connect = screen.getByRole("button", { name: m.import_claude_connect() });
  expect((connect as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(connect);
  expect(host.connect).toHaveBeenCalledTimes(2);
});
