// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { makeSessionShape } from "../../test-support/analytics.js";
import { router } from "../../stores/router.svelte.js";
import SessionShape from "./SessionShape.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  analytics.sessionShape = null;
  analytics.errors.sessionShape = null;
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

it("opens sessions in a message-length bucket from its button; empty buckets stay inert", async () => {
  const nav = vi.spyOn(router, "navigateToSessions").mockReturnValue(true);
  analytics.sessionShape = {
    ...makeSessionShape(),
    count: 3,
    length_distribution: [
      { label: "1-5", count: 3 },
      { label: "6-15", count: 0 },
    ],
  };
  component = mount(SessionShape, { target: document.body });
  await tick();

  const buttons = [...document.querySelectorAll<HTMLButtonElement>("button.bar-row")];
  expect(buttons).toHaveLength(1);
  expect(buttons[0]!.textContent).toContain("1-5");
  expect(document.querySelectorAll(".bar-row")).toHaveLength(2);
  buttons[0]!.click();
  expect(nav).toHaveBeenCalledExactlyOnceWith({ min_messages: "1", max_messages: "5" }, [
    "min_messages",
    "max_messages",
  ]);
});

it("distinguishes loading, accepted results, and failures with or without results", async () => {
  analytics.sessionShape = null;
  analytics.errors.sessionShape = null;
  component = mount(SessionShape, { target: document.body });
  await tick();
  expect(document.querySelector(".loading")).not.toBeNull();
  analytics.sessionShape = makeSessionShape();
  await tick();
  expect(document.body.textContent).toContain("No data for this period");
  analytics.errors.sessionShape = "request failed";
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.body.textContent).toContain("No data for this period");
  analytics.sessionShape = null;
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.body.textContent).not.toContain("No data for this period");
});
