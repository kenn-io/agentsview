// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { flushSync, mount, unmount } from "svelte";
import { SessionsService, type DbMessage as Message } from "../../api/generated/index.js";
import { messages } from "../../stores/messages.svelte.js";
import { sessionToc } from "../../stores/sessionToc.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import SessionPromptPicker from "./SessionPromptPicker.svelte";

let sessionCounter = 0;
let sessionId = "";

function msg(ordinal: number, extra: Partial<Message> = {}): Message {
  return {
    id: ordinal + 1,
    session_id: sessionId,
    ordinal,
    role: "user",
    content: `prompt ${ordinal}`,
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
    content_length: (extra.content ?? `prompt ${ordinal}`).length,
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

function optionLabels(): string[] {
  return screen
    .queryAllByRole("option")
    .map((el) => el.querySelector(".kit-typeahead__option-label")!.textContent!.trim());
}

function status(): string {
  return document.querySelector(".picker-status")!.textContent!.replace(/\s+/g, " ").trim();
}

async function open() {
  await fireEvent.click(screen.getByRole("button", { name: "Jump to prompt" }));
  await settle();
}

let component: ReturnType<typeof mount> | null = null;

beforeEach(() => {
  sessionId = `picker-session-${++sessionCounter}`;
  sessionToc.resetView();
  ui.setBlockVisible("user", true);
  ui.pendingScrollOrdinal = null;
  component = mount(SessionPromptPicker, { target: document.body });
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  vi.restoreAllMocks();
  messages.messages = [];
  messages.hasOlder = false;
  messages.sessionId = null;
  document.body.innerHTML = "";
});

describe("SessionPromptPicker", () => {
  it("lists only typed prompts and marks the turn on screen", async () => {
    load([
      msg(0, { content: "Set up the repo" }),
      msg(1, { role: "assistant", content: "Done" }),
      msg(2, { is_system: true, content: "injected" }),
      msg(3, { content: "Add tests\nwith details" }),
      msg(4, { role: "assistant" }),
      msg(5, { content: "Ship it" }),
    ]);
    sessionToc.view = { topOrdinal: 4, minOrdinal: 3, maxOrdinal: 4, atEnd: false };
    await open();

    expect(optionLabels()).toEqual(["Set up the repo", "Add tests", "Ship it"]);
    expect(status()).toBe("3 prompts");
    const current = screen.getByRole("option", { selected: true });
    expect(current.textContent).toContain("Add tests");
    expect(current.querySelector(".kit-typeahead__option-meta")!.textContent).toContain("#2");
  });

  it("searches the whole prompt, not just its first line", async () => {
    load([
      msg(0, { content: "Look at the handler\nthe cache key ignores rounding" }),
      msg(1, { content: "Fix the cache" }),
      msg(2, { content: "Write the changelog" }),
    ]);
    await open();
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "cache rounding" } });
    await settle();

    expect(optionLabels()).toEqual(["Look at the handler"]);
    expect(status()).toBe("1 of 3 prompts");

    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "deploy" } });
    await settle();
    expect(optionLabels()).toEqual([]);
    expect(document.body.textContent).toContain("No prompts match");
  });

  it("jumps to the chosen prompt", async () => {
    load([msg(0, { content: "First" }), msg(1, { content: "Second" })]);
    await open();
    await fireEvent.mouseDown(screen.getByRole("option", { name: /Second/ }));
    await settle();

    expect(ui.pendingScrollOrdinal).toBe(1);
    expect(sessionToc.picked).toBe(1);
  });

  it("renders a window of the first 100 prompts in a very long session", async () => {
    load(Array.from({ length: 250 }, (_, i) => msg(i)));
    await open();

    expect(screen.getAllByRole("option")).toHaveLength(100);
    expect(optionLabels()[0]).toBe("prompt 0");
    expect(status()).toBe("Prompts 1–100 of 250. Type to search them all.");

    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "prompt" } });
    await settle();
    expect(screen.getAllByRole("option")).toHaveLength(100);
    expect(status()).toBe("250 matches, showing the first 100");
  });

  it("reads prompts older than the loaded window and numbers them in order", async () => {
    let respond!: (value: unknown) => void;
    const read = vi
      .spyOn(SessionsService, "getApiV1SessionsByIdMessages")
      .mockReturnValue(new Promise((resolve) => (respond = resolve)) as never);
    load([msg(4, { content: "Loaded prompt" })], true);
    await open();
    // Counts would cover only the loaded page until the read finishes.
    expect(status()).toBe("Loading earlier prompts…");

    respond({
      messages: [msg(2, { content: "Second prompt" }), msg(0, { content: "First prompt" })],
      transcript_revision: "rev-1",
    });
    await settle();
    expect(status()).toBe("3 prompts");

    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0]![0]).toEqual({ id: sessionId });
    expect(read.mock.calls[0]![1]).toEqual({
      roles: "user",
      direction: "desc",
      from: 3,
      limit: 1000,
    });
    expect(optionLabels()).toEqual(["First prompt", "Second prompt", "Loaded prompt"]);
    const loaded = screen.getByRole("option", { name: /Loaded prompt/ });
    expect(loaded.querySelector(".kit-typeahead__option-meta")!.textContent).toContain("#3");
  });

  it("opens from the keyboard shortcut's request", async () => {
    load([msg(0, { content: "Only prompt" })]);
    sessionToc.requestOpen();
    await settle();

    expect(optionLabels()).toEqual(["Only prompt"]);
    expect(document.activeElement).toBe(screen.getByRole("combobox"));
  });
});
