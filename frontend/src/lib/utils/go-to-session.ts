export type SessionId = string;

export type SessionIdResolution =
  | { kind: "resolved"; id: SessionId }
  | { kind: "empty" | "unknown" | "ambiguous" | "capped" };

const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function resolveSessionId(
  input: string,
  candidates: readonly string[],
  capped: boolean,
): SessionIdResolution {
  const value = input.trim();
  if (!value) return { kind: "empty" };
  if (capped) return { kind: "capped" };

  const normalizedUuid = UUID_PATTERN.test(value) ? value.toLowerCase() : null;
  const matches = [
    ...new Set(
      candidates.filter((candidate) => {
        if (candidate === value) return true;
        if (!normalizedUuid) return false;
        const normalizedCandidate = candidate.toLowerCase();
        return (
          normalizedCandidate === normalizedUuid ||
          normalizedCandidate.endsWith(`:${normalizedUuid}`) ||
          normalizedCandidate.endsWith(`~${normalizedUuid}`)
        );
      }),
    ),
  ];

  if (matches.length === 1) return { kind: "resolved", id: matches[0]! };
  if (matches.length > 1) return { kind: "ambiguous" };
  return { kind: "unknown" };
}

export function sessionLookupPartial(input: string): string {
  const value = input.trim();
  return UUID_PATTERN.test(value) ? value.toLowerCase() : value;
}
