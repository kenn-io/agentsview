import { describe, expect, it, beforeEach, vi } from "vite-plus/test";

import {
  DEFAULT_LOCALE,
  LOCALE_STORAGE_KEY,
  SUPPORTED_LOCALES,
  chooseInitialLocale,
  formatDateTime,
  normalizeLocale,
  setLocale,
} from "./index.js";
import { m } from "../paraglide/messages.js";
import * as runtime from "../paraglide/runtime.js";
import en from "../../../messages/en.json";
import zhCN from "../../../messages/zh-CN.json";

describe("i18n locale selection", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
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

  it("uses the stored locale before browser languages", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "zh-CN");
    vi.stubGlobal("navigator", {
      languages: ["en-US"],
      language: "en-US",
    });

    expect(chooseInitialLocale()).toBe("zh-CN");
  });

  it("uses the browser language when no stored locale exists", () => {
    vi.stubGlobal("navigator", {
      languages: ["zh-CN", "en-US"],
      language: "en-US",
    });

    expect(chooseInitialLocale()).toBe("zh-CN");
  });

  it("respects browser language priority", () => {
    vi.stubGlobal("navigator", {
      languages: ["en-US", "zh-CN"],
      language: "zh-CN",
    });

    expect(chooseInitialLocale()).toBe("en");
  });

  it("falls back to English when browser languages are unsupported", () => {
    vi.stubGlobal("navigator", {
      languages: ["fr-FR"],
      language: "fr-FR",
    });

    expect(chooseInitialLocale()).toBe("en");
  });

  it("keeps the supported locale list explicit", () => {
    expect(SUPPORTED_LOCALES).toEqual(["en", "zh-CN"]);
  });

  it("keeps Simplified Chinese locale keys aligned with English", () => {
    expect(Object.keys(zhCN).sort()).toEqual(Object.keys(en).sort());
  });

  it("renders generated Paraglide messages for each supported locale", () => {
    runtime.setLocale("en", { reload: false });
    expect(m.nav_sessions()).toBe("Sessions");
    expect(m.status_bar_sessions({ count: "12" })).toBe("12 sessions");

    runtime.setLocale("zh-CN", { reload: false });
    expect(m.nav_sessions()).toBe("会话");
    expect(m.status_bar_sessions({ count: "12" })).toBe("12 个会话");
  });

  it("selects cardinal plural variants per locale", () => {
    runtime.setLocale("en", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 tool call");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 tool calls");
    expect(m.parallel_group_call_count({ count: 1 })).toBe("1 call");
    expect(m.parallel_group_call_count({ count: 2 })).toBe("2 calls");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 message");
    expect(m.subagent_inline_message_count({ count: 5 })).toBe("5 messages");
    expect(
      m.message_content_turn_summary({ count: 1, duration: "2m 1s" }),
    ).toBe("turn 2m 1s · 1 call");
    expect(
      m.message_content_turn_summary({ count: 4, duration: "2m 1s" }),
    ).toBe("turn 2m 1s · 4 calls");

    // Simplified Chinese has no plural distinction, so a single variant
    // serves every count.
    runtime.setLocale("zh-CN", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 次 tool call");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 次 tool call");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 条消息");
  });

  it("formats dates with the active Paraglide locale", () => {
    const dateTimeFormat = vi
      .spyOn(Intl, "DateTimeFormat")
      .mockImplementation(function DateTimeFormatMock(locale, options) {
        return {
          format: () => `${locale}:${options?.timeZone ?? "local"}`,
        } as Intl.DateTimeFormat;
      } as typeof Intl.DateTimeFormat);

    runtime.setLocale("zh-CN", { reload: false });

    expect(formatDateTime(0, { timeZone: "UTC" })).toBe("zh-CN:UTC");
    expect(dateTimeFormat).toHaveBeenCalledWith("zh-CN", {
      timeZone: "UTC",
    });
  });

  it("sets the Paraglide runtime locale with its default reload behavior", () => {
    const setParaglideLocale = vi
      .spyOn(runtime, "setLocale")
      .mockImplementation(() => undefined);

    setLocale("zh-CN");

    expect(setParaglideLocale).toHaveBeenCalledWith("zh-CN");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("zh-CN");
  });
});
