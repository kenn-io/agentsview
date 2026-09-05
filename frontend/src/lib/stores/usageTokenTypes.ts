import type { UsageMode } from "./usage.svelte.js";

export const ALL_TOKEN_TYPES = ["input", "cache_write", "cache_read", "output"] as const;

export type UsageTokenType = (typeof ALL_TOKEN_TYPES)[number];

export interface TokenBreakdown {
  inputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  outputTokens: number;
}

const TOKEN_TYPE_SET = new Set<string>(ALL_TOKEN_TYPES);

export function canonicalTokenTypes(selected: readonly UsageTokenType[]): UsageTokenType[] {
  const selectedSet = new Set(selected);
  return ALL_TOKEN_TYPES.filter((tokenType) => selectedSet.has(tokenType));
}

export function selectedTokenTypesFromParams(params: Record<string, string>): UsageTokenType[] {
  const raw = params.token_types;
  if (!raw) return [...ALL_TOKEN_TYPES];

  const values = raw.split(",");
  if (values.length === 0 || values.some((value) => !TOKEN_TYPE_SET.has(value))) {
    return [...ALL_TOKEN_TYPES];
  }
  return canonicalTokenTypes(values as UsageTokenType[]);
}

export function withSelectedTokenTypes(
  params: Record<string, string>,
  selected: readonly UsageTokenType[],
  mode: UsageMode,
): Record<string, string> {
  const next = { ...params };
  delete next.token_types;
  const canonical = canonicalTokenTypes(selected);
  if (mode === "token" && canonical.length > 0 && canonical.length < ALL_TOKEN_TYPES.length) {
    next.token_types = canonical.join(",");
  }
  return next;
}

export function sumSelectedTokens(
  breakdown: TokenBreakdown,
  selected: readonly UsageTokenType[],
): number {
  let total = 0;
  for (const tokenType of selected) {
    switch (tokenType) {
      case "input":
        total += breakdown.inputTokens;
        break;
      case "cache_write":
        total += breakdown.cacheCreationTokens;
        break;
      case "cache_read":
        total += breakdown.cacheReadTokens;
        break;
      case "output":
        total += breakdown.outputTokens;
        break;
    }
  }
  return total;
}
