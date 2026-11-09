// @vitest-environment jsdom
import { afterEach, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { makeSessionShape } from "../../test-support/analytics.js";
import SessionShape from "./SessionShape.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  analytics.sessionShape = null;
  analytics.errors.sessionShape = null;
  document.body.innerHTML = "";
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
