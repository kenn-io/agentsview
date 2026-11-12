import { describe, expect, it } from "vite-plus/test";
import type { DbMessage as Message } from "../../api/generated/index.js";
import {
  currentEntryIndex,
  entryVisible,
  isPromptMessage,
  mergeTocEntries,
  pickPrompts,
  promptEntries,
  tocEntry,
  type TocEntry,
} from "./session-toc.js";

function msg(ordinal: number, extra: Partial<Message> = {}): Message {
  return {
    id: ordinal + 1,
    session_id: "s",
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

const entry = (ordinal: number, search = `prompt ${ordinal}`): TocEntry => ({
  ordinal,
  timestamp: "2026-01-01T00:00:00Z",
  label: `prompt ${ordinal}`,
  search,
  isShell: false,
  blocks: ["user"],
});

describe("isPromptMessage", () => {
  it("keeps typed user prompts, including empty ones and shell commands", () => {
    expect(isPromptMessage(msg(0))).toBe(true);
    expect(isPromptMessage(msg(1, { content: "" }))).toBe(true);
    expect(isPromptMessage(msg(2, { content: "<bash-input>ls</bash-input>" }))).toBe(true);
    expect(
      isPromptMessage(
        msg(3, { content: "<bash-input>ls</bash-input><bash-stdout>a</bash-stdout>" }),
      ),
    ).toBe(true);
  });

  it.each([
    ["an assistant reply", { role: "assistant" }],
    ["a system-flagged row", { is_system: true }],
    ["a continuation card", { is_system: true, source_subtype: "continuation" }],
    ["a compaction boundary", { is_compact_boundary: true }],
    ["stored tool output", { source_subtype: "tool_result" }],
    ["injected command output", { content: "<local-command-stdout>ok</local-command-stdout>" }],
    ["a reminder-only row", { content: "<system-reminder>ctx</system-reminder>" }],
    ["shell output", { content: "<bash-stdout>hello</bash-stdout>" }],
    [
      "shell output and errors",
      { content: "<bash-stdout>a</bash-stdout><bash-stderr>oops</bash-stderr>" },
    ],
    [
      "shell output that prints a closing tag",
      { content: "<bash-stdout>before </bash-stdout> after</bash-stdout>" },
    ],
    [
      "shell output that prints other shell tags",
      {
        content:
          "<bash-stdout>before </bash-stdout><bash-stderr>after</bash-stdout><bash-stderr></bash-stderr>",
      },
    ],
    [
      "shell output after a reminder",
      { content: "<system-reminder>ctx</system-reminder>\n<bash-stdout>ok</bash-stdout>" },
    ],
  ] satisfies [string, Partial<Message>][])("drops %s", (_name, extra) => {
    expect(isPromptMessage(msg(0, extra))).toBe(false);
  });
});

describe("tocEntry", () => {
  it("labels a prompt with its first line, whitespace collapsed", () => {
    const e = tocEntry(
      msg(3, { content: "<system-reminder>ctx</system-reminder>\n  Fix   the\tbug\nand more" }),
    );
    expect(e).toMatchObject({ ordinal: 3, label: "Fix the bug", isShell: false });
  });

  it("shows a shell shortcut as the command typed", () => {
    const e = tocEntry(msg(1, { content: "<bash-input>git status</bash-input>" }));
    expect(e.label).toBe("!git status");
    expect(e.isShell).toBe(true);
  });

  it("caps a long first line", () => {
    expect(tocEntry(msg(1, { content: "x".repeat(500) })).label).toHaveLength(160);
  });
});

describe("entryVisible", () => {
  const hiding =
    (...hidden: string[]) =>
    (type: string) =>
      !hidden.includes(type);

  it("hides a text-only prompt with the user filter", () => {
    const e = tocEntry(msg(1, { content: "just words" }));
    expect(entryVisible(e, hiding())).toBe(true);
    expect(entryVisible(e, hiding("user"))).toBe(false);
  });

  it("keeps a prompt with a code fence, as the transcript keeps its card", () => {
    const e = tocEntry(msg(1, { content: "Why does this fail?\n```go\nfunc main() {}\n```" }));
    expect(e.blocks).toContain("code");
    expect(entryVisible(e, hiding("user"))).toBe(true);
    expect(entryVisible(e, hiding("user", "code"))).toBe(true);
  });
});

describe("mergeTocEntries", () => {
  it("puts prompts read before the loaded window ahead of the window's own prompts", () => {
    const loaded = [msg(10), msg(11, { role: "assistant" }), msg(12, { content: "newest" })];
    const merged = mergeTocEntries([entry(2), entry(6)], loaded);
    expect(merged.map((e) => e.ordinal)).toEqual([2, 6, 10, 12]);
    expect(merged.at(-1)!.label).toBe("newest");
  });

  it("takes the loaded copy where an earlier read overlaps the window", () => {
    const merged = mergeTocEntries(
      [entry(2), { ...entry(10), label: "stale" }],
      [msg(10, { content: "fresh" })],
    );
    expect(merged).toEqual([
      entry(2),
      {
        ordinal: 10,
        timestamp: "2026-01-01T00:00:00Z",
        label: "fresh",
        search: "fresh",
        isShell: false,
        blocks: ["user"],
      },
    ]);
  });

  it("keeps every earlier prompt when nothing is loaded", () => {
    expect(mergeTocEntries([entry(1)], [])).toEqual([entry(1)]);
  });
});

describe("currentEntryIndex", () => {
  const entries = promptEntries([msg(0), msg(5), msg(9)]);
  const view = (topOrdinal: number, minOrdinal: number, maxOrdinal: number, atEnd = false) => ({
    topOrdinal,
    minOrdinal,
    maxOrdinal,
    atEnd,
  });

  it("marks the last prompt at or before the top row", () => {
    expect(currentEntryIndex(entries, view(7, 7, 8), null)).toBe(1);
    expect(currentEntryIndex(entries, view(5, 5, 6), null)).toBe(1);
    expect(currentEntryIndex(entries, view(4, 4, 6), null)).toBe(0);
  });

  it("marks the last prompt on screen once the transcript reaches its end", () => {
    expect(currentEntryIndex(entries, view(7, 7, 10, true), null)).toBe(2);
  });

  it("keeps a picked prompt current while it is on screen", () => {
    expect(currentEntryIndex(entries, view(4, 4, 10), 9)).toBe(2);
    expect(currentEntryIndex(entries, view(0, 0, 3), 9)).toBe(0);
  });

  it("marks nothing above the first prompt or without a view", () => {
    expect(currentEntryIndex(promptEntries([msg(3)]), view(1, 1, 2), null)).toBe(-1);
    expect(currentEntryIndex(entries, null, null)).toBe(-1);
  });
});

describe("tocEntry search text", () => {
  it("searches past the first line, lowercased with whitespace collapsed", () => {
    const e = tocEntry(msg(0, { content: "Fix the CACHE\n\n  eviction   bug" }));
    expect(e.label).toBe("Fix the CACHE");
    expect(e.search).toBe("fix the cache eviction bug");
  });
});

describe("pickPrompts", () => {
  const many = Array.from({ length: 300 }, (_, i) => entry(i));
  const ordinals = (entries: readonly TocEntry[]) => entries.map((e) => e.ordinal);

  it("lists the current prompt first, then the ones before it, with no query", () => {
    const pick = pickPrompts(many, "", 150, 4);
    expect(ordinals(pick.shown)).toEqual([150, 149, 148, 147]);
    expect(pick).toMatchObject({ matched: 300, start: 147 });
  });

  it("stops at the first prompt and starts at the last one without a current prompt", () => {
    expect(ordinals(pickPrompts(many, "", 1, 4).shown)).toEqual([1, 0]);
    expect(ordinals(pickPrompts(many, "", -1, 4).shown)).toEqual([299, 298, 297, 296]);
  });

  it("matches prompts containing every word, in any order and case", () => {
    const entries = [
      entry(0, "fix the cache eviction bug"),
      entry(1, "cache hit rate is low"),
      entry(2, "eviction policy for the cache"),
      entry(3, "unrelated"),
    ];
    expect(ordinals(pickPrompts(entries, "  Cache   EVICTION ", -1).shown)).toEqual([0, 2]);
    expect(pickPrompts(entries, "nothing here", -1)).toEqual({ shown: [], matched: 0, start: 0 });
  });

  it("counts every match but shows only the first ones up to the limit", () => {
    // Prompts containing "29": 29, 129, 229, and 290-299.
    const pick = pickPrompts(many, "prompt 29", 0, 3);
    expect(pick.matched).toBe(13);
    expect(ordinals(pick.shown)).toEqual([29, 129, 229]);
  });
});
