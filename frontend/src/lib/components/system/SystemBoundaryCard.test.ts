// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, unmount } from "svelte";
// @ts-ignore
import SystemBoundaryCard from "./SystemBoundaryCard.svelte";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("SystemBoundaryCard", () => {
  it("renders a human label for a known subtype", () => {
    const c = mount(SystemBoundaryCard, {
      target: document.body,
      props: {
        subtype: "continuation",
        content: "This session is being continued from...",
        timestamp: "2026-04-18T12:00:00Z",
      },
    });
    expect(document.body.textContent).toMatch(/Session continuation/);
    unmount(c);
  });

  it("falls back to the raw subtype when unknown", () => {
    const c = mount(SystemBoundaryCard, {
      target: document.body,
      props: {
        subtype: "future_unknown_subtype",
        content: "anything",
        timestamp: "2026-04-18T12:00:00Z",
      },
    });
    expect(document.body.textContent).toMatch(/future_unknown_subtype/);
    unmount(c);
  });

  it("shows the content preview as collapsed details", () => {
    const c = mount(SystemBoundaryCard, {
      target: document.body,
      props: {
        subtype: "stop_hook",
        content: "line one\nline two\nline three",
        timestamp: "2026-04-18T12:00:00Z",
      },
    });
    // <details> contains the full content; hidden until expanded.
    const details = document.body.querySelector("details");
    expect(details).toBeTruthy();
    expect(details?.querySelector("pre")?.textContent).toBe(
      "line one\nline two\nline three",
    );
    unmount(c);
  });
});
