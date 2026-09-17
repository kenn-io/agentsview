import type { MoneyMoney } from "./api/generated/index.js";
import { getLocale } from "./i18n/index.js";
import { costDisplay } from "./stores/costDisplay.svelte.js";

export type Money = MoneyMoney;

export const ZERO_MONEY: Money = Object.freeze({ microdollars: 0 });

export function moneyFromMicrodollars(microdollars: number): Money {
  return { microdollars };
}

export function divideMoney(value: Money, divisor: number): Money {
  if (divisor === 0) return ZERO_MONEY;
  return moneyFromMicrodollars(Math.round(value.microdollars / divisor));
}

export function compareMoney(left: Money, right: Money): number {
  return left.microdollars - right.microdollars;
}

export function formatMoney(value: Money): string {
  const { currency, eurPerUsd } = costDisplay.preference;
  const amount =
    (value.microdollars / 1_000_000) *
    (currency === "EUR" ? eurPerUsd! : 1);
  const absoluteAmount = Math.abs(amount);
  if (absoluteAmount > 0 && absoluteAmount < 0.01) {
    const cents = new Intl.NumberFormat(getLocale(), {
      style: "currency",
      currency,
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(0.01);
    return amount < 0 ? `>-${cents}` : `<${cents}`;
  }

  const showCents = absoluteAmount < 100;
  return new Intl.NumberFormat(getLocale(), {
    style: "currency",
    currency,
    minimumFractionDigits: showCents ? 2 : 0,
    maximumFractionDigits: showCents ? 2 : 0,
  }).format(amount);
}

export function formatSignedMoney(value: Money): string {
  if (value.microdollars === 0) return formatMoney(value);
  const formatted = formatMoney(value);
  return value.microdollars > 0 ? `+${formatted}` : formatted;
}
