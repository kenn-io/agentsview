export type ActivityKind = "thinking" | "generation" | "tool" | "unattributed";

export function activityToken(kind: ActivityKind): string {
  const tokens: Record<ActivityKind, string> = {
    thinking: "var(--cat-task)",
    generation: "var(--cat-edit)",
    tool: "var(--cat-tool)",
    unattributed: "var(--cat-other)",
  };
  return tokens[kind];
}
