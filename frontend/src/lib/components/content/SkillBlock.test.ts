// @vitest-environment jsdom
import { mount, unmount, tick } from "svelte";
import { afterEach, describe, expect, it } from "vite-plus/test";
import { setLocale } from "../../i18n/index.js";

// @ts-ignore
import SkillBlock from "./SkillBlock.svelte";

describe("SkillBlock", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    setLocale("en");
  });

  it("localizes the label fallback", async () => {
    setLocale("zh-CN");

    component = mount(SkillBlock, {
      target: document.body,
      props: { content: "Use the project guidance." },
    });
    await tick();

    expect(document.querySelector(".skill-label")?.textContent).toBe("技能：未知");
  });
});
