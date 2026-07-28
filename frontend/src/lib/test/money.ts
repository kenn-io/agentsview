import type { Money } from "../money.js";

/** Builds the wire-format money object used by UI fixtures. */
export function testMoney(dollars: number): Money {
  return { microdollars: Math.round(dollars * 1_000_000) };
}
