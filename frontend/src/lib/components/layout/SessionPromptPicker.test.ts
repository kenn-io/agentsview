// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { flushSync, mount, unmount } from "svelte";
import { SessionsService, type DbMessage as Message } from "../../api/generated/index.js";
import { ApiError } from "../../api/runtime.js";
import { messages } from "../../stores/messages.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { msg } from "../../utils/test-messages.js";
import SessionPromptPicker from "./SessionPromptPicker.svelte";
import { openPromptPicker } from "./prompt-picker.js";

// Parsed content is cached by id and length, so each prompt needs its own.
const prompt = (ordinal: number, content = `prompt ${ordinal}`) =>
  msg({ id: ordinal + 1, ordinal, content, content_length: content.length });

// Serves the pages and puts the same rows in the transcript, as when the
// transcript has caught up with the archive.
function serve(...pages: Message[][]) {
  const read = vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages");
  for (const page of pages) {
    read.mockResolvedValueOnce({ messages: page, transcript_revision: "r1" } as never);
  }
  messages.messages = pages.flat();
  messages.loadedRevision = "r1";
  return read;
}

function hang() {
  vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages").mockReturnValue(
    new Promise(() => {}) as never,
  );
}

async function close() {
  await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
  await settle();
}

async function settle() {
  for (let i = 0; i < 5; i++) {
    await Promise.resolve();
    flushSync();
  }
}

function labels(): string[] {
  return screen
    .queryAllByRole("option")
    .map((el) => el.querySelector(".kit-typeahead__option-label")!.textContent!.trim());
}

async function open() {
  await fireEvent.click(screen.getByRole("button", { name: "Jump to prompt" }));
  await settle();
}

async function search(text: string) {
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: text } });
  await settle();
}

let component: ReturnType<typeof mount> | null = null;

beforeEach(() => {
  messages.sessionId = "s1";
  ui.pendingScrollOrdinal = null;
  component = mount(SessionPromptPicker, { target: document.body });
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  vi.restoreAllMocks();
  ui.showAllBlocks();
  messages.sessionId = null;
  messages.messages = [];
  messages.loadedRevision = null;
  document.body.innerHTML = "";
});

describe("SessionPromptPicker", () => {
  it("reads every page of prompts and lists them newest first", async () => {
    const first = Array.from({ length: 1000 }, (_, i) => prompt(i * 2));
    const read = serve(first, [prompt(2000, "Latest prompt")]);
    await open();

    expect(read.mock.calls.map((call) => call[1])).toEqual([
      { roles: "user", direction: "asc", from: 0, limit: 1000 },
      { roles: "user", direction: "asc", from: 1999, limit: 1000, expected_revision: "r1" },
    ]);
    const newest = screen.getAllByRole("option")[0]!;
    expect(newest.textContent).toContain("Latest prompt");
    expect(newest.querySelector(".kit-typeahead__option-meta")!.textContent).toContain("#1,001");
  });

  it("restarts the read when the transcript changes between pages", async () => {
    const read = vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages");
    const page = (revision: string, rows: Message[]) =>
      read.mockResolvedValueOnce({ messages: rows, transcript_revision: revision } as never);
    page(
      "r1",
      Array.from({ length: 1000 }, (_, i) => prompt(i, "old")),
    );
    read.mockRejectedValueOnce(new ApiError(409, "transcript changed"));
    page("r2", [prompt(0, "Rewritten prompt")]);
    messages.messages = [prompt(0, "Rewritten prompt")];
    messages.loadedRevision = "r2";
    await open();

    expect(read.mock.calls.map((call) => call[1])).toEqual([
      { roles: "user", direction: "asc", from: 0, limit: 1000 },
      { roles: "user", direction: "asc", from: 1000, limit: 1000, expected_revision: "r1" },
      { roles: "user", direction: "asc", from: 0, limit: 1000 },
    ]);
    expect(labels()).toEqual(["Rewritten prompt"]);
  });

  it("offers only the prompts the transcript's filters show", async () => {
    serve([prompt(0, "Plain prompt"), prompt(1, "Run this\n```sh\nmake test\n```")]);
    await open();
    expect(labels()).toEqual(["Run this", "Plain prompt"]);

    ui.setBlockVisible("user", false);
    await settle();
    expect(labels()).toEqual(["Run this"]);

    ui.setBlockVisible("user", true);
    await settle();
    expect(labels()).toEqual(["Run this", "Plain prompt"]);
  });

  it("keeps the last list on reopening only while the transcript is unchanged", async () => {
    serve([prompt(0, "Before the resync")]);
    await open();
    await close();

    hang();
    await open();
    expect(labels()).toEqual(["Before the resync"]);
    await close();

    // A resync moved the transcript on, so the old ordinals may be wrong.
    messages.loadedRevision = "r2";
    await open();
    expect(labels()).toEqual([]);
    expect(document.body.textContent).toContain("Loading prompts…");
  });

  it("withdraws an open list when the transcript moves to another revision", async () => {
    serve([prompt(0, "Before the resync")]);
    await open();
    expect(labels()).toEqual(["Before the resync"]);

    messages.loadedRevision = "r2";
    await settle();
    expect(labels()).toEqual([]);
    expect(document.body.textContent).toContain("Loading prompts…");
  });

  it("waits for the transcript to reach the revision the prompts were read from", async () => {
    vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages").mockResolvedValueOnce({
      messages: [prompt(0, "Rewritten prompt")],
      transcript_revision: "r2",
    } as never);
    messages.messages = [prompt(0, "Old prompt")];
    messages.loadedRevision = "r1";
    await open();
    expect(labels()).toEqual([]);
    expect(document.body.textContent).toContain("Loading prompts…");

    messages.messages = [prompt(0, "Rewritten prompt")];
    messages.loadedRevision = "r2";
    await settle();
    expect(labels()).toEqual(["Rewritten prompt"]);
  });

  it("offers no cached prompts when a reread fails after a resync", async () => {
    serve([prompt(0, "Before the resync")]);
    await open();
    await close();

    messages.loadedRevision = "r2";
    vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages").mockRejectedValue(new Error("down"));
    await open();
    expect(labels()).toEqual([]);
    expect(screen.getByRole("alert").textContent).toContain("Could not load prompts");
  });

  it("holds back prompts the transcript has not loaded yet", async () => {
    serve([prompt(0, "Loaded"), prompt(2, "Just typed")]);
    messages.messages = [prompt(0, "Loaded"), msg({ id: 99, ordinal: 1, role: "assistant" })];
    await open();
    expect(labels()).toEqual(["Loaded"]);

    messages.messages = [...messages.messages, prompt(2, "Just typed")];
    await settle();
    expect(labels()).toEqual(["Just typed", "Loaded"]);
  });

  it("lists only typed prompts, by their first line", async () => {
    serve([
      prompt(0, "Set up the repo\nwith details"),
      msg({ ordinal: 1, source_subtype: "tool_result", content: "output" }),
      msg({ ordinal: 2, content: "<bash-stdout>ok</bash-stdout>" }),
      prompt(3, "Ship it"),
    ]);
    await open();

    expect(labels()).toEqual(["Ship it", "Set up the repo"]);
  });

  it("matches every typed word anywhere in a prompt", async () => {
    serve([
      prompt(0, "Look at the handler\nthe cache key ignores rounding"),
      prompt(1, "Fix the cache"),
      prompt(2, "Write the changelog"),
    ]);
    await open();

    await search("Rounding  CACHE");
    expect(labels()).toEqual(["Look at the handler"]);
    await search("deploy");
    expect(labels()).toEqual([]);
    expect(document.body.textContent).toContain("No prompts match");
  });

  it("renders at most 100 matches and says how many there are", async () => {
    serve(Array.from({ length: 250 }, (_, i) => prompt(i)));
    await open();

    expect(screen.getAllByRole("option")).toHaveLength(100);
    expect(labels()[0]).toBe("prompt 249");
    expect(document.querySelector(".picker-status")!.textContent!.trim()).toBe(
      "Showing the newest 100 of 250. Type to narrow.",
    );

    // 24, 124, 224, and 240-249: few enough to show them all.
    await search("prompt 24");
    expect(screen.getAllByRole("option")).toHaveLength(13);
    expect(document.querySelector(".picker-status")!.textContent!.trim()).toBe("");
  });

  it("jumps to the chosen prompt", async () => {
    serve([prompt(0, "First"), prompt(4, "Second")]);
    await open();
    await fireEvent.mouseDown(screen.getByRole("option", { name: /Second/ }));

    expect(ui.pendingScrollOrdinal).toBe(4);
  });

  it("drops the previous session's prompts while the next one loads", async () => {
    serve([prompt(0, "From session one")]);
    await open();
    await close();

    messages.sessionId = "s2";
    hang();
    await open();

    expect(labels()).toEqual([]);
    expect(document.body.textContent).toContain("Loading prompts…");
  });

  it("shows an error when the prompts cannot be read", async () => {
    vi.spyOn(SessionsService, "getApiV1SessionsByIdMessages").mockRejectedValue(new Error("down"));
    await open();

    expect(screen.getByRole("alert").textContent).toContain("Could not load prompts");
  });

  it("opens from the keyboard shortcut", async () => {
    serve([prompt(0, "Only prompt")]);
    openPromptPicker();
    await settle();

    expect(labels()).toEqual(["Only prompt"]);
    expect(document.activeElement).toBe(screen.getByRole("combobox"));
  });
});
