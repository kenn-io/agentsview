export type DisplayCurrency = "USD" | "EUR";

export interface CostDisplayPreference {
  currency: DisplayCurrency;
  eurPerUsd: number | null;
}

export const COST_DISPLAY_STORAGE_KEY = "agentsview-cost-display";

const DEFAULT_PREFERENCE: CostDisplayPreference = {
  currency: "USD",
  eurPerUsd: null,
};

type StorageLike = Pick<Storage, "getItem" | "setItem">;

function getLocalStorage(): StorageLike | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

function parsePreference(value: unknown): CostDisplayPreference | null {
  if (typeof value !== "object" || value === null) return null;

  const candidate = value as Partial<CostDisplayPreference>;
  if (candidate.currency !== "USD" && candidate.currency !== "EUR") return null;

  const rate = candidate.eurPerUsd;
  if (
    rate !== null &&
    (typeof rate !== "number" || !Number.isFinite(rate) || rate <= 0)
  ) {
    return null;
  }
  if (candidate.currency === "EUR" && typeof rate !== "number") return null;

  return { currency: candidate.currency, eurPerUsd: rate };
}

export class CostDisplayStore {
  #preference: CostDisplayPreference = $state({ ...DEFAULT_PREFERENCE });

  constructor(private readonly storage: StorageLike | null = getLocalStorage()) {
    this.hydrate();
  }

  get preference(): Readonly<CostDisplayPreference> {
    return this.#preference;
  }

  setPreference(currency: DisplayCurrency, eurPerUsd: number | null): boolean {
    const rate =
      currency === "USD" && eurPerUsd === null
        ? this.#preference.eurPerUsd
        : eurPerUsd;
    const preference = parsePreference({ currency, eurPerUsd: rate });
    if (!preference) return false;

    this.#preference = preference;
    this.persist();
    return true;
  }

  private hydrate(): void {
    if (!this.storage) return;
    try {
      const raw = this.storage.getItem(COST_DISPLAY_STORAGE_KEY);
      if (!raw) return;
      const preference = parsePreference(JSON.parse(raw));
      if (preference) this.#preference = preference;
    } catch {
      // Storage can be unavailable or contain invalid JSON.
    }
  }

  private persist(): void {
    if (!this.storage) return;
    try {
      this.storage.setItem(
        COST_DISPLAY_STORAGE_KEY,
        JSON.stringify(this.#preference),
      );
    } catch {
      // Storage can be unavailable or full; current-tab state still works.
    }
  }
}

export const costDisplay = new CostDisplayStore();
