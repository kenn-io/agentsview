// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";

const mocks = vi.hoisted(() => ({ getApiV1RecallEntries: vi.fn() }));

vi.mock("../../api/generated/index.js", () => ({
  RecallService: { getApiV1RecallEntries: mocks.getApiV1RecallEntries },
}));

import { ApiError } from "../../api/runtime.js";
// @ts-ignore
import RecallPanel from "./RecallPanel.svelte";

async function flush() {
  for (let i = 0; i < 3; i++) {
    await Promise.resolve();
    await tick();
  }
}

describe("RecallPanel load failures", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    mocks.getApiV1RecallEntries.mockReset();
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
  });

  function stateLine(): HTMLElement {
    const state = document.querySelector<HTMLElement>(".recall-state");
    expect(state?.textContent).toBe("Could not load distilled recall.");
    return state!;
  }

  it("shows a read-only backend's 501 as unavailable, not as an error", async () => {
    mocks.getApiV1RecallEntries.mockRejectedValue(
      new ApiError(501, "not available in remote mode"),
    );
    component = mount(RecallPanel, { target: document.body, props: { sessionId: "s1" } });
    await flush();

    expect(stateLine().classList.contains("recall-error")).toBe(false);
  });

  it("keeps the error treatment for a genuine server failure", async () => {
    mocks.getApiV1RecallEntries.mockRejectedValue(new ApiError(500, "boom"));
    component = mount(RecallPanel, { target: document.body, props: { sessionId: "s1" } });
    await flush();

    expect(stateLine().classList.contains("recall-error")).toBe(true);
  });
});
