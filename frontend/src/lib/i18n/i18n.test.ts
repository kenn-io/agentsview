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
import ja from "../../../messages/ja.json";
import az from "../../../messages/az.json";
import es from "../../../messages/es.json";

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
    expect(normalizeLocale("ja")).toBe("ja");
    expect(normalizeLocale("ja-JP")).toBe("ja");
    expect(normalizeLocale("az")).toBe("az");
    expect(normalizeLocale("az-AZ")).toBe("az");
    expect(normalizeLocale("az-Latn-AZ")).toBe("az");
    expect(normalizeLocale("es")).toBe("es");
    expect(normalizeLocale("es-ES")).toBe("es");
    expect(normalizeLocale("es-MX")).toBe("es");
    expect(normalizeLocale("es-419")).toBe("es");
    expect(normalizeLocale("ES-AR")).toBe("es");
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
    expect(SUPPORTED_LOCALES).toEqual(["en", "zh-CN", "zh-TW", "ko", "fr", "ja", "az", "es"]);
  });

  it("keeps every translated locale's keys aligned with English", () => {
    expect(Object.keys(zhCN).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(zhTW).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(ko).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(fr).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(ja).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(az).sort()).toEqual(Object.keys(en).sort());
    expect(Object.keys(es).sort()).toEqual(Object.keys(en).sort());
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
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("12 sessions");

    runtime.setLocale("zh-CN", { reload: false });
    expect(m.nav_sessions()).toBe("会话");
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("12 个会话");

    runtime.setLocale("zh-TW", { reload: false });
    expect(m.nav_sessions()).toBe("對話");
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("12 個對話");

    runtime.setLocale("ko", { reload: false });
    expect(m.nav_sessions()).toBe("세션");
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("세션 12개");

    runtime.setLocale("fr", { reload: false });
    expect(m.nav_sessions()).toBe("Sessions");
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("12 sessions");

    runtime.setLocale("es", { reload: false });
    expect(m.nav_sessions()).toBe("Sesiones");
    expect(
      m.status_bar_sessions({
        count: 12,
        countLabel: "12",
      }),
    ).toBe("12 sesiones");
    expect(m.settings_language_label()).toBe("Idioma de la interfaz");

    runtime.setLocale("ja", { reload: false });
    expect(m.nav_sessions()).toBe(ja.nav_sessions);
    expect(
      m.session_breadcrumb_usage_breakdown_steps({
        count: 5,
        countLabel: "5",
      }),
    ).toBe("5 ステップ");
    expect(m.trash_deleted_ago({ time: "ちょうど今" })).toBe("削除: ちょうど今");
    expect(m.data_view_inventory()).toBe("インベントリ");
    expect(m.activity_untimed_count({ count: "3" })).toBe("3 件（時間情報なし）");
    expect(m.insights_page_no_generated_saved()).toBe("保存済みの生成分析はありません。");
    expect(m.activity_loading_usage()).toBe("使用状況を読み込み中…");

    runtime.setLocale("az", { reload: false });
    expect(m.nav_sessions()).toBe(az.nav_sessions);
    expect(m.settings_language_azerbaijani()).toBe(az.settings_language_azerbaijani);
  });

  it("selects cardinal plural variants per locale", () => {
    runtime.setLocale("en", { reload: false });
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 tool call");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 tool calls");
    expect(m.parallel_group_call_count({ count: 1 })).toBe("1 call");
    expect(m.parallel_group_call_count({ count: 2 })).toBe("2 calls");
    expect(
      m.status_bar_sessions({
        count: 1,
        countLabel: "1",
      }),
    ).toBe("1 session");
    expect(
      m.sidebar_session_count({
        count: 2,
        countLabel: "2",
      }),
    ).toBe("2 sessions");
    expect(
      m.trash_msgs({
        count: 1,
        countLabel: "1",
      }),
    ).toBe("1 msg");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 message");
    expect(m.subagent_inline_message_count({ count: 5 })).toBe("5 messages");
    expect(m.message_content_turn_summary({ count: 1, duration: "2m 1s" })).toBe(
      "turn 2m 1s · 1 call",
    );
    expect(m.message_content_turn_summary({ count: 4, duration: "2m 1s" })).toBe(
      "turn 2m 1s · 4 calls",
    );

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
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 則訊息");

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

    // Spanish has one/other like English, and CLDR puts 0 in `other`, so zero
    // takes the plural form unlike French.
    runtime.setLocale("es", { reload: false });
    expect(m.tool_call_group_call_count({ count: 0 })).toBe("0 llamadas a herramienta");
    expect(m.tool_call_group_call_count({ count: 1 })).toBe("1 llamada a herramienta");
    expect(m.tool_call_group_call_count({ count: 3 })).toBe("3 llamadas a herramienta");
    expect(m.subagent_inline_message_count({ count: 1 })).toBe("1 mensaje");
    expect(m.subagent_inline_message_count({ count: 5 })).toBe("5 mensajes");
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
    const setParaglideLocale = vi.spyOn(runtime, "setLocale").mockImplementation(() => undefined);

    setLocale("zh-CN");

    expect(setParaglideLocale).toHaveBeenCalledWith("zh-CN");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("zh-CN");
  });
});

const FRICTION_KEYS = [
  "nav_friction",
  "friction_page_help_intro",
  "friction_page_help_docs",
  "friction_page_refresh",
  "friction_date_label",
  "friction_date_previous",
  "friction_date_next",
  "friction_date_no_match",
  "friction_build_now",
  "friction_build_title",
  "friction_build_done",
  "friction_build_nothing",
  "friction_build_failed",
  "friction_empty_title",
  "friction_empty_hint",
  "friction_load_error",
  "friction_retry",
  "friction_digest_heading",
  "friction_digest_missing",
  "friction_digest_meta",
  "friction_scanned",
  "friction_headline_title",
  "friction_p0_title",
  "friction_p0_none",
  "friction_p0_item",
  "friction_patterns_new",
  "friction_patterns_recurring",
  "friction_patterns_none",
  "friction_pattern_occurrences",
  "friction_pattern_sessions",
  "friction_pattern_first_seen",
  "friction_section_corrections",
  "friction_section_errors",
  "friction_section_workarounds",
  "friction_section_deferrals",
  "friction_section_patterns",
  "friction_none_corrections",
  "friction_none_errors",
  "friction_none_workarounds",
  "friction_none_deferrals",
  "friction_none_patterns",
  "friction_section_frustration",
  "friction_section_interruptions",
  "friction_none_frustration",
  "friction_none_interruptions",
  "friction_interruption_count",
  "friction_kind_correction",
  "friction_kind_error",
  "friction_kind_workaround",
  "friction_kind_deferral",
  "friction_kind_pattern",
  "friction_kind_frustration",
  "friction_kind_interruption",
  "friction_disabled_title",
  "friction_disabled_hint",
  "friction_section_personas",
  "friction_section_spend",
  "friction_spend_total",
  "friction_spend_no_cost",
  "friction_spend_coverage",
  "friction_spend_tokens",
  "friction_spend_by_role",
  "friction_spend_by_model",
  "friction_archive_title",
  "friction_archive_yesterday",
  "friction_archive_no_rows",
  "friction_archive_week",
  "friction_markdown_title",
  "friction_markdown_show",
  "friction_markdown_copy",
  "friction_markdown_copied",
  "friction_markdown_download",
  "friction_markdown_loading",
  "friction_markdown_error",
  "friction_open_session",
  "friction_open_session_start",
  "friction_quality_card_title",
  "friction_quality_card_body",
  "friction_quality_card_open",
  "friction_session_title",
  "friction_session_loading",
  "friction_session_error",
  "friction_session_empty",
  "friction_session_jump",
  "friction_session_jump_title",
] as const;

describe("friction log messages", () => {
  beforeEach(() => {
    setLocale("en");
  });

  it.each([
    ["en", en],
    ["zh-CN", zhCN],
    ["zh-TW", zhTW],
    ["ko", ko],
    ["fr", fr],
    ["ja", ja],
    ["az", az],
    ["es", es],
  ] as const)("defines every friction key in %s", (_locale, catalogue) => {
    const keys = new Set(Object.keys(catalogue));
    const missing = FRICTION_KEYS.filter((key) => !keys.has(key));
    expect(missing).toEqual([]);
  });

  it.each([
    [1, "1 session scanned"],
    [4, "4 sessions scanned"],
  ])("selects the English plural for %i scanned sessions", (count, expected) => {
    expect(m.friction_scanned({ count })).toBe(expected);
  });

  it("keeps technical identifiers untranslated in the P0 line", () => {
    setLocale("fr");
    expect(m.friction_p0_item({ tool: "bash", count: 3 })).toContain("bash");
    setLocale("en");
    expect(m.friction_p0_item({ tool: "bash", count: 3 })).toBe(
      "bash failed in 3 distinct sessions",
    );
  });

  it("formats the digest heading with an em dash", () => {
    expect(m.friction_digest_heading({ date: "2026-09-21" })).toBe(
      "Friction Log — 2026-09-21",
    );
  });
});
