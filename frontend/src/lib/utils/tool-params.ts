/** Extracts display metadata and fallback content from tool call input_json. */

export interface MetaTag {
  label: string;
  value: string;
}

export function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) + "\u2026" : s;
}

type Params = Record<string, unknown>;

/** Extract metadata tags for common tool types.
 *  Returns null for Task/TaskCreate/TaskUpdate (handled separately). */
export function extractToolParamMeta(
  toolName: string,
  params: Params,
): MetaTag[] | null {
  const skip = ["Task", "TaskCreate", "TaskUpdate"];
  if (skip.includes(toolName)) return null;
  const meta: MetaTag[] = [];
  if (toolName === "Read") {
    const filePath = params.file_path ?? params.path;
    if (filePath)
      meta.push({
        label: "file",
        value: truncate(String(filePath), 80),
      });
    if (params.offset != null)
      meta.push({
        label: "offset",
        value: String(params.offset),
      });
    if (params.limit != null)
      meta.push({
        label: "limit",
        value: String(params.limit),
      });
    if (params.pages)
      meta.push({
        label: "pages",
        value: String(params.pages),
      });
  } else if (toolName === "Edit") {
    const filePath = params.file_path ?? params.path ?? params.filePath;
    if (filePath)
      meta.push({
        label: "file",
        value: truncate(String(filePath), 80),
      });
    if (params.replace_all)
      meta.push({ label: "mode", value: "replace_all" });
  } else if (toolName === "Write") {
    const filePath = params.file_path ?? params.path;
    if (filePath)
      meta.push({
        label: "file",
        value: truncate(String(filePath), 80),
      });
  } else if (toolName === "Grep") {
    if (params.pattern)
      meta.push({
        label: "pattern",
        value: truncate(String(params.pattern), 60),
      });
    if (params.path)
      meta.push({
        label: "path",
        value: truncate(String(params.path), 80),
      });
    if (params.glob)
      meta.push({ label: "glob", value: String(params.glob) });
    if (params.output_mode)
      meta.push({
        label: "mode",
        value: String(params.output_mode),
      });
  } else if (toolName === "Glob") {
    if (params.pattern)
      meta.push({
        label: "pattern",
        value: String(params.pattern),
      });
    if (params.path)
      meta.push({
        label: "path",
        value: truncate(String(params.path), 80),
      });
  } else if (toolName === "Bash") {
    if (params.description)
      meta.push({
        label: "description",
        value: truncate(String(params.description), 80),
      });
  } else if (toolName === "Skill") {
    if (params.skill)
      meta.push({
        label: "skill",
        value: String(params.skill),
      });
  }
  return meta.length ? meta : null;
}

/** Parameter keys that are pi-internal metadata, not tool input.
 *  These should not appear in the expanded content display. */
const INTERNAL_PARAMS = new Set(["agent__intent", "_i"]);

/** Generate displayable content from input params when
 *  the regex-captured content is empty. */
export function generateFallbackContent(
  toolName: string,
  params: Params,
): string | null {
  if (toolName === "Task" || toolName === "Agent") return null;
  if (toolName === "Edit") {
    const lines: string[] = [];
    // Claude Code: old_string/new_string; OpenCode: oldString/newString (camelCase)
    const oldStr =
      params.old_string ?? params.old_str ?? params.oldString;
    const newStr =
      params.new_string ?? params.new_str ?? params.newString;
    if (oldStr != null) {
      lines.push("--- old");
      lines.push(truncate(String(oldStr), 500));
    }
    if (newStr != null) {
      lines.push("+++ new");
      lines.push(truncate(String(newStr), 500));
    }
    // Pi: edits[] array with set_line, replace_lines, insert_after, or op-based operations
    if (!lines.length && Array.isArray(params.edits)) {
      for (const edit of params.edits as Record<string, unknown>[]) {
        const setLine = edit.set_line as
          | Record<string, unknown>
          | undefined;
        const replaceLines = edit.replace_lines as
          | Record<string, unknown>
          | undefined;
        const insertAfter = edit.insert_after as
          | Record<string, unknown>
          | undefined;
        if (setLine) {
          // {set_line: {anchor, new_text}} format
          if (setLine.anchor) lines.push(`@ ${setLine.anchor}`);
          if (setLine.new_text != null) {
            const text = String(setLine.new_text);
            lines.push(text ? truncate(text, 400) : "(delete)");
          }
        } else if (replaceLines) {
          // {replace_lines: {start_anchor, end_anchor, new_text}} format
          const start = replaceLines.start_anchor;
          const end = replaceLines.end_anchor;
          if (start && end) lines.push(`@ ${start}..${end}`);
          else if (start) lines.push(`@ ${start}`);
          const text = String(replaceLines.new_text ?? "");
          lines.push(text ? truncate(text, 400) : "(delete)");
        } else if (insertAfter) {
          // {insert_after: {anchor, text}} format
          if (insertAfter.anchor) lines.push(`insert after ${insertAfter.anchor}`);
          const text = String(insertAfter.text ?? "");
          lines.push(text ? truncate(text, 400) : "(empty)");
        } else if (Array.isArray(edit.lines)) {
          // {op, pos, end, lines} format — real Pi agent format
          if (edit.op) lines.push(`${edit.op}${edit.pos ? ` @ ${edit.pos}` : ""}`);
          lines.push(truncate((edit.lines as string[]).join("\n"), 400));
        } else {
          // {op, tag, content} format — legacy/alternative Pi format
          if (edit.tag) lines.push(`tag: ${edit.tag}`);
          const content = edit.content;
          if (Array.isArray(content))
            lines.push(truncate(content.join("\n"), 400));
        }
      }
    }
    return lines.length ? lines.join("\n") : null;
  }
  if (toolName === "Write" && params.content != null) {
    const text = String(params.content);
    return text ? truncate(text, 500) : "(empty file)";
  }
  const lines: string[] = [];
  for (const [key, value] of Object.entries(params)) {
    if (INTERNAL_PARAMS.has(key)) continue;
    if (value == null || value === "") continue;
    const strVal =
      typeof value === "string"
        ? value
        : JSON.stringify(value);
    lines.push(`${key}: ${truncate(strVal, 200)}`);
  }
  return lines.length ? lines.join("\n") : null;
}
