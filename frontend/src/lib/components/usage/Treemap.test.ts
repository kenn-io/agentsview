// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import Treemap from "./Treemap.svelte";

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
            color: "#1f77b4",
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
  it("selects with Enter and opens on double click", async () => {
    const onSelect = vi.fn();
    const onOpen = vi.fn();
    const items = [
      { id: "alpha", label: "Alpha", value: 42, color: "#1f77b4", selected: true },
      { id: "beta", label: "Beta", value: 21, color: "#ff7f0e", dimmed: true },
    ];
    const component = mount(Treemap, { target: document.body, props: { items, onSelect } });
    await tick();
    const tiles = document.querySelectorAll(".tile");
    expect(tiles).toHaveLength(2);
    expect(tiles[0]!.getAttribute("aria-pressed")).toBe("true");
    expect(tiles[1]!.classList.contains("dimmed")).toBe(true);
    expect(tiles[1]!.querySelector("rect")!.getAttribute("fill")).toBe("#ff7f0e");
    tiles[1]!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    expect(onSelect).toHaveBeenCalledWith("beta");
    tiles[0]!.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
    await unmount(component);
    const project = mount(Treemap, { target: document.body, props: { items, onSelect, onOpen } });
    await tick();
    const tile = document.querySelector(".tile")!;
    tile.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    expect(onOpen).not.toHaveBeenCalled();
    tile.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
    tile.dispatchEvent(new MouseEvent("dblclick", { detail: 2, bubbles: true }));
    expect(onOpen).toHaveBeenCalledWith("alpha");
    expect(onSelect.mock.calls).toEqual([["beta"], ["alpha"], ["alpha"]]);
    await unmount(project);
  });

});
