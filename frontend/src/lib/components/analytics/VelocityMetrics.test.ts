// @vitest-environment jsdom
import { afterEach, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { makeVelocity } from "../../test-support/analytics.js";
import VelocityMetrics from "./VelocityMetrics.svelte";

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  analytics.velocity = null;
  analytics.errors.velocity = null;
  document.body.innerHTML = "";
});

it("distinguishes loading, accepted results, and failures with or without results", async () => {
  analytics.velocity = null;
  analytics.errors.velocity = null;
  component = mount(VelocityMetrics, { target: document.body });
  await tick();
  expect(document.querySelector(".loading")).not.toBeNull();
  analytics.velocity = makeVelocity();
  await tick();
  expect(document.querySelector(".metrics-grid")).not.toBeNull();
  analytics.errors.velocity = "request failed";
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.querySelector(".metrics-grid")).not.toBeNull();
  analytics.velocity = null;
  await tick();
  expect(document.querySelector(".error")).not.toBeNull();
  expect(document.querySelector(".metrics-grid")).toBeNull();
});
