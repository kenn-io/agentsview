/** Extracts display metadata and fallback content from tool call input_json. */

export interface MetaTag {
  label: string;
  value: string;
  /** Short visual text for paths; value remains the canonical copy value. */
  displayValue?: string;
}

export function isAbsolutePath(value: string): boolean {
  return value.startsWith("/") || /^[A-Za-z]:[\\/]/.test(value) || value.startsWith("\\\\");
}

export function pathDisplayValue(value: string): string {
  if (!isAbsolutePath(value)) return truncate(value, 100);
  if (value.length <= 80) return value;
  const parts = value.split(/[\\/]+/).filter(Boolean);
  const rootParts = value.startsWith("/")
    ? 0
    : /^[A-Za-z]:[\\/]/.test(value)
      ? 1
      : value.startsWith("\\\\")
        ? 2
        : 0;
  if (parts.length <= rootParts + 2) return value;
  return parts.slice(-2).join("/");
}

/** Keep the full value; show at most `max` characters. */
function textMetaValue(value: unknown, max: number): Pick<MetaTag, "value" | "displayValue"> {
  const canonical = paramText(value);
  const displayValue = truncate(canonical, max);
  return displayValue === canonical ? { value: canonical } : { value: canonical, displayValue };
}

function pathMetaValue(value: unknown): Pick<MetaTag, "value" | "displayValue"> {
  const canonical = paramText(value);
  const displayValue = pathDisplayValue(canonical);
  return displayValue === canonical ? { value: canonical } : { value: canonical, displayValue };
}

export function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) + "\u2026" : s;
}

type Params = Record<string, unknown>;

/** Display text for a tool parameter decoded from JSON. Matches String(),
 *  so arrays still join with commas, except that objects show their JSON
 *  instead of "[object Object]". */
function paramText(value: unknown): string {
  if (Array.isArray(value))
    return value.map((item) => (item == null ? "" : paramText(item))).join(",");
  if (typeof value === "object" && value !== null) return JSON.stringify(value);
  // JSON.parse yields no other object types, so value is a primitive here.
  return String(value as string | number | boolean | null | undefined);
}

/** Max diff lines rendered in fallback content to avoid DOM bloat. */
const MAX_DIFF_LINES = 200;

/** Parameter keys that are pi-internal metadata, not tool input.
 *  These should not appear in the expanded content display. */
const INTERNAL_PARAMS = new Set(["agent__intent", "_i"]);

/** Bash parameters shown in the body or the description field. */
const BASH_BODY_PARAMS = new Set(["command", "cmd", "description"]);

/** Bash parameters that name a directory. */
const BASH_PATH_PARAMS = new Set(["cwd", "workdir", "dir", "directory"]);

const SHELL_SAFE_ARG = /^[\w@%+=:,./-]+$/;

function shellQuote(arg: string): string {
  return SHELL_SAFE_ARG.test(arg) ? arg : `'${arg.replaceAll("'", `'\\''`)}'`;
}

/** The shell command a Bash call ran, or null when it names none. An
 *  argument array is quoted so the result can be pasted into a shell. */
export function bashCommandText(params: Params): string | null {
  const cmd = params.command ?? params.cmd;
  if (cmd == null || cmd === "") return null;
  if (Array.isArray(cmd)) return cmd.map((arg) => shellQuote(paramText(arg))).join(" ");
  return paramText(cmd);
}

function capLines(text: string): string {
  const lines = text.split("\n");
  if (lines.length <= MAX_DIFF_LINES) return text;
  return lines.slice(0, MAX_DIFF_LINES).join("\n") + `\n... (${lines.length} lines total)`;
}

/** Extract metadata tags for common tool types.
 *  Dispatches on normalized category so all agents (Claude,
 *  Gemini, Codex, etc.) render consistently.
 *  Returns null for Task/TaskCreate/TaskUpdate (handled separately). */
export function extractToolParamMeta(
  toolName: string,
  params: Params,
  category?: string,
): MetaTag[] | null {
  const skip = ["Task", "TaskCreate", "TaskUpdate"];
  if (skip.includes(toolName)) return null;
  const cat = category || toolName;
  const meta: MetaTag[] = [];
  if (cat === "Read") {
    const filePath = params.file_path ?? params.path;
    if (filePath)
      meta.push({
        label: "file",
        ...pathMetaValue(filePath),
      });
    if (params.offset != null)
      meta.push({
        label: "offset",
        value: paramText(params.offset),
      });
    if (params.limit != null)
      meta.push({
        label: "limit",
        value: paramText(params.limit),
      });
    if (params.pages)
      meta.push({
        label: "pages",
        value: paramText(params.pages),
      });
  } else if (cat === "Edit") {
    const filePath = params.file_path ?? params.path ?? params.filePath ?? params.file;
    if (filePath)
      meta.push({
        label: "file",
        ...pathMetaValue(filePath),
      });
    if (params.replace_all) meta.push({ label: "mode", value: "replace_all" });
    // A Codex cloud change that renames its file names the new path here.
    const kind = params.kind;
    const movePath = kind && typeof kind === "object" ? (kind as Params).move_path : undefined;
    if (typeof movePath === "string" && movePath)
      meta.push({ label: "moved to", ...pathMetaValue(movePath) });
  } else if (cat === "Write") {
    const filePath = params.file_path ?? params.path ?? params.file;
    if (filePath)
      meta.push({
        label: "file",
        ...pathMetaValue(filePath),
      });
  } else if (cat === "Grep") {
    const pattern = params.pattern ?? params.query;
    if (pattern)
      meta.push({
        label: "pattern",
        value: truncate(paramText(pattern), 60),
      });
    if (params.path)
      meta.push({
        label: "path",
        ...pathMetaValue(params.path),
      });
    if (params.glob) meta.push({ label: "glob", value: paramText(params.glob) });
    if (params.output_mode)
      meta.push({
        label: "mode",
        value: paramText(params.output_mode),
      });
  } else if (cat === "Glob") {
    if (params.pattern)
      meta.push({
        label: "pattern",
        value: paramText(params.pattern),
      });
    if (params.path)
      meta.push({
        label: "path",
        ...pathMetaValue(params.path),
      });
  } else if (cat === "Bash") {
    // The command is the block body, so the other parameters, such as the
    // working directory, become labeled fields.
    if (params.description)
      meta.push({ label: "description", ...textMetaValue(params.description, 80) });
    for (const [key, value] of Object.entries(params)) {
      if (BASH_BODY_PARAMS.has(key) || INTERNAL_PARAMS.has(key)) continue;
      if (value == null || value === "") continue;
      meta.push(
        BASH_PATH_PARAMS.has(key)
          ? { label: key, ...pathMetaValue(value) }
          : { label: key, ...textMetaValue(value, 80) },
      );
    }
  } else if (toolName === "Skill" || toolName === "skill") {
    const skill = params.skill ?? params.name;
    if (skill)
      meta.push({
        label: "skill",
        value: paramText(skill),
      });
  }
  return meta.length ? meta : null;
}

function visibleParamLines(params: Params, excluded = new Set<string>()): string[] {
  const lines: string[] = [];
  for (const [key, value] of Object.entries(params)) {
    if (INTERNAL_PARAMS.has(key) || excluded.has(key)) continue;
    if (value == null || value === "") continue;
    const strVal = typeof value === "string" ? value : JSON.stringify(value);
    lines.push(`${key}: ${strVal}`);
  }
  return lines;
}

/** Generate displayable content from input params when
 *  the regex-captured content is empty. */
export function generateFallbackContent(toolName: string, params: Params): string | null {
  if (toolName === "Task" || toolName === "Agent") return null;
  if (toolName === "Bash" || toolName === "run_command") {
    // Only the command; extractToolParamMeta shows the other parameters.
    const cmd = bashCommandText(params);
    if (cmd != null) return capLines(cmd);
  }
  const isEdit = toolName === "Edit" || params.command === "strReplace";
  if (isEdit) {
    const lines: string[] = [];
    // Claude Code: old_string/new_string; OpenCode: oldString/newString (camelCase)
    const oldStr = params.old_string ?? params.old_str ?? params.oldString ?? params.oldStr;
    const newStr = params.new_string ?? params.new_str ?? params.newString ?? params.newStr;
    // Kiro IDE: pre-computed unified diff from Go parser
    const diffText = params.diff;
    if (!lines.length && typeof diffText === "string" && diffText) {
      const diffLines = diffText.split("\n");
      if (diffLines.length > MAX_DIFF_LINES) {
        return (
          diffLines.slice(0, MAX_DIFF_LINES).join("\n") + `\n... (${diffLines.length} lines total)`
        );
      }
      return diffText;
    }
    const patchText = params.patch ?? params.patch_text ?? params.patchText;
    if (!lines.length && typeof patchText === "string" && patchText) {
      const patchLines = patchText.split("\n");
      if (patchLines.length > MAX_DIFF_LINES) {
        return (
          patchLines.slice(0, MAX_DIFF_LINES).join("\n") +
          `\n... (${patchLines.length} lines total)`
        );
      }
      return patchText;
    }
    if (oldStr != null || newStr != null) {
      const oldText = paramText(oldStr ?? "");
      const newText = paramText(newStr ?? "");
      const oldLines = oldText.split("\n");
      const newLines = newText.split("\n");
      lines.push(`@@ -1,${oldLines.length} +1,${newLines.length} @@`);
      for (const l of oldLines) lines.push(`-${l}`);
      for (const l of newLines) lines.push(`+${l}`);
      if (lines.length > MAX_DIFF_LINES) {
        lines.length = MAX_DIFF_LINES;
        lines.push(`... (${oldLines.length + newLines.length} lines total)`);
      }
    }
    // Pi: edits[] array with set_line, replace_lines, insert_after, or op-based operations
    if (!lines.length && Array.isArray(params.edits)) {
      for (const edit of params.edits as Record<string, unknown>[]) {
        const setLine = edit.set_line as Record<string, unknown> | undefined;
        const replaceLines = edit.replace_lines as Record<string, unknown> | undefined;
        const insertAfter = edit.insert_after as Record<string, unknown> | undefined;
        if (setLine) {
          // {set_line: {anchor, new_text}} format
          if (setLine.anchor) lines.push(`@ ${paramText(setLine.anchor)}`);
          if (setLine.new_text != null) {
            const text = paramText(setLine.new_text);
            lines.push(text ? truncate(text, 400) : "(delete)");
          }
        } else if (replaceLines) {
          // {replace_lines: {start_anchor, end_anchor, new_text}} format
          const start = replaceLines.start_anchor;
          const end = replaceLines.end_anchor;
          if (start && end) lines.push(`@ ${paramText(start)}..${paramText(end)}`);
          else if (start) lines.push(`@ ${paramText(start)}`);
          const text = paramText(replaceLines.new_text ?? "");
          lines.push(text ? truncate(text, 400) : "(delete)");
        } else if (insertAfter) {
          // {insert_after: {anchor, text}} format
          if (insertAfter.anchor) lines.push(`insert after ${paramText(insertAfter.anchor)}`);
          const text = paramText(insertAfter.text ?? "");
          lines.push(text ? truncate(text, 400) : "(empty)");
        } else if (Array.isArray(edit.lines)) {
          // {op, pos, end, lines} format — real Pi agent format
          if (edit.op)
            lines.push(`${paramText(edit.op)}${edit.pos ? ` @ ${paramText(edit.pos)}` : ""}`);
          lines.push(truncate((edit.lines as string[]).join("\n"), 400));
        } else {
          // {op, tag, content} format — legacy/alternative Pi format
          if (edit.tag) lines.push(`tag: ${paramText(edit.tag)}`);
          const content = edit.content;
          if (Array.isArray(content)) lines.push(truncate(content.join("\n"), 400));
        }
      }
    }
    return lines.length ? lines.join("\n") : null;
  }
  if (toolName === "Write" || (toolName === "write" && params.command === "create")) {
    if (params.content != null) {
      const text = paramText(params.content);
      if (!text) return "(empty file)";
      const allLines = text.split("\n");
      const capped = allLines.length > MAX_DIFF_LINES;
      const show = capped ? allLines.slice(0, MAX_DIFF_LINES) : allLines;
      const header = `@@ -0,0 +1,${allLines.length} @@\n`;
      const body = show.map((l) => `+${l}`).join("\n");
      const suffix = capped ? `\n... (${allLines.length} lines total)` : "";
      return header + body + suffix;
    }
  }
  const lines = visibleParamLines(params);
  if (!lines.length) return null;
  const allLines = lines.join("\n").split("\n");
  if (allLines.length > MAX_DIFF_LINES) {
    return allLines.slice(0, MAX_DIFF_LINES).join("\n") + `\n... (${allLines.length} lines total)`;
  }
  return lines.join("\n");
}

function nonEmptyText(value: unknown): string | null {
  return value == null || value === "" ? null : paramText(value);
}

function filePathParam(params: Params): string | null {
  return nonEmptyText(params.file_path ?? params.path ?? params.filePath ?? params.file);
}

function visibleEntries(params: Params): Array<[string, unknown]> {
  return Object.entries(params).filter(
    ([key, value]) => !INTERNAL_PARAMS.has(key) && value != null && value !== "",
  );
}

function copyTextFor(name: string, params: Params): string | null {
  if (name === "Bash" || name === "run_command") return bashCommandText(params);
  if (name === "Edit" || params.command === "strReplace") {
    const diff = params.diff ?? params.patch ?? params.patch_text ?? params.patchText;
    if (typeof diff === "string" && diff) return diff;
    const oldStr = params.old_string ?? params.old_str ?? params.oldString ?? params.oldStr;
    const newStr = params.new_string ?? params.new_str ?? params.newString ?? params.newStr;
    if (oldStr != null || newStr != null) {
      const oldLines = paramText(oldStr ?? "").split("\n");
      const newLines = paramText(newStr ?? "").split("\n");
      return [
        `@@ -1,${oldLines.length} +1,${newLines.length} @@`,
        ...oldLines.map((line) => `-${line}`),
        ...newLines.map((line) => `+${line}`),
      ].join("\n");
    }
    // Only a bare path copies as the path; any other input, such as Pi
    // edit operations or RooCode replacement content, goes out as JSON.
    return visibleEntries(params).length === 1 ? filePathParam(params) : null;
  }
  if (name === "Write" || (name === "write" && params.command === "create")) {
    return nonEmptyText(params.content) ?? filePathParam(params);
  }
  if (name === "Read") return filePathParam(params);
  if (name === "Grep" || name === "Glob") return nonEmptyText(params.pattern ?? params.query);
  return null;
}

/** Text a tool block's copy button puts on the clipboard: what a user would
 *  paste back somewhere. A Bash call copies its command, an edit its diff,
 *  a write its file content, a read its path, and a search its pattern.
 *  Other tools copy their single text argument, or else their input as
 *  JSON. Task calls return null; their prompt is copied instead. */
export function toolCopyText(
  toolName: string,
  category: string | undefined,
  params: Params,
): string | null {
  if (toolName === "Task" || toolName === "Agent") return null;
  const known = (category ? copyTextFor(category, params) : null) ?? copyTextFor(toolName, params);
  if (known != null) return known;
  const visible = visibleEntries(params);
  if (visible.length === 0) return null;
  const [first] = visible;
  if (visible.length === 1 && first && typeof first[1] === "string") return first[1];
  return JSON.stringify(Object.fromEntries(visible), null, 2);
}
