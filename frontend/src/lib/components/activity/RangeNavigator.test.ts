import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/svelte";

const store = vi.hoisted(() => ({
  activity: {
    preset: "day" as "day" | "week" | "month" | "custom",
    date: "2026-06-16",
    from: "",
    to: "",
    step: vi.fn(),
    setFrom: vi.fn(),
    setTo: vi.fn(),
    load: vi.fn(),
  },
}));
vi.mock("../../stores/activity.svelte.js", () => store);

import RangeNavigator from "./RangeNavigator.svelte";

beforeEach(() => {
  store.activity.preset = "day";
  store.activity.date = "2026-06-16";
  store.activity.from = "";
  store.activity.to = "";
  store.activity.step.mockReset();
  store.activity.setFrom.mockReset();
  store.activity.setTo.mockReset();
  store.activity.load.mockReset();
});

describe("RangeNavigator", () => {
  it("steps backward and reloads for a day preset", async () => {
    render(RangeNavigator);
    await fireEvent.click(screen.getByRole("button", { name: /previous/i }));
    expect(store.activity.step).toHaveBeenCalledWith(-1);
    expect(store.activity.load).toHaveBeenCalled();
  });
  it("steps forward and reloads", async () => {
    render(RangeNavigator);
    await fireEvent.click(screen.getByRole("button", { name: /next/i }));
    expect(store.activity.step).toHaveBeenCalledWith(1);
  });
  it("shows from/to date inputs for the custom preset", () => {
    store.activity.preset = "custom";
    store.activity.from = "2026-06-10";
    store.activity.to = "2026-06-12";
    render(RangeNavigator);
    const inputs = screen.getAllByDisplayValue(/2026-06-1[02]/);
    expect(inputs.length).toBe(2);
  });
  it("editing the from input updates the store and reloads", async () => {
    store.activity.preset = "custom";
    render(RangeNavigator);
    const inputs = document.querySelectorAll('input[type="date"]');
    await fireEvent.input(inputs[0]!, { target: { value: "2026-06-11" } });
    expect(store.activity.setFrom).toHaveBeenCalledWith("2026-06-11");
    expect(store.activity.load).toHaveBeenCalled();
  });
});
