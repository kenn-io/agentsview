// @vitest-environment jsdom
/** Search translations are compiled for every supported application locale. */
import { afterEach, describe, expect, it } from "vite-plus/test";
import en from "../../../messages/find/en.json";
import zhCN from "../../../messages/find/zh-CN.json";
import zhTW from "../../../messages/find/zh-TW.json";
import ko from "../../../messages/find/ko.json";
import fr from "../../../messages/find/fr.json";
import ja from "../../../messages/find/ja.json";
import baseEn from "../../../messages/en.json";
import { m, setLocale, type SupportedLocale } from "./index.js";

const catalogs = { en, "zh-CN": zhCN, "zh-TW": zhTW, ko, fr, ja };
afterEach(() => setLocale("en"));

describe("find message catalogs", () => {
  it("has identical keys in all locales without overriding existing copy", () => {
    const keys = Object.keys(en).sort();
    for (const catalog of Object.values(catalogs))
      expect(Object.keys(catalog).sort()).toEqual(keys);
    for (const key of keys) expect(Object.hasOwn(baseEn, key)).toBe(false);
  });

  it.each(Object.entries(catalogs))(
    "compiles localized announcements for %s",
    (locale, catalog) => {
      setLocale(locale as SupportedLocale);
      expect(m.session_find_loading_history()).toBe(catalog.session_find_loading_history);
      expect(m.session_find_announce_match({ current: 2, total: 3 })).toBe(
        catalog.session_find_announce_match.replace("{current}", "2").replace("{total}", "3"),
      );
    },
  );
});
