// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import Treemap from "./Treemap.svelte";
import { ui } from "../../stores/ui.svelte.js";

describe("Treemap", () => {
  afterEach(() => {
    setLocale("en");
    document.body.innerHTML = "";
  });

  it("keeps the localized tile hover title", async () => {
    setLocale("en");
    const component = mount(Treemap, {
      target: document.body,
      props: {
        items: [
          {
            id: "alpha",
            label: "Alpha",
            value: 42,
            style: { color: "#1f77b4", fill: "solid" as const },
            meta: "Meta",
          },
        ],
      },
    });
    await tick();

    expect(document.querySelector(".tile title")?.textContent).toBe("Click to focus Alpha");
    const tile = document.querySelector<SVGGElement>(".tile");
    expect(tile?.hasAttribute("role")).toBe(false);
    expect(tile?.hasAttribute("tabindex")).toBe(false);
    expect(tile?.hasAttribute("aria-pressed")).toBe(false);
    // A role-less <g> takes its name from <title>; an aria-label there is not exposed reliably.
    expect(tile?.hasAttribute("aria-label")).toBe(false);
    const clipPath = tile?.getAttribute("clip-path");
    expect(typeof clipPath).toBe("string");
    if (typeof clipPath !== "string") {
      unmount(component);
      return;
    }
    expect(clipPath).toMatch(/^url\(#.+\)$/);
    const clipId = clipPath.slice(5, -1);
    expect(document.getElementById(clipId)?.querySelector("rect")).not.toBeNull();

    unmount(component);
  });
  it("gives variable palette colors readable tile ink", async () => {
    document.documentElement.style.setProperty("--test-series-lime", "#a3e635");
    document.documentElement.style.setProperty("--test-series-navy", "#1f3a8a");
    const component = mount(Treemap, {
      target: document.body,
      props: {
        items: [
          { id: "lime", label: "Lime", value: 60, style: { color: "var(--test-series-lime)", fill: "solid" as const } },
          { id: "navy", label: "Navy", value: 40, style: { color: "var(--test-series-navy)", fill: "solid" as const } },
        ],
      },
    });
    await tick();

    const tiles = [...document.querySelectorAll<SVGGElement>(".tile")];
    const ink = tiles.map((tile) => tile.style.getPropertyValue("--tile-ink"));
    const fill = tiles.map((tile) => tile.style.getPropertyValue("--tile-fill"));
    // Bright lime takes dark text; dark navy takes white text.
    expect(ink).toEqual(["#111318", "#ffffff"]);
    // Each fill moves away from its ink: lime lightens, navy darkens.
    expect(fill[0]).not.toBe("#a3e635");
    expect(fill[1]).not.toBe("#1f3a8a");

    unmount(component);
    document.documentElement.style.removeProperty("--test-series-lime");
    document.documentElement.style.removeProperty("--test-series-navy");
  });

  it("repaints mounted tiles when the theme changes", async () => {
    const root = document.documentElement.style;
    const startTheme = ui.theme;
    root.setProperty("--test-series-swap", "#a3e635");
    const component = mount(Treemap, {
      target: document.body,
      props: {
        items: [{ id: "swap", label: "Swap", value: 1, style: { color: "var(--test-series-swap)", fill: "solid" as const } }],
      },
    });
    await tick();
    const tile = () => document.querySelector<SVGGElement>(".tile")!;
    expect(tile().style.getPropertyValue("--tile-ink")).toBe("#111318");

    // A theme switch changes what the variable resolves to; the same mounted
    // items must pick up the new color without being replaced.
    root.setProperty("--test-series-swap", "#1f3a8a");
    ui.toggleTheme();
    await tick();
    expect(tile().style.getPropertyValue("--tile-ink")).toBe("#ffffff");

    root.setProperty("--test-series-swap", "#a3e635");
    ui.highContrast = !ui.highContrast;
    await tick();
    expect(tile().style.getPropertyValue("--tile-ink")).toBe("#111318");

    unmount(component);
    ui.highContrast = !ui.highContrast;
    if (ui.theme !== startTheme) ui.toggleTheme();
    root.removeProperty("--test-series-swap");
  });

  it("selects with Enter and opens on double click", async () => {
    const onSelect = vi.fn();
    const onOpen = vi.fn();
    const items = [
      {
        id: "alpha",
        label: "Alpha",
        value: 42,
        style: { color: "#1f77b4", fill: "solid" as const },
        selected: true,
      },
      {
        id: "beta",
        label: "Beta",
        value: 21,
        style: { color: "#ff7f0e", fill: "solid" as const },
        dimmed: true,
      },
    ];
    const component = mount(Treemap, { target: document.body, props: { items, onSelect } });
    await tick();
    const tiles = document.querySelectorAll(".tile");
    expect(tiles).toHaveLength(2);
    expect(tiles[0]!.getAttribute("aria-pressed")).toBe("true");
    expect(tiles[1]!.classList.contains("dimmed")).toBe(true);
    expect(tiles[1]!.querySelector("rect")!.getAttribute("fill")).toBe("#ff7f0e");
    tiles[1]!.dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }),
    );
    expect(onSelect).toHaveBeenCalledWith("beta");
    tiles[0]!.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
    await unmount(component);
    const project = mount(Treemap, { target: document.body, props: { items, onSelect, onOpen } });
    await tick();
    const tile = document.querySelector(".tile")!;
    tile.dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }),
    );
    expect(onOpen).not.toHaveBeenCalled();
    tile.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
    tile.dispatchEvent(new MouseEvent("dblclick", { detail: 2, bubbles: true }));
    expect(onOpen).toHaveBeenCalledWith("alpha");
    expect(onSelect.mock.calls).toEqual([["beta"], ["alpha"], ["alpha"]]);
    await unmount(project);
  });
  it("sizes labels by tile share on the chosen scale", async () => {
    const items = [
      {
        id: "big",
        label: "Big",
        value: 80,
        style: { color: "#1f77b4", fill: "solid" as const },
      },
      {
        id: "small",
        label: "Small",
        value: 20,
        style: { color: "#ff7f0e", fill: "solid" as const },
      },
    ];
    const labelSizes = () =>
      [...document.querySelectorAll<SVGTextElement>(".tile-label")].map((label) =>
        parseFloat(label.style.fontSize),
      );

    const linear = mount(Treemap, {
      target: document.body,
      props: { items, labelScale: "linear" },
    });
    await tick();
    expect(labelSizes()).toEqual([17, 12]);
    unmount(linear);

    const log = mount(Treemap, { target: document.body, props: { items, labelScale: "log" } });
    await tick();
    expect(labelSizes()).toEqual([18, 16]);
    unmount(log);
  });
});
