/** Returns the user-facing label for a tool call.
 *
 *  Prefers the normalized category (e.g. "Bash" for codex's
 *  exec_command) over the raw tool name, so analytics and
 *  ToolBlock headers stay consistent across agents.
 *
 *  Falls back to the raw tool name when the category is too
 *  generic ("Other") or when a tool's specific name is more
 *  informative than its category (e.g. "Skill" inside the
 *  Tool category). */
export function displayToolName(call: {
  tool_name: string;
  category?: string | null;
}): string {
  const cat = call.category;
  if (cat && cat !== "Other" && cat !== "Tool") return cat;
  return call.tool_name;
}
