import type { MoneyMoney } from "./api/generated/models/MoneyMoney.js";
import { getLocale } from "./i18n/index.js";

export type Money = MoneyMoney;

export const ZERO_MONEY: Money = Object.freeze({ microdollars: 0 });

export function moneyFromMicrodollars(microdollars: number): Money {
  return { microdollars };
}

export function addMoney(left: Money, right: Money): Money {
  return moneyFromMicrodollars(left.microdollars + right.microdollars);
}

export function subtractMoney(left: Money, right: Money): Money {
  return moneyFromMicrodollars(left.microdollars - right.microdollars);
}

export function divideMoney(value: Money, divisor: number): Money {
  if (divisor === 0) return ZERO_MONEY;
  return moneyFromMicrodollars(Math.round(value.microdollars / divisor));
}

export function compareMoney(left: Money, right: Money): number {
  return left.microdollars - right.microdollars;
}

export function formatMoney(value: Money): string {
  const dollars = value.microdollars / 1_000_000;
  const absoluteMicrodollars = Math.abs(value.microdollars);
  if (absoluteMicrodollars > 0 && absoluteMicrodollars < 10_000) {
    const cents = new Intl.NumberFormat(getLocale(), {
      style: "currency",
      currency: "USD",
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(0.01);
    return value.microdollars < 0 ? `>-${cents}` : `<${cents}`;
  }

  const showCents = absoluteMicrodollars < 100_000_000;
  return new Intl.NumberFormat(getLocale(), {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: showCents ? 2 : 0,
    maximumFractionDigits: showCents ? 2 : 0,
  }).format(dollars);
}

export function formatSignedMoney(value: Money): string {
  if (value.microdollars === 0) return formatMoney(value);
  const formatted = formatMoney(value);
  return value.microdollars > 0 ? `+${formatted}` : formatted;
}
