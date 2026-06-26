// ABOUTME: Builds a structured one-line summary for a tool call header.
// ABOUTME: Pure; reads input_json + result_content, conservative on counts.
import type { ToolCall } from "../api/types.js";

const MAX = 100;

/** Count lines, ignoring a single trailing newline. "" -> 0, "a\nb\n" -> 2. */
function countLines(s: string): number {
  if (s === "") return 0;
  const body = s.endsWith("\n") ? s.slice(0, -1) : s;
  return body === "" ? 0 : body.split("\n").length;
}

/** Count non-empty lines, or null when empty/blank (used for result lists). */
function countResultLines(s: string | undefined): number | null {
  if (!s) return null;
  let n = 0;
  for (const line of s.split("\n")) {
    if (line.trim() !== "") n += 1;
  }
  return n > 0 ? n : null;
}

function firstLine(s: string): string {
  return (s.split("\n")[0] ?? "").slice(0, MAX);
}

function asString(v: unknown): string | null {
  return typeof v === "string" && v.length > 0 ? v : null;
}

type Params = Record<string, unknown>;

function parseParams(toolCall: ToolCall): Params | null {
  if (!toolCall.input_json) return null;
  try {
    const parsed: unknown = JSON.parse(toolCall.input_json);
    return parsed && typeof parsed === "object" ? (parsed as Params) : null;
  } catch {
    return null;
  }
}

function fileArg(p: Params): string | null {
  return (
    asString(p.file_path) ??
    asString(p.path) ??
    asString(p.filePath) ??
    asString(p.file)
  );
}

function todoSummary(p: Params): string | null {
  const todos = p.todos;
  if (!Array.isArray(todos) || todos.length === 0) return null;
  const items = todos as Array<{ content?: unknown; status?: unknown }>;
  const target =
    items.find((t) => t?.status === "in_progress") ?? items[items.length - 1];
  const text = asString(target?.content);
  return text ? `→ ${text}`.slice(0, MAX) : null;
}

function specialSummary(name: string, p: Params): string | null {
  if (name === "TodoWrite") return todoSummary(p);
  if (name === "TaskCreate") {
    const subject = asString(p.subject);
    return subject ? subject.slice(0, MAX) : null;
  }
  if (name === "TaskUpdate") {
    const parts: string[] = [];
    if (p.taskId != null) parts.push(`#${String(p.taskId)}`);
    const status = asString(p.status);
    if (status) parts.push(status);
    const subject = asString(p.subject);
    if (subject) parts.push(subject);
    return parts.length ? parts.join(" · ").slice(0, MAX) : null;
  }
  if (name === "Skill" || name === "skill") {
    const skill = asString(p.skill) ?? asString(p.name);
    return skill ? skill.slice(0, MAX) : null;
  }
  if (name === "ToolSearch") {
    const query = asString(p.query);
    return query ? firstLine(query) : null;
  }
  return null;
}

function isTaskCall(name: string, cat: string | undefined): boolean {
  return (
    name === "Task" ||
    name === "Agent" ||
    cat === "Task" ||
    name.includes("subagent")
  );
}

/**
 * Build a structured one-line summary for a tool call header, or null when
 * only a generic first-body-line preview is possible. Pure: parses
 * input_json and reads result_content; never throws.
 */
export function summarizeToolCall(toolCall: ToolCall): string | null {
  const p = parseParams(toolCall);
  if (!p) return null;

  const name = toolCall.tool_name;
  const cat = toolCall.category;

  const special = specialSummary(name, p);
  if (special) return special;

  if (isTaskCall(name, cat)) {
    const desc = asString(p.description) ?? asString(p.prompt);
    return desc ? firstLine(desc) : null;
  }

  const key = cat || name;

  if (key === "Bash") {
    const cmd = asString(p.command) ?? asString(p.cmd);
    return cmd ? `$ ${firstLine(cmd)}` : null;
  }
  if (key === "Read") {
    const file = fileArg(p);
    if (!file) return null;
    const lines = toolCall.result_content
      ? countLines(toolCall.result_content)
      : 0;
    const suffix = lines > 0 ? ` (${lines} lines)` : "";
    return `${file.slice(0, MAX)}${suffix}`;
  }
  if (key === "Edit") {
    const file = fileArg(p);
    if (!file) return null;
    const oldS = p.old_string ?? p.oldString;
    const newS = p.new_string ?? p.newString;
    let suffix = "";
    if (typeof oldS === "string" && typeof newS === "string") {
      const added = countLines(newS);
      const removed = countLines(oldS);
      if (added > 0 || removed > 0) suffix = ` (+${added} -${removed})`;
    }
    return `${file.slice(0, MAX)}${suffix}`;
  }
  if (key === "Write") {
    const file = fileArg(p);
    if (!file) return null;
    const added = typeof p.content === "string" ? countLines(p.content) : 0;
    const suffix = added > 0 ? ` (+${added})` : "";
    return `${file.slice(0, MAX)}${suffix}`;
  }
  if (key === "Grep") {
    const pattern = asString(p.pattern) ?? asString(p.query);
    if (!pattern) return null;
    let suffix = "";
    if (p.output_mode !== "count") {
      const n = countResultLines(toolCall.result_content);
      if (n != null) suffix = ` (${n} matches)`;
    }
    return `${pattern.slice(0, MAX)}${suffix}`;
  }
  if (key === "Glob") {
    const pattern = asString(p.pattern);
    if (!pattern) return null;
    const n = countResultLines(toolCall.result_content);
    const suffix = n != null ? ` (${n} files)` : "";
    return `${pattern.slice(0, MAX)}${suffix}`;
  }

  // Generic structured fallback: any tool exposing a known key arg.
  const file = fileArg(p);
  if (file) return file.slice(0, MAX);
  const cmd = asString(p.command) ?? asString(p.cmd);
  if (cmd) return `$ ${firstLine(cmd)}`;
  const pattern = asString(p.pattern);
  if (pattern) return pattern.slice(0, MAX);

  return null;
}
