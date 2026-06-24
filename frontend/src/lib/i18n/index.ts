import {
  addMessages,
  init,
  locale,
} from "svelte-i18n";

import en from "./locales/en.json";
import zhCN from "./locales/zh-CN.json";

export const DEFAULT_LOCALE = "en";
export const LOCALE_STORAGE_KEY = "agentsview-locale";
export const SUPPORTED_LOCALES = ["en", "zh-CN"] as const;

export type SupportedLocale = typeof SUPPORTED_LOCALES[number];
type MessageDictionary = {
  [key: string]: MessageDictionary | string | Array<string | MessageDictionary> | null;
};

export function normalizeLocale(value: string | null | undefined): SupportedLocale {
  return matchingLocale(value) ?? DEFAULT_LOCALE;
}

function matchingLocale(value: string | null | undefined): SupportedLocale | null {
  const normalized = value?.trim().toLowerCase();
  if (!normalized) return null;
  if (normalized === "en" || normalized.startsWith("en-")) return "en";
  if (normalized === "zh-cn" || normalized.startsWith("zh-hans")) {
    return "zh-CN";
  }
  return null;
}

function storedLocale(): SupportedLocale | null {
  try {
    const raw = localStorage?.getItem(LOCALE_STORAGE_KEY);
    if (raw && SUPPORTED_LOCALES.includes(raw as SupportedLocale)) {
      return raw as SupportedLocale;
    }
  } catch {
    // Ignore storage failures and fall back to browser detection.
  }
  return null;
}

export function chooseInitialLocale(): SupportedLocale {
  // Default to English; only honor an explicit stored preference.
  // We intentionally do not auto-detect the browser language so the
  // interface stays in English until the user opts into another locale.
  return storedLocale() ?? DEFAULT_LOCALE;
}

export function setLocale(value: SupportedLocale) {
  locale.set(value);
  try {
    localStorage?.setItem(LOCALE_STORAGE_KEY, value);
  } catch {
    // Ignore storage failures; the active in-memory locale still changes.
  }
}

export function initI18n() {
  addMessages("en", en as MessageDictionary);
  addMessages("zh-CN", zhCN as MessageDictionary);
  init({
    fallbackLocale: DEFAULT_LOCALE,
    initialLocale: chooseInitialLocale(),
  });
}
