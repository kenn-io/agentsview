// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
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

    expect(document.querySelector(".tile title")?.textContent).toBe(
      "Click to hide Alpha",
    );

    unmount(component);
  });
});
