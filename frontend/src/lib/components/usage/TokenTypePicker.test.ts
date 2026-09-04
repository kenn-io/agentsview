// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import TokenTypePicker from "./TokenTypePicker.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  document.body.innerHTML = "";
});

describe("TokenTypePicker", () => {
  it("renders all token economics and reports combined selections", async () => {
    setLocale("en");
    const onChange = vi.fn();
    component = mount(TokenTypePicker, {
      target: document.body,
      props: { value: ["output"], onChange },
    });
    await tick();

    const trigger = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Token types: Output"]',
    );
    expect(trigger).not.toBeNull();
    trigger!.click();
    await tick();

    const items = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".kit-filter-dropdown__item"),
    );
    expect(items.map((item) => item.textContent?.trim())).toEqual([
      "Input",
      "Cache Writes",
      "Cached Read",
      "Output",
    ]);

    items[0]!.click();
    await tick();
    expect(onChange).toHaveBeenCalledWith(["input", "output"]);
  });

  it("does not allow the final token type to be deselected", async () => {
    setLocale("en");
    const onChange = vi.fn();
    component = mount(TokenTypePicker, {
      target: document.body,
      props: { value: ["output"], onChange },
    });
    await tick();

    document.querySelector<HTMLButtonElement>('button[aria-label="Token types: Output"]')!.click();
    await tick();
    const output = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".kit-filter-dropdown__item"),
    ).find((item) => item.textContent?.trim() === "Output");

    expect(output?.disabled).toBe(true);
    output?.click();
    expect(onChange).not.toHaveBeenCalled();
  });
});
