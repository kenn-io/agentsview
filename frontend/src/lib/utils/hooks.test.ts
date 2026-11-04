// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, unmount } from "svelte";
import type { DbMessage as Message } from "../api/generated/index.js";
import SystemBoundaryCard from "../components/system/SystemBoundaryCard.svelte";
import { ALL_BLOCK_TYPES, parseBlockFilters, serializeBlockFilters } from "../stores/ui.svelte.js";
import { projectSessionScope } from "../search/session-scope.js";
import { hasVisibleSegments } from "./content-parser.js";
import { buildDisplayItems } from "./display-items.js";
import { isSystemMessage } from "./messages.js";
import { filterDisplayItemsByTranscriptMode } from "./transcript-mode.js";

function message(ordinal: number, overrides: Partial<Message> = {}): Message {
  return {
    id: ordinal + 1,
    session_id: "synthetic-hooks",
    ordinal,
    role: "user",
    content: "request",
    content_length: 7,
    timestamp: "2024-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
    ...overrides,
  };
}
const hook = () =>
  message(2, {
    is_system: true,
    source_type: "attachment",
    source_subtype: "hook_success",
    content: "Hook name: Example\n\nStdout:\n  中文 e\u0301 <script>literal</script>\n",
  });

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

describe("hook output", () => {
  it("promotes hook output and keeps other system rows hidden", () => {
    expect(isSystemMessage(hook())).toBe(false);
    expect(isSystemMessage(message(2, { ...hook(), source_subtype: "hook_error" }))).toBe(true);
    expect(isSystemMessage(message(2, { is_system: true, content: "Hook output" }))).toBe(true);
    expect(
      hasVisibleSegments(message(2, { source_subtype: "hook_success" }), (t) => t === "hook"),
    ).toBe(false);
  });

  it("belongs to the Hooks filter independently of the role and system filters", () => {
    expect(hasVisibleSegments(hook(), (t) => t === "hook")).toBe(true);
    expect(hasVisibleSegments(hook(), (t) => t !== "hook")).toBe(false);
    const boundary = message(4, {
      is_system: true,
      source_type: "system",
      source_subtype: "continuation",
    });
    expect(hasVisibleSegments(boundary, (t) => t === "system")).toBe(true);
    expect(hasVisibleSegments(boundary, (t) => t === "hook")).toBe(false);
  });

  it("extends saved filters without linking Hooks to System boundaries", () => {
    const old = parseBlockFilters(JSON.stringify({ hidden: ["system"] }));
    expect(old.has("system")).toBe(false);
    expect(old.has("hook")).toBe(true);
    const visible = new Set(ALL_BLOCK_TYPES);
    visible.delete("hook");
    const restored = parseBlockFilters(serializeBlockFilters(visible));
    expect(restored.has("hook")).toBe(false);
    expect(restored.has("system")).toBe(true);
    expect(parseBlockFilters(null).has("hook")).toBe(true);
  });

  it("uses the same filter for projected transcript rows", () => {
    const rows = [message(0), hook(), message(3, { role: "assistant", content: "final" })];
    const onlyHooks = projectSessionScope({
      messages: rows,
      visibleBlocks: new Set(["hook"]),
      hasBlockFilters: true,
    });
    expect(onlyHooks.messages).toEqual([rows[1]]);
    const withoutHooks = new Set(ALL_BLOCK_TYPES);
    withoutHooks.delete("hook");
    const hidden = projectSessionScope({
      messages: rows,
      visibleBlocks: withoutHooks,
      hasBlockFilters: true,
    });
    expect(hidden.messages).toEqual([rows[0], rows[2]]);
    expect(hidden.allowsBlock(rows[1]!, "text")).toBe(false);
  });

  it("does not finish a turn or promote an intermediate reply in focused mode", () => {
    const items = buildDisplayItems([
      message(0),
      message(1, { role: "assistant", content: "intermediate" }),
      hook(),
      message(3, { role: "assistant", content: "final" }),
      message(4),
    ]);
    expect(
      filterDisplayItemsByTranscriptMode(items, "focused").flatMap((item) => item.ordinals),
    ).toEqual([0, 3, 4]);
    expect(
      filterDisplayItemsByTranscriptMode(items, "normal").flatMap((item) => item.ordinals),
    ).toEqual([0, 1, 2, 3, 4]);
  });

  it("uses a localized collapsed card and preserves escaped body text", () => {
    const content = hook().content;
    component = mount(SystemBoundaryCard, {
      target: document.body,
      props: { subtype: "hook_success", content, timestamp: "" },
    });
    expect(document.querySelector(".label")?.textContent).toBe("Hook output");
    const details = document.querySelector("details");
    expect(details).not.toBeNull();
    expect(details?.open).toBe(false);
    expect(details?.querySelector("pre")?.textContent).toBe(content);
    expect(document.querySelector("script")).toBeNull();
  });

  it("keeps the hook label without an empty details control", () => {
    component = mount(SystemBoundaryCard, {
      target: document.body,
      props: { subtype: "hook_success", content: "", timestamp: "" },
    });
    expect(document.querySelector(".label")?.textContent).toBe("Hook output");
    expect(document.querySelector("details")).toBeNull();
  });
});
