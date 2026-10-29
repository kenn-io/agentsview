// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import type { DbMessage as Message } from "../../api/generated/index.js";
// @ts-ignore
import EmptyMessagesRow from "./EmptyMessagesRow.svelte";

// The details toggle event fires in a later task.
async function toggled() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

afterEach(() => {
  document.body.innerHTML = "";
});

function emptyMessage(id: number, ordinal: number, role: string): Message {
  return {
    has_context_tokens: false,
    has_output_tokens: false,
    id,
    session_id: "session-1",
    ordinal,
    role,
    content: "",
    timestamp: "2026-10-25T10:12:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: 0,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
  };
}

describe("EmptyMessagesRow", () => {
  it("shows only the count while closed and asks to open when clicked", async () => {
    const onToggle = vi.fn();
    const c = mount(EmptyMessagesRow, {
      target: document.body,
      props: {
        messages: [emptyMessage(1, 4, "assistant"), emptyMessage(2, 5, "user")],
        open: false,
        onToggle,
      },
    });
    const summary = document.querySelector("summary")!;
    expect(summary.textContent).toContain("2 empty messages");
    expect(document.querySelectorAll(".role-label")).toHaveLength(0);

    summary.click();
    await toggled();
    expect(onToggle).toHaveBeenCalledWith(true);
    unmount(c);
  });

  it("shows the usual message cards while open and asks to close when clicked", async () => {
    const onToggle = vi.fn();
    const c = mount(EmptyMessagesRow, {
      target: document.body,
      props: {
        messages: [emptyMessage(1, 4, "assistant"), emptyMessage(2, 5, "user")],
        open: true,
        onToggle,
      },
    });
    const labels = [...document.querySelectorAll(".role-label")].map((el) => el.textContent);
    expect(labels).toEqual(["Assistant", "User"]);

    document.querySelector("summary")!.click();
    await toggled();
    expect(onToggle).toHaveBeenCalledWith(false);
    unmount(c);
  });
});
