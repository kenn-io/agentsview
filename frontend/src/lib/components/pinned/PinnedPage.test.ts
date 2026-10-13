// @vitest-environment jsdom
import { expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { pins } from "../../stores/pins.svelte.js";
import PinnedPage from "./PinnedPage.svelte";

it("shows an unpin failure for the clicked card", async () => {
  vi.spyOn(pins, "loadAll").mockResolvedValue(undefined);
  const unpin = vi
    .spyOn(pins, "unpin")
    .mockRejectedValue(new Error("archive is read-only while it rebuilds"));
  pins.pins = [
    {
      id: 1,
      session_id: "session-1",
      message_id: 10,
      ordinal: 0,
      created_at: "2026-01-01T00:00:00Z",
      content: "Retained pin",
      role: "user",
    },
  ];
  const component = mount(PinnedPage, { target: document.body });
  try {
    await tick();
    const card = document.querySelector(".pin-card")!;
    card.querySelector<HTMLButtonElement>(".unpin-btn")!.click();
    await Promise.resolve();
    await tick();
    expect(card.querySelector('[role="alert"]')?.textContent).toBe(
      "archive is read-only while it rebuilds",
    );
    expect(unpin).toHaveBeenCalledExactlyOnceWith("session-1", 10);
  } finally {
    await unmount(component);
    pins.pins = [];
    vi.restoreAllMocks();
    document.body.replaceChildren();
  }
});
