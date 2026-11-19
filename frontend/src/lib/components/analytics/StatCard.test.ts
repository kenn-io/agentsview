// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import StatCard from "./StatCard.svelte";

let component: ReturnType<typeof mount> | null = null;

async function render(props: Record<string, unknown>): Promise<HTMLElement> {
  component = mount(StatCard, { target: document.body, props: props as never });
  await tick();
  return document.body.querySelector<HTMLElement>(".card")!;
}

describe("StatCard", () => {
  afterEach(() => {
    if (component) unmount(component);
    component = null;
    document.body.innerHTML = "";
  });

  it("puts the value above the label and keeps the full sublabel in its title", async () => {
    const sub = "12 interactive / 3 subagents / 2 automated, 1 untimed";
    const card = await render({ value: "18", label: "Sessions", sub });
    const parts = Array.from(card.querySelectorAll("span")).map((el) => el.className.split(" ")[0]);
    expect(parts).toEqual(["card-value", "card-label", "card-sub"]);
    const subEl = card.querySelector(".card-sub")!;
    expect(subEl.textContent).toBe(sub);
    expect(subEl.getAttribute("title")).toBe(sub);
  });

  it("shows a placeholder and drops the sublabel on error", async () => {
    const card = await render({ value: "42", label: "Sessions", sub: "median 3", error: true });
    expect(card.querySelector(".card-value")?.textContent).toBe("--");
    expect(card.querySelector(".card-label")?.textContent).toBe("Sessions");
    expect(card.querySelector(".card-sub")).toBeNull();
  });
});
