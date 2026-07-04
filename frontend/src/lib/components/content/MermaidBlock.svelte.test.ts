// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import MermaidBlock from "./MermaidBlock.svelte";
import { m } from "../../i18n/index.js";

const renderMermaidMock = vi.hoisted(() => vi.fn());

vi.mock("../../utils/mermaid.js", () => ({
  renderMermaid: renderMermaidMock,
}));

afterEach(() => {
  document.body.innerHTML = "";
  vi.clearAllMocks();
});

describe("MermaidBlock", () => {
  it("shows a bounded pending state until the diagram renders", async () => {
    let resolveRender:
      | ((value: { ok: true; svg: string }) => void)
      | undefined;
    renderMermaidMock.mockImplementationOnce(
      () =>
        new Promise<{ ok: true; svg: string }>((resolve) => {
          resolveRender = resolve;
        }),
    );

    const component = mount(MermaidBlock, {
      target: document.body,
      props: {
        content: "graph TD\nA-->B\n",
      },
    });

    await tick();

    const pending = document.querySelector(".mermaid-pending");
    expect(pending?.textContent?.trim()).toBe(
      m.mermaid_render_pending(),
    );

    resolveRender?.({
      ok: true,
      svg: '<svg data-testid="mermaid-diagram"></svg>',
    });

    await tick();
    await tick();

    expect(document.querySelector('[data-testid="mermaid-diagram"]')).not.toBeNull();

    unmount(component);
  });

  it("falls back to readable source when rendering fails", async () => {
    renderMermaidMock.mockResolvedValueOnce({
      ok: false,
      error: {
        kind: "render",
        message: "Mermaid parse error",
      },
    });

    const source = "graph TD\nA--";
    const component = mount(MermaidBlock, {
      target: document.body,
      props: {
        content: source,
      },
    });

    await tick();
    await tick();

    expect(document.querySelector(".mermaid-fallback")).not.toBeNull();
    expect(document.body.textContent).toContain(source);
    expect(document.body.textContent).toContain(m.mermaid_render_failed());

    unmount(component);
  });
});
