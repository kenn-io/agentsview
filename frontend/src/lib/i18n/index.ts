import {
  getLocale,
  setLocale as setParaglideLocale,
} from "../paraglide/runtime.js";

export { m } from "../paraglide/messages.js";
// Current BCP 47 tag (en / zh-CN / zh-TW) for kit-ui components that take a
// `locale` prop, so their date/tooltip formatting follows the app language
// setting instead of the browser locale.
export { getLocale };

export const DEFAULT_LOCALE = "en";
export const LOCALE_STORAGE_KEY = "agentsview-locale";
export const SUPPORTED_LOCALES = ["en", "zh-CN", "zh-TW"] as const;

export type SupportedLocale = typeof SUPPORTED_LOCALES[number];

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
  if (
    normalized === "zh-tw" ||
    normalized.startsWith("zh-tw-") ||
    normalized.startsWith("zh-hant")
  ) {
    return "zh-TW";
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

function browserLocales(): string[] {
  if (typeof navigator === "undefined") return [];
  const languages = Array.isArray(navigator.languages)
    ? navigator.languages
    : [];
  return [...languages, navigator.language].filter(Boolean);
}

export function chooseInitialLocale(): SupportedLocale {
  const stored = storedLocale();
  if (stored) return stored;
  const browserLocale = browserLocales()
    .map(matchingLocale)
    .find((value): value is SupportedLocale => value !== null);
  return browserLocale ?? DEFAULT_LOCALE;
}

export function setLocale(value: SupportedLocale) {
  setParaglideLocale(value);
  try {
    localStorage?.setItem(LOCALE_STORAGE_KEY, value);
  } catch {
    // Ignore storage failures; the active in-memory locale still changes.
  }
}

export function formatDateTime(
  value: Date | number | string,
  options: Intl.DateTimeFormatOptions = {},
): string {
  return new Intl.DateTimeFormat(getLocale(), options).format(new Date(value));
}

export function initI18n() {
  setLocale(chooseInitialLocale());
}
