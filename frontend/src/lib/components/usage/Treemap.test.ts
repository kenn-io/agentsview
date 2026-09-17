// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import Treemap from "./Treemap.svelte";
import { costDisplay } from "../../stores/costDisplay.svelte.js";

describe("Treemap", () => {
  beforeEach(() => {
    costDisplay.setPreference("USD", null);
  });

  afterEach(() => {
    costDisplay.setPreference("USD", null);
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

    expect(document.querySelector(".tile title")?.textContent).toBe("Click to hide Alpha");
    const tile = document.querySelector<SVGGElement>(".tile");
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

  it("refreshes a default cost tile when the display rate changes", async () => {
    const component = mount(Treemap, {
      target: document.body,
      props: {
        items: [
          {
            id: "alpha",
            label: "Alpha",
            value: 10_000_000,
            color: "#1f77b4",
          },
        ],
      },
    });
    await tick();

    const value = document.querySelector(".tile-value");
    expect(value?.textContent).toBe("$10.00");
    const rect = document.querySelector<SVGRectElement>(".tile rect");
    const height = rect?.getAttribute("height");

    costDisplay.setPreference("EUR", 0.9);
    await tick();

    expect(value?.textContent).toBe("€9.00");
    expect(rect?.getAttribute("height")).toBe(height);
    unmount(component);
  });
});
