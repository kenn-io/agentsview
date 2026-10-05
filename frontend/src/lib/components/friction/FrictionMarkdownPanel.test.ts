// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";

const mocks = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  downloadFrictionDigestMarkdown: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("../../utils/clipboard.js", () => ({ copyToClipboard: mocks.copyToClipboard }));
vi.mock("../../api/client.js", () => ({
  downloadFrictionDigestMarkdown: mocks.downloadFrictionDigestMarkdown,
}));

import { friction } from "../../stores/friction.svelte.js";
// @ts-ignore
import FrictionMarkdownPanel from "./FrictionMarkdownPanel.svelte";

const MARKDOWN =
  "---\ndate: 2026-09-21\n---\n\n# Friction Log — 2026-09-21\n\n## P0 Alerts\n\n_No P0 alerts._\n\n";

describe("FrictionMarkdownPanel", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    friction.reset();
    friction.selectedDate = "2026-09-21";
    vi.clearAllMocks();
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  function button(label: string): HTMLButtonElement {
    const found = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (b) => b.textContent?.trim() === label || b.getAttribute("aria-label") === label,
    );
    expect(found, `button ${label}`).toBeDefined();
    return found!;
  }

  it("loads the Markdown on demand", async () => {
    const load = vi.spyOn(friction, "loadMarkdown").mockResolvedValue();
    component = mount(FrictionMarkdownPanel, {
      target: document.body,
      props: { date: "2026-09-21" },
    });
    await tick();
    expect(document.querySelector("pre.markdown-source")).toBeNull();
    button("Show Markdown").click();
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("shows the exact stored bytes as text and copies them", async () => {
    friction.markdown = MARKDOWN;
    component = mount(FrictionMarkdownPanel, {
      target: document.body,
      props: { date: "2026-09-21" },
    });
    await tick();
    expect(document.querySelector("pre.markdown-source")?.textContent).toBe(MARKDOWN);
    button("Copy Markdown").click();
    await tick();
    expect(mocks.copyToClipboard).toHaveBeenCalledWith(MARKDOWN);
  });

  it("downloads through the authenticated export helper", async () => {
    component = mount(FrictionMarkdownPanel, {
      target: document.body,
      props: { date: "2026-09-21" },
    });
    await tick();
    button("Download Markdown").click();
    await tick();
    expect(mocks.downloadFrictionDigestMarkdown).toHaveBeenCalledWith("2026-09-21");
  });

  it("shows the load error as an alert", async () => {
    friction.errors.markdown = "Could not load the Markdown digest.";
    component = mount(FrictionMarkdownPanel, {
      target: document.body,
      props: { date: "2026-09-21" },
    });
    await tick();
    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "Could not load the Markdown digest.",
    );
  });
});
