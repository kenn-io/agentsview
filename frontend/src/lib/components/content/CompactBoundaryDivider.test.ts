// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { mount, unmount } from "svelte";
// @ts-ignore
import CompactBoundaryDivider from "./CompactBoundaryDivider.svelte";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("CompactBoundaryDivider", () => {
  it("shows an expandable full summary when the compact summary is truncated", () => {
    const content = [
      "This is a long compact summary that definitely exceeds one hundred and forty characters so the preview needs truncation before users can expand the full content.",
      "Second line with more detail.",
    ].join("\n");

    const c = mount(CompactBoundaryDivider, {
      target: document.body,
      props: {
        message: {
          id: 1,
          session_id: "session-1",
          ordinal: 1,
          role: "system",
          content,
          timestamp: "2026-04-29T12:00:00Z",
          has_thinking: false,
          thinking_text: "",
          has_tool_use: false,
          content_length: content.length,
          model: "",
          context_tokens: 0,
          output_tokens: 0,
          is_system: true,
          is_compact_boundary: true,
        },
      },
    });

    const details = document.body.querySelector("details");
    expect(details).toBeTruthy();
    expect(details?.querySelector("summary")?.textContent).toContain("Show full summary");
    expect(details?.querySelector("pre")?.textContent).toBe(content);

    unmount(c);
  });

  it("renders a plain preview for short single-line summaries", () => {
    const content = "Short summary.";

    const c = mount(CompactBoundaryDivider, {
      target: document.body,
      props: {
        message: {
          id: 1,
          session_id: "session-1",
          ordinal: 1,
          role: "system",
          content,
          timestamp: "2026-04-29T12:00:00Z",
          has_thinking: false,
          thinking_text: "",
          has_tool_use: false,
          content_length: content.length,
          model: "",
          context_tokens: 0,
          output_tokens: 0,
          is_system: true,
          is_compact_boundary: true,
        },
      },
    });

    expect(document.body.querySelector("details")).toBeNull();
    expect(document.body.textContent).toContain(content);

    unmount(c);
  });
});
