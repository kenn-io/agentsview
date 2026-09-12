/** Stable tool scopes and user-controlled disclosure during search navigation. */
export interface ToolSearchScope {
  ordinal: number;
  callIdx: number | string;
}

export function toolSearchKey(
  scope: ToolSearchScope | undefined,
  kind: "tool-input" | "tool-output" | "tool-history",
  eventIndex?: number,
): string | undefined {
  if (!scope) return undefined;
  const index = eventIndex === undefined ? scope.callIdx : `${scope.callIdx}.${eventIndex}`;
  return `${scope.ordinal}:${kind}:${index}`;
}

/** A manual toggle survives repainting, but a new navigation can reveal again. */
export function searchCollapsed(
  userCollapsed: boolean,
  current: boolean,
  navigationRevision: number,
  overrideSeq: number,
): boolean {
  return overrideSeq === navigationRevision ? userCollapsed : current ? false : userCollapsed;
}
