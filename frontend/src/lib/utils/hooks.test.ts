// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, unmount } from "svelte";
import { msg } from "./test-messages.js";
import SystemBoundaryCard from "../components/system/SystemBoundaryCard.svelte";
import { ALL_BLOCK_TYPES } from "../stores/ui.svelte.js";
import { projectSessionScope } from "../search/session-scope.js";
import { hasVisibleSegments } from "./content-parser.js";
import { buildDisplayItems } from "./display-items.js";
import { isHookSuccessMessage, isSystemMessage } from "./messages.js";
import { filterDisplayItemsByTranscriptMode } from "./transcript-mode.js";

const hookSubtypes = [
  "hook_success",
  "hook_additional_context",
  "hook_cancelled",
  "stop_hook_summary",
];

const hook = (subtype = "hook_success") =>
  msg({
    ordinal: 2,
    is_system: true,
    source_type: subtype === "stop_hook_summary" ? "system" : "attachment",
    source_subtype: subtype,
    content: "Hook name: Example\n\nStdout:\n  中文 e\u0301 <script>literal</script>\n",
  });

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

it("keeps hook errors hidden and continuation boundaries visible under system", () => {
  expect(isSystemMessage({ ...hook(), source_subtype: "hook_error" })).toBe(true);
  const boundary = msg({
    ordinal: 4,
    is_system: true,
    source_type: "system",
    source_subtype: "continuation",
  });
  expect(hasVisibleSegments(boundary, (t) => t === "system")).toBe(true);
  expect(hasVisibleSegments(boundary, (t) => t === "hook")).toBe(false);
});

describe.each(hookSubtypes)("%s output", (subtype) => {
  it("promotes hook output", () => {
    expect(isSystemMessage(hook(subtype))).toBe(false);
    expect(isHookSuccessMessage(hook(subtype))).toBe(true);
  });

  it("uses a localized label", () => {
    const content = hook(subtype).content;
    component = mount(SystemBoundaryCard, {
      target: document.body,
      props: { subtype, content, timestamp: "" },
    });
    expect(document.querySelector(".label")?.textContent).toBe(
      subtype === "stop_hook_summary"
        ? "Stop hook summary"
        : subtype === "hook_cancelled"
          ? "Hook cancelled"
          : "Hook output",
    );
  });
});

it("does not finish a turn or promote an intermediate reply in focused mode", () => {
  const items = buildDisplayItems([
    msg({ ordinal: 0 }),
    msg({ ordinal: 1, role: "assistant", content: "intermediate" }),
    hook(),
    msg({ ordinal: 3, role: "assistant", content: "final" }),
    msg({ ordinal: 4 }),
  ]);
  expect(
    filterDisplayItemsByTranscriptMode(items, "focused").flatMap((item) => item.ordinals),
  ).toEqual([0, 3, 4]);
});

it("uses a collapsed card and preserves escaped body text", () => {
  const content = hook().content;
  component = mount(SystemBoundaryCard, {
    target: document.body,
    props: { subtype: "hook_success", content, timestamp: "" },
  });
  const details = document.querySelector("details");
  expect(details).not.toBeNull();
  expect(details?.open).toBe(false);
  expect(details?.querySelector("pre")?.textContent).toBe(content);
  expect(document.querySelector("script")).toBeNull();
});

describe("hook filters", () => {
  it("belongs to the Hooks filter independently of the role and system filters", () => {
    expect(hasVisibleSegments(hook(), (t) => t === "hook")).toBe(true);
    expect(hasVisibleSegments(hook(), (t) => t !== "hook")).toBe(false);
  });

  it("uses the same filter for projected transcript rows", () => {
    const rows = [
      msg({ ordinal: 0 }),
      hook(),
      msg({ ordinal: 3, role: "assistant", content: "final" }),
    ];
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
});
