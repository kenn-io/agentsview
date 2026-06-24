import { describe, expect, it, beforeEach, vi } from "vite-plus/test";

import {
  DEFAULT_LOCALE,
  LOCALE_STORAGE_KEY,
  SUPPORTED_LOCALES,
  chooseInitialLocale,
  normalizeLocale,
} from "./index.js";

describe("i18n locale selection", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("normalizes supported locale variants", () => {
    expect(normalizeLocale("en-US")).toBe("en");
    expect(normalizeLocale("zh-Hans-CN")).toBe("zh-CN");
    expect(normalizeLocale("zh-cn")).toBe("zh-CN");
  });

  it("falls back to English for unsupported locales", () => {
    expect(normalizeLocale("fr-FR")).toBe(DEFAULT_LOCALE);
    expect(normalizeLocale("")).toBe(DEFAULT_LOCALE);
  });

  it("uses the stored locale when one is set", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "zh-CN");
    vi.stubGlobal("navigator", {
      languages: ["en-US"],
      language: "en-US",
    });

    expect(chooseInitialLocale()).toBe("zh-CN");
  });

  it("defaults to English regardless of browser language", () => {
    vi.stubGlobal("navigator", {
      languages: ["zh-CN", "en-US"],
      language: "zh-CN",
    });

    expect(chooseInitialLocale()).toBe(DEFAULT_LOCALE);
  });

  it("defaults to English when no locale is stored", () => {
    vi.stubGlobal("navigator", {
      languages: ["fr-FR"],
      language: "fr-FR",
    });

    expect(chooseInitialLocale()).toBe("en");
  });

  it("keeps the supported locale list explicit", () => {
    expect(SUPPORTED_LOCALES).toEqual(["en", "zh-CN"]);
  });
});
