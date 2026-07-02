// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
import { messages } from "../../stores/messages.svelte.js";
import { setLocale } from "../../i18n/index.js";
// @ts-ignore
import SessionFindBar from "./SessionFindBar.svelte";

afterEach(() => {
  setLocale("en");
  inSessionSearch.close();
  messages.clear();
  document.body.innerHTML = "";
});

describe("SessionFindBar", () => {
  it("renders search controls in Simplified Chinese", async () => {
    setLocale("zh-CN");
    messages.sessionId = "s1";
    messages.messageCount = 10;
    inSessionSearch.isOpen = true;
    inSessionSearch.query = "missing";
    inSessionSearch.matches = [];
    inSessionSearch.loading = false;

    const component = mount(SessionFindBar, {
      target: document.body,
    });
    await tick();
    inSessionSearch.loading = false;
    await tick();

    expect(
      document.querySelector('[role="search"]')?.getAttribute("aria-label"),
    ).toBe("在会话中查找");
    expect(
      document
        .querySelector<HTMLInputElement>(".kit-find-bar__input")
        ?.getAttribute("placeholder"),
    ).toBe("在会话中查找...");
    expect(
      document
        .querySelector<HTMLInputElement>(".kit-find-bar__input")
        ?.getAttribute("aria-label"),
    ).toBe("搜索关键词");
    expect(document.querySelector(".kit-find-bar__counter")?.textContent?.trim()).toBe(
      "无结果",
    );
    expect(
      document
        .querySelector<HTMLButtonElement>(".kit-find-bar__nav-btn")
        ?.getAttribute("aria-label"),
    ).toBe("上一个匹配项");
    expect(
      document
        .querySelector<HTMLButtonElement>(".kit-find-bar__close")
        ?.getAttribute("aria-label"),
    ).toBe("关闭查找栏");

    unmount(component);
  });
});
