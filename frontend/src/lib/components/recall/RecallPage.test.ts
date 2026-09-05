// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { setLocale } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import { settings } from "../../stores/settings.svelte.js";
import { sync } from "../../stores/sync.svelte.js";

// @ts-ignore
import RecallPage from "./RecallPage.svelte";

describe("RecallPage", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    setLocale("en");
    router.route = "recall";
    router.params = {};
    sync.serverVersion = {
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    settings.loaded = true;
    settings.readOnly = false;
    settings.error = null;
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    router.params = {};
    sync.serverVersion = null;
    settings.loaded = false;
    settings.readOnly = false;
    settings.error = null;
  });

  it("switches between URL-backed Corpus and Generated insights tabs", async () => {
    component = mount(RecallPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Corpus");
    const generated = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Generated insights",
    );
    expect(generated).toBeDefined();

    generated!.click();
    await tick();

    expect(router.params.tab).toBe("generated");
    expect(document.querySelector(".generated-insights-panel")).not.toBeNull();
  });

  it("treats an insight parameter as a Generated insights deep link", async () => {
    router.params = { insight: "not-a-number" };
    component = mount(RecallPage, { target: document.body });
    await tick();

    expect(document.querySelector(".generated-insights-panel")).not.toBeNull();
  });

  it("shows only Generated insights for a read-only backend", async () => {
    sync.serverVersion = {
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: true,
    };
    component = mount(RecallPage, { target: document.body });
    await tick();

    expect(document.body.textContent).not.toContain("Corpus");
    expect(document.querySelector(".generated-insights-panel")).not.toBeNull();
  });

  it("waits for backend capability before mounting either panel", async () => {
    sync.serverVersion = null;
    settings.loaded = false;
    component = mount(RecallPage, { target: document.body });
    await tick();

    expect(document.querySelector(".recall-corpus-panel")).toBeNull();
    expect(document.querySelector(".generated-insights-panel")).toBeNull();
    expect(document.body.textContent).toContain("Loading Recall entries");
  });
});
