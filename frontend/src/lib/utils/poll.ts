/**
 * SameValueZero equality: like `===` but treats `NaN === NaN`.
 * Matches the semantics used by Map/Set and Array.prototype.includes.
 */
export function sameValueZero<T>(a: T, b: T): boolean {
  return a === b || (a !== a && b !== b);
}

/**
 * Polls a value-producing function until the value stays
 * constant for `stableDurationMs`. Throws if the value does
 * not stabilize within `maxWaitMs`.
 *
 * Uses SameValueZero for equality by default, which means
 * callers returning new object/array instances each poll must
 * supply a custom `isEqual` comparator.
 */
export async function waitForStableValue<T>(
  fn: () => Promise<T> | T,
  stableDurationMs: number,
  pollIntervalMs?: number,
  maxWaitMs?: number,
  isEqual: (a: T, b: T) => boolean = sameValueZero,
): Promise<T> {
  const interval = pollIntervalMs ?? 100;
  const deadline = Date.now() + (maxWaitMs ?? stableDurationMs * 3);
  let lastValue = await fn();
  let stableStart = Date.now();

  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, interval));
    const current = await fn();

    if (!isEqual(current, lastValue)) {
      lastValue = current;
      stableStart = Date.now();
    } else if (Date.now() - stableStart >= stableDurationMs) {
      return current;
    }
  }
  throw new Error(
    `Value did not stabilize within ` +
      `${maxWaitMs ?? stableDurationMs * 3}ms.` +
      ` Last value: ${lastValue}`,
  );
}
