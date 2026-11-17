// @vitest-environment jsdom
import { expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { pins } from "../../stores/pins.svelte.js";
import PinnedPage from "./PinnedPage.svelte";

const mocks = vi.hoisted(() => ({ getApiV1Pins: vi.fn() }));

vi.mock("../../api/generated/index", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...actual,
    PinsService: { ...actual.PinsService, getApiV1Pins: mocks.getApiV1Pins },
  };
});

it("shows a failed load with Retry instead of the empty state", async () => {
  mocks.getApiV1Pins
    .mockRejectedValueOnce(new Error("pins table is being rebuilt"))
    .mockResolvedValueOnce({
      pins: [
        {
          id: 2,
          session_id: "session-2",
          message_id: 20,
          ordinal: 3,
          created_at: "2026-01-01T00:00:00Z",
          content: "Pinned after retry",
          role: "assistant",
        },
      ],
    });
  const component = mount(PinnedPage, { target: document.body });
  try {
    const alert = await vi.waitFor(() => {
      const el = document.querySelector('[role="alert"]');
      expect(el).not.toBeNull();
      return el!;
    });
    expect(alert.textContent).toContain("Failed to load");
    expect(alert.textContent).toContain("pins table is being rebuilt");
    expect(document.body.textContent).not.toContain("No pinned messages");

    alert.querySelector<HTMLButtonElement>("button")!.click();
    await vi.waitFor(() => expect(document.body.textContent).toContain("Pinned after retry"));
    expect(document.querySelector('[role="alert"]')).toBeNull();
  } finally {
    await unmount(component);
    pins.pins = [];
    pins.error = null;
    mocks.getApiV1Pins.mockReset();
    document.body.replaceChildren();
  }
});

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
