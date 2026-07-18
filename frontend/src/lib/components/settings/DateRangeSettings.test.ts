// @vitest-environment jsdom
import { cleanup, fireEvent, render } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { initI18n } from "../../i18n/index.js";
import {
  YOKED_DATES_STORAGE_KEY,
  yokedDates,
} from "../../stores/yokedDates.svelte.js";
import DateRangeSettings from "./DateRangeSettings.svelte";

beforeEach(() => {
  localStorage.clear();
  yokedDates.hydrate();
  initI18n();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  yokedDates.hydrate();
});

describe("DateRangeSettings", () => {
  it("enables date linking and persists the browser preference", async () => {
    const { getByRole } = render(DateRangeSettings);
    const linkedDates = getByRole("switch", {
      name: "Link date ranges across pages",
    }) as HTMLInputElement;

    expect(linkedDates.checked).toBe(false);

    await fireEvent.click(linkedDates);

    expect(yokedDates.enabled).toBe(true);
    expect(JSON.parse(localStorage.getItem(YOKED_DATES_STORAGE_KEY)!)).toEqual({
      version: 2,
      enabled: true,
      range: null,
    });
  });

  it("disables date linking and clears the shared range", async () => {
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
    });

    const { getByLabelText } = render(DateRangeSettings);
    const checkbox = getByLabelText(
      "Link date ranges across pages",
    ) as HTMLInputElement;

    expect(checkbox.checked).toBe(true);
    expect(yokedDates.range).not.toBeNull();

    await fireEvent.click(checkbox);

    expect(yokedDates.enabled).toBe(false);
    expect(yokedDates.range).toBeNull();
    expect(JSON.parse(localStorage.getItem(YOKED_DATES_STORAGE_KEY)!)).toEqual({
      version: 2,
      enabled: false,
      range: null,
    });
  });
});
