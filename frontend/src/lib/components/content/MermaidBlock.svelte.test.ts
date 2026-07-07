// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, unmount } from "svelte";
import type { MarkdownMermaidAPI } from "@kenn-io/kit-ui/utils/markdown-mermaid";
import MermaidBlock from "./MermaidBlock.svelte";

// kit-ui's renderer reads the --mermaid-* palette (plus font tokens) from
// the root element at render time and throws when one is missing; the app
// gets them from @kenn-io/kit-ui/mermaid.css, which vitest does not load.
const MERMAID_THEME_VARS: Record<string, string> = {
  "--font-sans": "Inter, sans-serif",
  "--font-size-md": "13px",
  "--mermaid-bg": "#ffffff",
  "--mermaid-node-bg": "#ffffff",
  "--mermaid-node-text": "#24292f",
  "--mermaid-node-border": "#d0d7de",
  "--mermaid-cluster-bg": "#f6f8fa",
  "--mermaid-cluster-text": "#24292f",
  "--mermaid-cluster-border": "#d0d7de",
  "--mermaid-line": "#57606a",
  "--mermaid-text": "#24292f",
  "--mermaid-label-bg": "#ffffff",
  "--mermaid-label-text": "#24292f",
  "--mermaid-note-bg": "#fff8c5",
  "--mermaid-note-text": "#24292f",
  "--mermaid-note-border": "#d4a72c",
};

function fakeMermaidApi(
  run: MarkdownMermaidAPI["run"],
): MarkdownMermaidAPI {
  return {
    version: "11.15.0",
    initialize: vi.fn(),
    run: vi.fn(run),
  };
}

async function waitFor(
  predicate: () => boolean,
  timeoutMs = 1000,
): Promise<void> {
  const start = Date.now();
  while (!predicate()) {
    if (Date.now() - start > timeoutMs) {
      throw new Error("condition not met within timeout");
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

beforeEach(() => {
  for (const [name, value] of Object.entries(MERMAID_THEME_VARS)) {
    document.documentElement.style.setProperty(name, value);
  }
});

afterEach(() => {
  for (const name of Object.keys(MERMAID_THEME_VARS)) {
    document.documentElement.style.removeProperty(name);
  }
  document.body.innerHTML = "";
  vi.clearAllMocks();
});

describe("MermaidBlock", () => {
  it("emits the fence source as an escaped pre.mermaid block", () => {
    const component = mount(MermaidBlock, {
      target: document.body,
      props: {
        content: 'graph TD\nA["<img src=x onerror=alert(1)>"]-->B',
        // Never resolves: pin the pre-render state.
        mermaidOptions: { load: () => new Promise(() => {}) },
      },
    });
    flushSync();

    const pre = document.querySelector("pre.mermaid");
    expect(pre).not.toBeNull();
    expect(pre?.textContent).toBe(
      'graph TD\nA["<img src=x onerror=alert(1)>"]-->B',
    );
    expect(document.querySelector("img")).toBeNull();

    unmount(component);
  });

  it("renders the diagram into kit-ui's pan/zoom viewer", async () => {
    const api = fakeMermaidApi(async ({ nodes }) => {
      for (const node of Array.from(nodes)) {
        node.innerHTML = '<svg viewBox="0 0 120 60"><text>diagram</text></svg>';
      }
    });
    const component = mount(MermaidBlock, {
      target: document.body,
      props: {
        content: "graph TD\nA-->B",
        mermaidOptions: { load: async () => api },
      },
    });
    flushSync();

    await waitFor(
      () => document.querySelector("pre.mermaid.kit-mermaid-viewer") !== null,
    );

    const viewer = document.querySelector("pre.mermaid.kit-mermaid-viewer");
    expect(viewer?.querySelector("svg")).not.toBeNull();
    expect(
      viewer?.querySelector('button[aria-label="Copy Mermaid source"]'),
    ).not.toBeNull();
    expect(
      viewer?.querySelector(
        'button[aria-label="Open diagram in expanded view"]',
      ),
    ).not.toBeNull();
    expect(api.initialize).toHaveBeenCalled();

    unmount(component);
  });

  it("keeps the source readable when the mermaid runtime fails to load", async () => {
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    const source = "graph TD\nA-->B";
    const component = mount(MermaidBlock, {
      target: document.body,
      props: {
        content: source,
        mermaidOptions: {
          load: () => Promise.reject(new Error("chunk load failed")),
        },
      },
    });
    flushSync();

    await waitFor(() => consoleError.mock.calls.length > 0);

    const pre = document.querySelector("pre.mermaid");
    expect(pre?.textContent).toBe(source);
    expect(document.querySelector(".kit-mermaid-viewer")).toBeNull();

    consoleError.mockRestore();
    unmount(component);
  });
});
