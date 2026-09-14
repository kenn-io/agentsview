export type SessionId = string;

export type SessionIdResolution =
  | { kind: "resolved"; id: SessionId }
  | { kind: "empty" | "invalid" | "unknown" | "ambiguous" | "capped" };

const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function resolveSessionId(
  input: string,
  candidates: readonly string[],
  capped: boolean,
): SessionIdResolution {
  const uuid = input.trim();
  if (!uuid) return { kind: "empty" };
  if (!UUID_PATTERN.test(uuid)) return { kind: "invalid" };
  if (capped) return { kind: "capped" };

  const normalized = uuid.toLowerCase();
  const matches = [
    ...new Set(
      candidates.filter((candidate) => {
        const value = candidate.toLowerCase();
        return (
          value === normalized ||
          value.endsWith(`:${normalized}`) ||
          value.endsWith(`~${normalized}`)
        );
      }),
    ),
  ];

  if (matches.length === 1) return { kind: "resolved", id: matches[0]! };
  if (matches.length > 1) return { kind: "ambiguous" };
  return { kind: "unknown" };
}
