// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import CurrencySettings from "./CurrencySettings.svelte";
import { setLocale } from "../../i18n/index.js";
import { costDisplay } from "../../stores/costDisplay.svelte.js";

async function chooseCurrency(label: string): Promise<void> {
  const trigger = document.querySelector<HTMLButtonElement>(
    'button[title="Display currency"]',
  );
  expect(trigger).not.toBeNull();
  trigger!.click();
  await tick();

  const option = Array.from(document.querySelectorAll<HTMLElement>('[role="option"]')).find(
    (candidate) => candidate.textContent?.trim() === label,
  );
  expect(option).not.toBeNull();
  option!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
  await tick();
}

async function setRate(value: string): Promise<void> {
  const input = document.querySelector<HTMLInputElement>("#currency-eur-per-usd");
  expect(input).not.toBeNull();
  input!.value = value;
  input!.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();
}

function applyButton(): HTMLButtonElement {
  const button = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
    (candidate) => candidate.textContent?.trim() === "Apply",
  );
  expect(button).not.toBeNull();
  return button!;
}

beforeEach(() => {
  localStorage.clear();
  setLocale("en");
  costDisplay.setPreference("USD", null);
});

afterEach(() => {
  setLocale("en");
  costDisplay.setPreference("USD", null);
  document.body.innerHTML = "";
});

describe("CurrencySettings", () => {
  it("renders USD by default with the manual-rate explanation", async () => {
    const component = mount(CurrencySettings, { target: document.body });
    await tick();

    expect(document.querySelector('button[title="Display currency"]')?.textContent).toContain("USD");
    expect(document.body.textContent).toContain("EUR per USD");
    expect(document.body.textContent).toContain("1 USD = ... EUR");

    unmount(component);
  });

  it("keeps the applied preference unchanged until a valid EUR rate is applied", async () => {
    const component = mount(CurrencySettings, { target: document.body });
    await tick();

    await chooseCurrency("EUR");
    expect(costDisplay.preference).toEqual({ currency: "USD", eurPerUsd: null });

    await setRate("");
    applyButton().click();
    await tick();

    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "positive finite",
    );
    expect(costDisplay.preference).toEqual({ currency: "USD", eurPerUsd: null });

    unmount(component);
  });

  it.each(["", "0", "-0.1", "abc", "Infinity"])(
    "rejects an invalid EUR rate %j",
    async (value) => {
      const component = mount(CurrencySettings, { target: document.body });
      await tick();
      await chooseCurrency("EUR");
      await setRate(value);
      applyButton().click();
      await tick();

      expect(document.querySelector('[role="alert"]')).not.toBeNull();
      expect(costDisplay.preference).toEqual({ currency: "USD", eurPerUsd: null });
      unmount(component);
      document.body.innerHTML = "";
    },
  );

  it("accepts EUR parity and lets USD apply without a rate", async () => {
    const component = mount(CurrencySettings, { target: document.body });
    await tick();

    await chooseCurrency("EUR");
    await setRate("1");
    applyButton().click();
    await tick();
    expect(costDisplay.preference).toEqual({ currency: "EUR", eurPerUsd: 1 });

    await chooseCurrency("USD");
    await setRate("");
    applyButton().click();
    await tick();
    expect(costDisplay.preference).toEqual({ currency: "USD", eurPerUsd: 1 });

    unmount(component);
  });

  it("keeps the remembered rate available after switching back to USD", async () => {
    const component = mount(CurrencySettings, { target: document.body });
    await tick();

    await chooseCurrency("EUR");
    await setRate("0.9");
    applyButton().click();
    await tick();

    await chooseCurrency("USD");
    await setRate("");
    applyButton().click();
    await tick();
    await chooseCurrency("EUR");

    expect(document.querySelector<HTMLInputElement>("#currency-eur-per-usd")?.value).toBe("0.9");
    unmount(component);
  });
});
