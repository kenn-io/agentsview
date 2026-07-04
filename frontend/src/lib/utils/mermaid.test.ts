// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vite-plus/test";

const initializeMock = vi.hoisted(() => vi.fn());
const renderMock = vi.hoisted(() => vi.fn());

vi.mock("mermaid", () => ({
  default: {
    initialize: initializeMock,
    render: renderMock,
  },
}));

beforeEach(() => {
  vi.resetModules();
  initializeMock.mockClear();
  renderMock.mockReset();
});

describe("renderMermaid", () => {
  it("initializes Mermaid with SVG-safe labels", async () => {
    renderMock.mockResolvedValueOnce({
      svg: "<svg><text>Diagram</text></svg>",
    });

    const { renderMermaid } = await import("./mermaid.js");
    const result = await renderMermaid("graph TD\nA-->B\n");

    expect(result.ok).toBe(true);
    expect(initializeMock).toHaveBeenCalledWith({
      startOnLoad: false,
      securityLevel: "strict",
      htmlLabels: false,
    });
  });

  it("sanitizes returned SVG before exposing it to components", async () => {
    renderMock.mockResolvedValueOnce({
      svg: '<svg><script>alert(1)</script><g onclick="alert(2)"><text>Safe label</text></g></svg>',
    });

    const { renderMermaid } = await import("./mermaid.js");
    const result = await renderMermaid("graph TD\nA-->B\n");

    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.svg).toContain("<svg");
    expect(result.svg).toContain("Safe label");
    expect(result.svg).not.toContain("<script");
    expect(result.svg).not.toContain("onclick");
  });

  it("returns a sanitize error for non-SVG render output", async () => {
    renderMock.mockResolvedValueOnce({
      svg: "<span>not a diagram</span>",
    });

    const { renderMermaid } = await import("./mermaid.js");
    const result = await renderMermaid("graph TD\nA-->B\n");

    expect(result).toEqual({
      ok: false,
      error: {
        kind: "sanitize",
        message: "Mermaid returned an empty or invalid SVG.",
      },
    });
  });
});
