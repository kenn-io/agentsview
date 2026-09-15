// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import { clearContentCaches } from "../utils/content-parser.js";
import { collectSearchBlocks } from "./block-text.js";
import { buildSessionIndex } from "./session-index.js";
let id = 940000;
function message(text: string): Message {
  const content = `\`\`\`text\n${text}\n\`\`\``;
  return {
    id: id++,
    session_id: "cache-test",
    ordinal: 0,
    role: "assistant",
    content,
    content_length: content.length,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    is_system: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
  };
}
beforeEach(clearContentCaches);
describe("prepared session block cache", () => {
  it("keeps exact positions across repeated query changes", () => {
    const source = message("needle İ needle");
    expect(
      buildSessionIndex([source], "needle").matches.map(({ start, end }) => [start, end]),
    ).toEqual([
      [0, 6],
      [9, 15],
    ]);
    expect(buildSessionIndex([source], "i").matches.map(({ start, end }) => [start, end])).toEqual([
      [7, 8],
    ]);
    expect(buildSessionIndex([source], "absent").total).toBe(0);
    expect(buildSessionIndex([source], "needle").total).toBe(2);
  });
  it("invalidates when a replacement message version arrives", () => {
    const source = message("before");
    buildSessionIndex([source], "needle");
    const replacement = { ...message("needle"), id: source.id };
    // The message loader clears this existing parser cache on SSE replacement.
    clearContentCaches();
    expect(buildSessionIndex([replacement], "needle").total).toBe(1);
    expect(buildSessionIndex([replacement], "before").total).toBe(0);
  });
  it("does not retain stale prepared text after an in-place block edit", () => {
    const source = message("before");
    buildSessionIndex([source], "needle");
    collectSearchBlocks(source)[0]!.text = "needle";
    expect(buildSessionIndex([source], "needle").total).toBe(1);
    expect(buildSessionIndex([source], "before").total).toBe(0);
  });
  it("keeps blank queries empty even after a cache is populated", () => {
    const source = message("needle");
    expect(buildSessionIndex([source], "needle").total).toBe(1);
    expect(buildSessionIndex([source], " ").total).toBe(0);
  });
});
