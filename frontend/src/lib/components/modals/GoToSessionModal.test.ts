// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { ui } from "../../stores/ui.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { router } from "../../stores/router.svelte.js";
// @ts-ignore
import GoToSessionModal from "./GoToSessionModal.svelte";

const UUID = "123e4567-e89b-12d3-a456-426614174000";

const harness = vi.hoisted(() => ({
  resolveSessionIds: vi.fn(),
  sessions: { navigateToSession: vi.fn() },
  router: { navigateToSession: vi.fn() },
}));

vi.mock("../../api/generated/index", () => ({
  SessionsService: {
    getApiV1SessionIdsResolve: harness.resolveSessionIds,
  },
}));

vi.mock("../../api/runtime.js", () => ({

  isAbortError: vi.fn(
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  ),
}));

vi.mock("../../stores/sessions.svelte.js", () => ({ sessions: harness.sessions }));
vi.mock("../../stores/router.svelte.js", () => ({ router: harness.router }));

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

function input(): HTMLInputElement {
  return document.querySelector<HTMLInputElement>("#go-to-session-input")!;
}

async function setInput(value: string): Promise<void> {
  const control = input();
  control.value = value;
  control.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();
}

async function pressKey(key: string, options: KeyboardEventInit = {}): Promise<KeyboardEvent> {
  const event = new KeyboardEvent("keydown", {
    key,
    bubbles: true,
    cancelable: true,
    ...options,
  });
  input().dispatchEvent(event);
  await settle();
  return event;
}

describe("GoToSessionModal", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    document.body.innerHTML = "";
    ui.activeModal = "goToSession";
    harness.resolveSessionIds.mockReset();
    harness.resolveSessionIds.mockResolvedValue({ ids: [] });
    harness.sessions.navigateToSession.mockReset();
    harness.sessions.navigateToSession.mockResolvedValue(undefined);
    harness.router.navigateToSession.mockReset();
    ui.activeModal = "goToSession";
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    ui.activeModal = null;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  async function open(): Promise<void> {
    component = mount(GoToSessionModal, { target: document.body });
    await settle();
  }

  it("focuses the UUID input on mount", async () => {
    await open();

    expect(input()).toBe(document.activeElement);
    expect(input().getAttribute("placeholder")).toBe("Paste a session ID or UUID");
  });

  it("resolves one canonical ID and sends it to both navigation owners", async () => {
    await open();
    const canonical = `remote-host~codex:${UUID}`;
    harness.resolveSessionIds.mockResolvedValue({ ids: [canonical] });
    await setInput(UUID);

    const event = await pressKey("Enter");

    expect(event.defaultPrevented).toBe(true);
    expect(harness.resolveSessionIds).toHaveBeenCalledExactlyOnceWith(
      { partial: UUID, limit: 1000 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(harness.sessions.navigateToSession).toHaveBeenCalledExactlyOnceWith(canonical);
    expect(harness.router.navigateToSession).toHaveBeenCalledExactlyOnceWith(canonical);
    expect(ui.activeModal).toBeNull();
  });

  it("normalizes uppercase UUID input before lookup", async () => {
    await open();
    const canonical = `codex:${UUID}`;
    harness.resolveSessionIds.mockResolvedValue({ ids: [canonical] });
    await setInput(UUID.toUpperCase());

    await pressKey("Enter");

    expect(harness.resolveSessionIds).toHaveBeenCalledExactlyOnceWith(
      { partial: UUID, limit: 1000 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(harness.sessions.navigateToSession).toHaveBeenCalledExactlyOnceWith(canonical);
  });

  it("resolves an opaque session ID", async () => {
    const id = "test-session-project-reclassification-nested";
    await open();
    harness.resolveSessionIds.mockResolvedValue({ ids: [id] });
    await setInput(id);

    await pressKey("Enter");

    expect(harness.resolveSessionIds).toHaveBeenCalledExactlyOnceWith(
      { partial: id, limit: 1000 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(harness.sessions.navigateToSession).toHaveBeenCalledExactlyOnceWith(id);
    expect(harness.router.navigateToSession).toHaveBeenCalledExactlyOnceWith(id);
    expect(ui.activeModal).toBeNull();
  });

  it.each([
    ["empty", "", "Enter a session ID or UUID."],
    ["unknown", UUID, "No session matches that ID or UUID."],
  ] as const)("keeps the dialog open for %s input", async (_name, value, message) => {
    await open();
    await setInput(value);

    await pressKey("Enter");

    expect(document.querySelector('[role="alert"]')?.textContent).toContain(message);
    expect(ui.activeModal).toBe("goToSession");
    expect(harness.sessions.navigateToSession).not.toHaveBeenCalled();
    expect(harness.router.navigateToSession).not.toHaveBeenCalled();
    if (_name === "empty") {
      expect(harness.resolveSessionIds).not.toHaveBeenCalled();
    }
  });

  it("reports an ambiguous lookup result without changing the route", async () => {
    await open();
    harness.resolveSessionIds.mockResolvedValue({
      ids: [`codex:${UUID}`, `host~claude:${UUID}`],
    });
    await setInput(UUID);

    await pressKey("Enter");

    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "Multiple sessions match that ID or UUID. Open the desired session from an existing link.",
    );
    expect(ui.activeModal).toBe("goToSession");
    expect(harness.router.navigateToSession).not.toHaveBeenCalled();
  });

  it("reports a capped lookup result without changing the route", async () => {
    await open();
    harness.resolveSessionIds.mockResolvedValue({
      ids: Array.from({ length: 1000 }, (_, index) => `other-${index}`),
    });
    await setInput(UUID);

    await pressKey("Enter");

    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "Too many sessions match that ID or UUID. Open the desired session from an existing link.",
    );
    expect(ui.activeModal).toBe("goToSession");
    expect(harness.router.navigateToSession).not.toHaveBeenCalled();
  });

  it("reports a failed lookup and preserves the input", async () => {
    await open();
    harness.resolveSessionIds.mockRejectedValue(new Error("network down"));
    await setInput(UUID);

    await pressKey("Enter");

    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "Could not look up that session. Try again.",
    );
    expect(input().value).toBe(UUID);
    expect(ui.activeModal).toBe("goToSession");
  });

  it("lets the latest submission win", async () => {
    await open();
    let resolveFirst!: (value: { ids: string[] }) => void;
    let resolveSecond!: (value: { ids: string[] }) => void;
    harness.resolveSessionIds
      .mockImplementationOnce(
        () => new Promise<{ ids: string[] }>((resolve) => { resolveFirst = resolve; }),
      )
      .mockImplementationOnce(
        () => new Promise<{ ids: string[] }>((resolve) => { resolveSecond = resolve; }),
      );

    await setInput(UUID);
    await pressKey("Enter");
    await setInput(UUID.replace("000", "001"));
    await pressKey("Enter");

    resolveFirst({ ids: [`codex:${UUID}`] });
    await settle();
    expect(harness.sessions.navigateToSession).not.toHaveBeenCalled();
    expect(document.querySelector('[role="alert"]')).toBeNull();

    const latestUuid = UUID.replace("000", "001");
    resolveSecond({ ids: [`codex:${latestUuid}`] });
    await settle();
    expect(harness.sessions.navigateToSession).toHaveBeenCalledExactlyOnceWith(`codex:${latestUuid}`);
    expect(harness.router.navigateToSession).toHaveBeenCalledExactlyOnceWith(`codex:${latestUuid}`);
  });

  it("ignores a lookup after Escape or modal replacement", async () => {
    await open();
    let resolve!: (value: { ids: string[] }) => void;
    harness.resolveSessionIds.mockImplementation(
      () => new Promise<{ ids: string[] }>((done) => { resolve = done; }),
    );
    await setInput(UUID);
    await pressKey("Enter");
    await pressKey("Escape");
    expect(ui.activeModal).toBeNull();

    resolve({ ids: [`codex:${UUID}`] });
    await settle();
    expect(harness.sessions.navigateToSession).not.toHaveBeenCalled();

    unmount(component!);
    component = undefined;
    ui.activeModal = "goToSession";
    let resolveReplacement!: (value: { ids: string[] }) => void;
    harness.resolveSessionIds.mockImplementationOnce(
      () => new Promise<{ ids: string[] }>((done) => { resolveReplacement = done; }),
    );
    await open();
    await setInput(UUID);
    await pressKey("Enter");
    ui.activeModal = "shortcuts";
    unmount(component!);
    component = undefined;
    resolveReplacement({ ids: [`codex:${UUID}`] });
    await settle();
    expect(harness.sessions.navigateToSession).not.toHaveBeenCalled();
  });

  it("closes when the modal overlay is pressed", async () => {
    await open();
    const overlay = document.querySelector<HTMLElement>(".kit-modal-overlay")!;

    overlay.dispatchEvent(new MouseEvent("pointerdown", { bubbles: true }));

    expect(ui.activeModal).toBeNull();
  });

  it("supports retrying after an unknown result", async () => {
    await open();
    harness.resolveSessionIds
      .mockResolvedValueOnce({ ids: [] })
      .mockResolvedValueOnce({ ids: [`codex:${UUID}`] });
    await setInput(UUID);
    await pressKey("Enter");
    expect(document.querySelector('[role="alert"]')?.textContent).toContain("No session matches");

    await pressKey("Enter");

    expect(harness.resolveSessionIds).toHaveBeenCalledTimes(2);
    expect(harness.sessions.navigateToSession).toHaveBeenCalledExactlyOnceWith(`codex:${UUID}`);
  });

  it("blocks Enter and Escape during composition or consumed events", async () => {
    await open();
    const wrapper = document.querySelector<HTMLElement>(".go-to-session-input")!;
    const start = new Event("compositionstart", { bubbles: true });
    const end = new Event("compositionend", { bubbles: true });
    wrapper.dispatchEvent(start);
    await setInput(UUID);

    await pressKey("Enter");
    await pressKey("Escape");
    expect(harness.resolveSessionIds).not.toHaveBeenCalled();
    expect(ui.activeModal).toBe("goToSession");

    wrapper.dispatchEvent(end);
    await pressKey("Enter", { isComposing: true });
    await pressKey("Escape", { keyCode: 229 });
    const consumed = new KeyboardEvent("keydown", {
      key: "Enter",
      bubbles: true,
      cancelable: true,
    });
    consumed.preventDefault();
    input().dispatchEvent(consumed);
    await settle();
    expect(harness.resolveSessionIds).not.toHaveBeenCalled();
    expect(ui.activeModal).toBe("goToSession");

    await pressKey("Escape");
    expect(ui.activeModal).toBeNull();
  });
});
