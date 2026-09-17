export type ActivityKind = "tool" | "unattributed";

export function activityToken(kind: ActivityKind): string {
  const tokens: Record<ActivityKind, string> = {
    tool: "var(--cat-tool)",
    unattributed: "var(--cat-other)",
  };
  return tokens[kind];
}
