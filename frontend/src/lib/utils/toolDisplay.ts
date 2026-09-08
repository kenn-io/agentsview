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
export function displayToolName(call: { tool_name: string; category?: string | null }): string {
  const cat = call.category;
  if (cat && cat !== "Other" && cat !== "Tool") return cat;
  return call.tool_name;
}

/** Display retained image placeholders alongside their surrounding text.
 * Unrecognized results stay intact so display projection cannot hide blocks. */
export function displayToolResult(content: string): string {
  if (!content.includes('"agentsview_image"')) return content;
  const direct = displayToolResultArray(content);
  if (direct !== null) return direct;

  // Multi-agent summaries join labelled results and trailing anonymous output
  // with blank lines. Keep each label and any unrecognized part as stored.
  return content
    .split("\n\n")
    .map((part) => {
      const newline = part.indexOf("\n");
      if (newline > 0 && part.slice(0, newline).trimEnd().endsWith(":")) {
        const body = part.slice(newline + 1);
        return part.slice(0, newline + 1) + (displayToolResultArray(body) ?? body);
      }
      return displayToolResultArray(part) ?? part;
    })
    .join("\n\n");
}

function displayToolResultArray(content: string): string | null {
  let blocks: unknown;
  try {
    blocks = JSON.parse(content);
  } catch {
    return null;
  }
  if (!Array.isArray(blocks)) return content;
  const text: string[] = [];
  let hasImage = false;
  for (const block of blocks) {
    if (!block || typeof block !== "object" || typeof block.text !== "string") {
      return content;
    }
    if (block.type === "agentsview_image" && block.version === 1) {
      hasImage = true;
    } else if (block.type !== "input_text" && block.type !== "text") {
      return content;
    }
    text.push(block.text);
  }
  return hasImage ? text.join("\n\n") : content;
}
