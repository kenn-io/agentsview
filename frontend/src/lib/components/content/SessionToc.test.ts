// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import { SessionsService, type DbMessage as Message } from "../../api/generated/index.js";
import { messages } from "../../stores/messages.svelte.js";
import { sessionToc } from "../../stores/sessionToc.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import SessionToc from "./SessionToc.svelte";

let sessionCounter = 0;
let sessionId = "";

function msg(ordinal: number, extra: Partial<Message> = {}): Message {
  return {
    id: ordinal + 1,
    session_id: sessionId,
    ordinal,
    role: "user",
    content: `prompt ${ordinal}`,
    content_length: 8,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
    ...extra,
  };
}

function load(rows: Message[], hasOlder = false) {
  messages.sessionId = sessionId;
  messages.messages = rows;
  messages.hasOlder = hasOlder;
  messages.loading = false;
}

async function settle() {
  for (let i = 0; i < 5; i++) {
    await Promise.resolve();
    flushSync();
  }
}

function labels(): string[] {
  return [...document.querySelectorAll(".toc-entry")].map((el) => el.textContent!.replace(/\s+/g, " ").trim());
}

let component: ReturnType<typeof mount> | null = null;
function render() {
  component = mount(SessionToc, { target: document.body });
  flushSync();
}

beforeEach(() => {
  sessionId = `toc-session-${++sessionCounter}`;
  sessionToc.setOpen(true);
  sessionToc.resetView();
  ui.sortNewestFirst = false;
  ui.setBlockVisible("user", true);
  ui.pendingScrollOrdinal = null;
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  vi.restoreAllMocks();
  sessionToc.setOpen(false);
  messages.messages = [];
  messages.hasOlder = false;
  messages.sessionId = null;
  document.body.innerHTML = "";
});

describe("SessionToc", () => {
  it("lists the session's prompts in transcript order and marks the turn on screen", () => {
    load([
      msg(0, { content: "Set up the repo" }),
      msg(1, { role: "assistant", content: "Done" }),
      msg(2, { is_system: true, content: "injected" }),
      msg(3, { content: "Add tests\nwith details" }),
      msg(4, { role: "assistant" }),
    ]);
    sessionToc.view = { topOrdinal: 4, minOrdinal: 4, maxOrdinal: 4, atEnd: false };
    render();

    expect(labels()).toEqual(["1 Set up the repo", "2 Add tests"]);
    expect(document.querySelector(".toc-count")!.textContent!.trim()).toBe("2 prompts");
    const current = document.querySelector('[aria-current="location"]');
    expect(current?.textContent).toContain("Add tests");

    ui.sortNewestFirst = true;
    flushSync();
    expect(labels()).toEqual(["2 Add tests", "1 Set up the repo"]);
  });

  it("jumps to a prompt when its entry is clicked", () => {
    load([msg(0), msg(1, { role: "assistant" }), msg(2)]);
    render();

    (document.querySelector('button[data-ordinal="2"]') as HTMLButtonElement).click();
    flushSync();

    expect(ui.pendingScrollOrdinal).toBe(2);
    expect(ui.selectedOrdinal).toBe(2);
    expect(document.querySelector('[aria-current="location"]')).toBeNull();
    sessionToc.view = { topOrdinal: 2, minOrdinal: 2, maxOrdinal: 2, atEnd: false };
    flushSync();
    expect(document.querySelector('[aria-current="location"]')?.getAttribute("data-ordinal")).toBe("2");
  });

  it("moves between entries with the arrow keys and closes on Escape or t", () => {
    load([msg(0), msg(2), msg(4)]);
    const toggle = document.createElement("button");
    toggle.setAttribute("aria-controls", "session-toc");
    document.body.append(toggle);
    const documentKeys = vi.fn();
    document.addEventListener("keydown", documentKeys);
    render();

    const buttons = () => [...document.querySelectorAll<HTMLButtonElement>(".toc-entry")];
    expect(buttons().map((b) => b.tabIndex)).toEqual([0, -1, -1]);
    buttons()[0]!.focus();
    const press = (key: string) =>
      document.activeElement!.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));

    press("ArrowDown");
    flushSync();
    expect(document.activeElement).toBe(buttons()[1]);
    expect(buttons().map((b) => b.tabIndex)).toEqual([-1, 0, -1]);
    press("End");
    expect(document.activeElement).toBe(buttons()[2]);
    press("ArrowDown");
    expect(document.activeElement).toBe(buttons()[2]);
    press("Home");
    expect(document.activeElement).toBe(buttons()[0]);
    // The transcript's own arrow navigation must not also run.
    expect(documentKeys).not.toHaveBeenCalled();

    press("Escape");
    flushSync();
    expect(sessionToc.open).toBe(false);
    expect(document.querySelector(".session-toc")).toBeNull();
    expect(document.activeElement).toBe(toggle);

    sessionToc.setOpen(true);
    flushSync();
    buttons()[1]!.focus();
    press("t");
    flushSync();
    expect(sessionToc.open).toBe(false);
    expect(document.activeElement).toBe(toggle);
    expect(documentKeys).not.toHaveBeenCalled();
    document.removeEventListener("keydown", documentKeys);
  });

  it("reads prompts older than the loaded window and numbers them first", async () => {
    const read = vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages").mockResolvedValue({
      messages: [msg(40, { content: "older two" }), msg(30, { is_system: true }), msg(10, { content: "older one" })],
    } as never);
    load([msg(100, { content: "loaded" })], true);
    render();
    expect(document.querySelector(".toc-note")?.textContent).toBe("Loading earlier prompts…");
    expect(document.querySelector(".toc-count")).toBeNull();

    await settle();

    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0]![0]).toEqual({ id: sessionId });
    expect(read.mock.calls[0]![1]).toEqual({ roles: "user", direction: "desc", from: 99, limit: 1000 });
    expect(labels()).toEqual(["1 older one", "2 older two", "3 loaded"]);
    expect(document.querySelector(".toc-count")!.textContent!.trim()).toBe("3 prompts");

    // Loading more history into the window needs no second read.
    load([msg(40, { content: "older two" }), msg(100, { content: "loaded" })], true);
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    expect(labels()).toEqual(["1 older one", "2 older two", "3 loaded"]);
  });

  it("offers a retry when earlier prompts fail to load", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const read = vi
      .spyOn(SessionsService, "getApiV1SessionsByIdMessages")
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValueOnce({ messages: [msg(3, { content: "recovered" })] } as never);
    load([msg(50)], true);
    render();
    await settle();

    expect(document.querySelector('[role="alert"]')?.textContent).toContain("Could not load earlier prompts");
    expect(labels()).toEqual(["prompt 50"]);

    (document.querySelector('[role="alert"] button') as HTMLButtonElement).click();
    await settle();

    expect(read).toHaveBeenCalledTimes(2);
    expect(labels()).toEqual(["1 recovered", "2 prompt 50"]);
  });

  it("explains an empty outline", () => {
    load([msg(0, { role: "assistant" })]);
    render();
    expect(document.querySelector(".toc-note")?.textContent).toBe("No prompts in this session");

    load([msg(0)]);
    ui.setBlockVisible("user", false);
    flushSync();
    expect(document.querySelector(".toc-note")?.textContent).toBe("Prompts are hidden by the message filter");
    expect(document.querySelector(".toc-entry")).toBeNull();
  });
});
