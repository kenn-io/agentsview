import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import type { Match } from "./session-index.js";
import type { SearchBlock } from "./block-text.js";
import { matchSnippet, groupFindResults, resultRows } from "./results.js";
function message(ordinal: number): Message {
  return {
    id: ordinal,
    session_id: "results",
    ordinal,
    role: "assistant",
    content: "needle needle",
    content_length: 13,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
  };
}
function block(ordinal: number): SearchBlock {
  return { key: `${ordinal}:text:0`, ordinal, kind: "text", text: "needle needle" };
}
function match(ordinal: number, occurrence = 0): Match {
  return {
    ordinal,
    blockKey: `${ordinal}:text:0`,
    occurrence,
    start: occurrence * 7,
    end: occurrence * 7 + 6,
  };
}
describe("find result snippets", () => {
  it("shows literal context with explicit truncation flags", () => {
    expect(matchSnippet("abcdefneedleuvwxyz", 6, 12, 3)).toEqual({
      before: "def",
      hit: "needle",
      after: "uvw",
      leading: true,
      trailing: true,
    });
  });
  it("retains all short context without HTML interpretation", () => {
    expect(matchSnippet("<b>needle</b>", 3, 9)).toEqual({
      before: "<b>",
      hit: "needle",
      after: "</b>",
      leading: false,
      trailing: false,
    });
  });
  it("extends snippet edges rather than splitting emoji", () => {
    expect(matchSnippet("x😀needle😀x", 3, 9, 1)).toEqual({
      before: "😀",
      hit: "needle",
      after: "😀",
      leading: true,
      trailing: true,
    });
  });
  it("keeps an emoji match intact", () => {
    expect(matchSnippet("x😀y", 1, 3, 0)).toEqual({
      before: "",
      hit: "😀",
      after: "",
      leading: true,
      trailing: true,
    });
  });
});
describe("find result groups", () => {
  it("groups every occurrence by message and preserves model identity", () => {
    const matches = [match(1), match(1, 1), match(2)];
    const groups = groupFindResults([message(1), message(2)], matches, (source) => [
      block(source.ordinal),
    ]);
    expect(groups.map((group) => group.count)).toEqual([2, 1]);
    expect(groups[0]?.entries[1]?.match).toBe(matches[1]);
    expect(groups[0]?.entries[1]?.snippet.hit).toBe("needle");
  });
  it("collects each matching message's blocks once", () => {
    let calls = 0;
    groupFindResults([message(1)], [match(1), match(1, 1)], (source) => {
      calls++;
      return [block(source.ordinal)];
    });
    expect(calls).toBe(1);
  });
  it("reverses message groups while retaining within-message reading order", () => {
    const groups = groupFindResults(
      [message(1), message(2)],
      [match(1), match(1, 1), match(2)],
      (source) => [block(source.ordinal)],
      true,
    );
    expect(groups.map((group) => group.message.ordinal)).toEqual([2, 1]);
    expect(groups[1]?.entries.map((entry) => entry.match.occurrence)).toEqual([0, 1]);
  });
  it("ignores unloaded messages and stale block identities", () => {
    expect(groupFindResults([message(1)], [match(2)], () => [block(1)])).toEqual([]);
    expect(groupFindResults([message(1)], [match(1)], () => [])).toEqual([]);
  });
  it("sorts groups by ordinal regardless of input message ordering", () => {
    const groups = groupFindResults([message(2), message(1)], [match(2), match(1)], (source) => [
      block(source.ordinal),
    ]);
    expect(groups.map((group) => group.message.ordinal)).toEqual([1, 2]);
  });
  it("virtualizes individual occurrences, including a single giant message", () => {
    const matches = Array.from({ length: 2000 }, (_, occurrence) => ({ ...match(1), occurrence }));
    const rows = resultRows(groupFindResults([message(1)], matches, () => [block(1)]));
    expect(rows).toHaveLength(2001);
    expect(rows[0]?.kind).toBe("group");
    expect(rows[1]?.kind).toBe("match");
    expect(new Set(rows.map((row) => row.key)).size).toBe(2001);
  });
});
