// @vitest-environment jsdom
import { describe, it, expect } from "vite-plus/test";
import { mount, unmount, tick } from "svelte";
import type { Virtualizer } from "@tanstack/virtual-core";
// @ts-ignore
import VirtualizerTest from "./VirtualizerTest.svelte";

// Runs the real TanStack Virtualizer, which calls onChange only when the
// visible range moves. Rows added below the viewport do not move it, so the
// wrapper has to tell its readers about the new layout itself.
describe("createVirtualizer layout after an options update", () => {
  it.each(["element", "window"] as const)(
    "shows rows added below the viewport without a scroll (%s virtualizer)",
    async (type) => {
      const scrollEl = document.createElement("div");
      const options = (count: number) => ({
        count,
        estimateSize: () => 50,
        ...(type === "element" ? { getScrollElement: () => scrollEl } : {}),
      });
      const renderedTotals: number[] = [];
      const component = mount(VirtualizerTest, {
        target: document.createElement("div"),
        props: {
          type,
          options: options(10),
          onInstanceChange: (inst: Virtualizer<Element | Window, Element> | undefined) => {
            if (inst) renderedTotals.push(inst.getTotalSize());
          },
        },
      });
      await tick();
      expect(renderedTotals.at(-1)).toBe(500);

      component.setOptions(options(14));
      await tick();

      expect(renderedTotals.at(-1)).toBe(700);
      await unmount(component);
    },
  );
});
