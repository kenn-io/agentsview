// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import UsageModePicker from "./UsageModePicker.svelte";

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  document.body.innerHTML = "";
});

describe("UsageModePicker", () => {
  it("renders the selected metric and reports changes", async () => {
    setLocale("en");
    const onChange = vi.fn();
    component = mount(UsageModePicker, {
      target: document.body,
      props: { value: "cost", onChange },
    });
    await tick();

    const group = document.querySelector('[role="radiogroup"][aria-label="Usage metric"]');
    expect(group).not.toBeNull();
    const radios = Array.from(group!.querySelectorAll<HTMLButtonElement>('[role="radio"]'));
    expect(radios.map((radio) => radio.textContent?.trim())).toEqual(["Cost", "Tokens"]);
    expect(radios[0]?.getAttribute("aria-checked")).toBe("true");

    radios[1]!.click();
    await tick();

    expect(onChange).toHaveBeenCalledWith("token");
  });
});
