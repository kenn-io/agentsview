// @vitest-environment jsdom
import { describe, expect, it, vi } from "vite-plus/test";
import { mount, unmount } from "svelte";
// @ts-ignore
import OptionTypeahead from "./OptionTypeahead.svelte";

describe("OptionTypeahead", () => {
  it("keeps a disabled trigger closed", () => {
    const onselect = vi.fn();
    const component = mount(OptionTypeahead, {
      target: document.body,
      props: {
        options: [{ name: "codex", label: "Codex" }],
        value: "codex",
        fallbackLabel: "Codex",
        placeholder: "Pick agent",
        title: "Agent",
        emptyLabel: "No agents",
        disabled: true,
        onselect,
      },
    });

    const trigger = document.querySelector<HTMLButtonElement>(".typeahead-trigger");
    expect(trigger).not.toBeNull();
    expect(trigger!.disabled).toBe(true);

    trigger!.click();

    expect(document.querySelector(".typeahead-input")).toBeNull();
    expect(onselect).not.toHaveBeenCalled();

    void unmount(component);
  });
});
