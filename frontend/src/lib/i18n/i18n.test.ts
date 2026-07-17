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
import zhTW from "../../../messages/zh-TW.json";
import ko from "../../../messages/ko.json";
import fr from "../../../messages/fr.json";

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
    expect(normalizeLocale("zh-TW-TW")).toBe("zh-TW");
    expect(normalizeLocale("zh-tw")).toBe("zh-TW");
    expect(normalizeLocale("zh-Hant-HK")).toBe("zh-TW");
    expect(normalizeLocale("ko")).toBe("ko");
    expect(normalizeLocale("ko-KR")).toBe("ko");
    expect(normalizeLocale("fr")).toBe("fr");
    expect(normalizeLocale("fr-FR")).toBe("fr");
    expect(normalizeLocale("fr-CA")).toBe("fr");
    expect(normalizeLocale("fr-CH")).toBe("fr");
  });

  it("falls back to English for unsupported locales", () => {
    expect(normalizeLocale("de-DE")).toBe(DEFAULT_LOCALE);
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
      languages: ["de-DE"],
      language: "de-DE",
    });

    expect(chooseInitialLocale()).toBe("en");
  });

  it("keeps the supported locale list explicit", () => {
    expect(SUPPORTED_LOCALES).toEqual(["en", "zh-CN", "zh-TW", "ko", "fr"]);
  });

  it("keeps every translated locale's keys aligned with English", () => {
    expect(Object.keys(zhCN).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(zhTW).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(ko).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(fr).sort()).toEqual(Object.keys(en).sort());
  });

  it("points auth recovery at pre-auth token sources", () => {
    expect(en.app_auth_description).toContain("~/.agentsview/config.toml");
    expect(en.app_auth_description).toContain("AGENTSVIEW_AUTH_TOKEN");
    expect(en.app_auth_description).not.toContain("server's console");
    expect(en.app_auth_description).not.toContain("settings page");
    expect(zhCN.app_auth_description).toContain("~/.agentsview/config.toml");
    expect(zhCN.app_auth_description).toContain("AGENTSVIEW_AUTH_TOKEN");
    expect(zhCN.app_auth_description).not.toContain("服务器控制台");
    expect(zhCN.app_auth_description).not.toContain("设置页");
  });

  it("renders generated Paraglide messages for each supported locale", () => {
    runtime.setLocale("en", { reload: false });
    expect(m.nav_sessions()).toBe("Sessions");
    expect(m.status_bar_sessions({
      count: 12,
      countLabel: "12",
    })).toBe("12 sessions");

    runtime.setLocale("zh-CN", { reload: false });
    expect(m.nav_sessions()).toBe("会话");
    expect(m.status_bar_sessions({
      count: 12,
      countLabel: "12",
    })).toBe("12 个会话");

    runtime.setLocale("zh-TW", { reload: false });
    expect(m.nav_sessions()).toBe("會話");
    expect(m.status_bar_sessions({
      count: 12,
      countLabel: "12",
    })).toBe("12 個會話");

    runtime.setLocale("ko", { reload: false });
    expect(m.nav_sessions()).toBe("세션");
    expect(m.status_bar_sessions({
      count: 12,
      countLabel: "12",
    })).toBe("세션 12개");

    runtime.setLocale("fr", { reload: false });
    expect(m.nav_sessions()).toBe("Sessions");
    expect(m.status_bar_sessions({
      count: 12,
      countLabel: "12",
    })).toBe("12 sessions");
  });

  it("selects cardinal plural variants per locale", () => {
    runtime.setLocale("en", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 tool call");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 tool calls");
    expect(m.parallel_group_call_count({ count: 1 })).toBe("1 call");
    expect(m.parallel_group_call_count({ count: 2 })).toBe("2 calls");
    expect(m.status_bar_sessions({
      count: 1,
      countLabel: "1",
    })).toBe("1 session");
    expect(m.sidebar_session_count({
      count: 2,
      countLabel: "2",
    })).toBe("2 sessions");
    expect(m.trash_msgs({
      count: 1,
      countLabel: "1",
    })).toBe("1 msg");
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

    // Traditional Chinese has no plural distinction, so a single variant
    // serves every count.
    runtime.setLocale("zh-TW", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 次 tool call");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 次 tool call");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 條消息");

    // Korean has no plural distinction, so a single variant serves every count.
    runtime.setLocale("ko", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("도구 호출 1회");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("도구 호출 3회");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("메시지 1개");

    // French has one/other like English, but CLDR puts 0 in `one`, so zero
    // takes the singular form where English would say "0 tool calls".
    runtime.setLocale("fr", { reload: false });
    expect(m.tool_call_group_call_count({ count: 0 })).toBe("0 appel d'outil");
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 appel d'outil");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 appels d'outil");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 message");
    expect(m.subagent_inline_message_count({ count: 5 })).toBe("5 messages");
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
