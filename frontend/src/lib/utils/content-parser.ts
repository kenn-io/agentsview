import type { Message, ToolCall } from "../api/types.js";
import { LRUCache } from "./cache.js";

export type SegmentType = "text" | "thinking" | "tool" | "code";

export interface ContentSegment {
  type: SegmentType;
  content: string;
  /** Tool name or code language, when applicable */
  label?: string;
  /** Structured tool call data from the API, when available */
  toolCall?: ToolCall;
}

/**
 * Marked thinking blocks use explicit [/Thinking] delimiters.
 * Tried first; captures everything between markers.
 */
const THINKING_MARKED_RE =
  /\[Thinking\]\n?([\s\S]*?)\n?\[\/Thinking\]/g;

/**
 * Legacy thinking blocks without end markers.
 * Used as fallback for old data that predates [/Thinking].
 */
const THINKING_LEGACY_RE =
  /\[Thinking\]\n?([\s\S]*?)(?=\n\[|\n\n|$)/g;

const TOOL_NAMES =
  "Tool|Read|Write|Edit|Bash|Glob|Grep|Other|TaskCreate|TaskUpdate|TaskGet|TaskList|Task|Agent|Skill|" +
  "SendMessage|Question|Todo List|Entering Plan Mode|" +
  "Exiting Plan Mode|exec_command|shell_command|" +
  "write_stdin|apply_patch|shell|parallel|view_image|" +
  "request_user_input|update_plan";

const TOOL_ALIASES: Record<string, string> = {
  Agent: "Task",
  exec_command: "Bash",
  shell_command: "Bash",
  write_stdin: "Bash",
  shell: "Bash",
  apply_patch: "Edit",
  // Pi tool names
  str_replace: "Edit",
  run_command: "Bash",
  create_file: "Write",
  read_file: "Read",
  // Lowercase pi/OpenCode bare tool names
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  grep: "Grep",
  glob: "Glob",
  find: "Read",
};


const TOOL_RE = new RegExp(
  `\\[(${TOOL_NAMES})([^\\]]*)\\]([\\s\\S]*?)(?=\\n\\[|\\n\\n|$)`,
  "g",
);

const CODE_BLOCK_RE = /```(\w*)\n([\s\S]*?)```/g;

/** Returns true if text[from..to) contains a backtick run of
 *  exactly `len` characters. Used to detect a closing inline
 *  code delimiter on the same line as the opener. */
function hasRunBefore(
  text: string,
  from: number,
  to: number,
  len: number,
): boolean {
  for (let k = from; k < to; k++) {
    if (text[k] !== "`") continue;
    const s = k;
    while (k < to && text[k] === "`") k++;
    if (k - s === len) return true;
  }
  return false;
}

/**
 * Scan for inline code spans per CommonMark rules: an opening
 * backtick run of length N is closed by the next run of exactly
 * N backticks. Fenced code blocks (triple-backtick at line
 * start followed by a newline) are excluded — those are handled
 * separately by CODE_BLOCK_RE.
 */
function scanInlineCodeSpans(
  text: string,
): Array<[number, number]> {
  const spans: Array<[number, number]> = [];
  let i = 0;
  while (i < text.length) {
    if (text[i] !== "`") {
      i++;
      continue;
    }
    // Measure opening backtick run length.
    const openStart = i;
    while (i < text.length && text[i] === "`") i++;
    const runLen = i - openStart;

    // Skip fenced code blocks: ≥3 backticks at line start
    // with no closing run of the same length on that line.
    if (
      runLen >= 3 &&
      (openStart === 0 || text[openStart - 1] === "\n")
    ) {
      const nl = text.indexOf("\n", i);
      if (nl >= 0 && !hasRunBefore(text, i, nl, runLen)) {
        continue;
      }
    }

    // Scan for a closing run of exactly the same length.
    let found = false;
    for (let j = i; j < text.length; j++) {
      if (text[j] !== "`") continue;
      const closeStart = j;
      while (j < text.length && text[j] === "`") j++;
      if (j - closeStart === runLen) {
        spans.push([openStart, j]);
        i = j;
        found = true;
        break;
      }
    }
    // If no closing run found, i is already past the
    // unmatched opening run — continue scanning for
    // other valid spans of different lengths.
  }
  return spans;
}

interface Match {
  start: number;
  end: number;
  segment: ContentSegment;
}

const toolOnlyCache = new LRUCache<string, boolean>(12000);
const segmentCache = new LRUCache<string, ContentSegment[]>(8000);

/** Returns true if the message contains only tool calls (no text) */
export function isToolOnly(msg: Message): boolean {
  const key =
    `${msg.role}|${msg.has_tool_use ? 1 : 0}|${msg.content}`;
  const cached = toolOnlyCache.get(key);
  if (cached !== undefined) return cached;

  if (msg.role !== "assistant") return false;
  if (!msg.has_tool_use) {
    toolOnlyCache.set(key, false);
    return false;
  }
  const stripped = msg.content
    .replace(THINKING_MARKED_RE, "")
    .replace(THINKING_LEGACY_RE, "")
    .replace(TOOL_RE, "")
    .trim();
  const result = stripped.length === 0;
  toolOnlyCache.set(key, result);
  return result;
}

/** Returns true if pos falls inside any inline code span. */
function insideInlineCode(
  pos: number,
  spans: Array<[number, number]>,
): boolean {
  return spans.some(([s, e]) => pos > s && pos < e);
}

function extractMatches(text: string, parseTools = true): Match[] {
  const matches: Match[] = [];

  // Pre-compute inline code spans so we can skip
  // false-positive marker matches inside backtick-quoted
  // text (e.g. `` `[Thinking]` `` in prose).
  const codeSpans = scanInlineCodeSpans(text);

  // Marked blocks first (explicit [/Thinking] delimiters)
  for (const m of text.matchAll(THINKING_MARKED_RE)) {
    if (insideInlineCode(m.index!, codeSpans)) continue;
    matches.push({
      start: m.index!,
      end: m.index! + m[0].length,
      segment: {
        type: "thinking",
        content: (m[1] ?? "").trim(),
      },
    });
  }

  // Legacy blocks (no end marker) — skip ranges already matched
  for (const m of text.matchAll(THINKING_LEGACY_RE)) {
    const start = m.index!;
    const end = start + m[0].length;
    if (insideInlineCode(start, codeSpans)) continue;
    const overlaps = matches.some(
      (o) => start >= o.start && start < o.end,
    );
    if (overlaps) continue;
    matches.push({
      start,
      end,
      segment: {
        type: "thinking",
        content: (m[1] ?? "").trim(),
      },
    });
  }

  if (parseTools) {
    for (const m of text.matchAll(TOOL_RE)) {
      if (insideInlineCode(m.index!, codeSpans)) continue;
      const toolName = m[1] ?? "";
      const toolArgs = (m[2] ?? "").trim();
      const displayName = TOOL_ALIASES[toolName] ?? toolName;
      const label = toolArgs
        ? `${displayName} ${toolArgs}`
        : displayName;
      matches.push({
        start: m.index!,
        end: m.index! + m[0].length,
        segment: {
          type: "tool",
          content: (m[3] ?? "").trim(),
          label,
        },
      });
    }
  }

  for (const m of text.matchAll(CODE_BLOCK_RE)) {
    const idx = m.index!;
    const insideOther = matches.some(
      (o) => idx >= o.start && idx < o.end,
    );
    if (insideOther) continue;

    matches.push({
      start: idx,
      end: idx + m[0].length,
      segment: {
        type: "code",
        content: m[2] ?? "",
        label: m[1] || undefined,
      },
    });
  }

  return matches;
}

function resolveOverlaps(matches: Match[]): Match[] {
  matches.sort((a, b) => a.start - b.start);
  const deduped: Match[] = [];
  let lastEnd = 0;
  for (const m of matches) {
    if (m.start < lastEnd) continue;
    deduped.push(m);
    lastEnd = m.end;
  }
  return deduped;
}

function buildSegments(
  text: string,
  matches: Match[],
): ContentSegment[] {
  const segments: ContentSegment[] = [];
  let pos = 0;

  for (const m of matches) {
    if (m.start > pos) {
      const gap = text
        .slice(pos, m.start)
        .replace(/^\n\n+/, "")
        .trimEnd();
      if (gap) {
        segments.push({ type: "text", content: gap });
      }
    }
    segments.push(m.segment);
    pos = m.end;
  }

  if (pos < text.length) {
    const tail = text
      .slice(pos)
      .replace(/^\n\n+/, "")
      .trimEnd();
    if (tail) {
      segments.push({ type: "text", content: tail });
    }
  }

  return segments;
}

function mergeThinking(
  segments: ContentSegment[],
): ContentSegment[] {
  const result: ContentSegment[] = [];
  for (const seg of segments) {
    const prev = result[result.length - 1];
    if (
      seg.type === "thinking" &&
      prev?.type === "thinking"
    ) {
      prev.content += "\n\n" + seg.content;
    } else {
      result.push({ ...seg });
    }
  }
  return result;
}

/** Parse message content into typed segments */
export function parseContent(text: string, hasToolUse = true): ContentSegment[] {
  if (!text) return [];
  const cacheKey = hasToolUse
    ? `tools\0${text}`
    : `notools\0${text}`;
  const cached = segmentCache.get(cacheKey);
  if (cached) return cached;

  const matches = extractMatches(text, hasToolUse);

  if (matches.length === 0) {
    const onlyText: ContentSegment[] = [
      { type: "text", content: text.trimEnd() },
    ];
    segmentCache.set(cacheKey, onlyText);
    return onlyText;
  }

  const deduped = resolveOverlaps(matches);
  const segments = mergeThinking(
    buildSegments(text, deduped),
  );

  segmentCache.set(cacheKey, segments);
  return segments;
}

/** Attach structured tool_calls data to parsed segments.
 *  For Bash tools with multi-line commands, replaces truncated
 *  regex content with the full command from input_json and
 *  absorbs orphaned text fragments. */
export function enrichSegments(
  segments: ContentSegment[],
  toolCalls?: ToolCall[],
): ContentSegment[] {
  if (!toolCalls?.length) return segments;

  const result: ContentSegment[] = [];
  let tcIdx = 0;

  for (let i = 0; i < segments.length; i++) {
    const seg = segments[i]!;

    if (seg.type === "tool" && tcIdx < toolCalls.length) {
      const tc = toolCalls[tcIdx]!;
      tcIdx++;
      const enriched: ContentSegment = { ...seg, toolCall: tc };

      if (tc.category === "Bash" && tc.input_json) {
        try {
          const input = JSON.parse(tc.input_json);
          const fullCmd = input.command ?? input.cmd;
          if (fullCmd && fullCmd.includes("\n")) {
            enriched.content = `$ ${fullCmd}`;
            // Absorb orphaned text segments from truncated command
            while (i + 1 < segments.length) {
              const next = segments[i + 1]!;
              if (next.type !== "text") break;
              if (!next.content.trim() ||
                fullCmd.includes(next.content.trim())) {
                i++;
              } else {
                break;
              }
            }
          }
        } catch {
          /* fallback to regex content */
        }
      }

      result.push(enriched);
    } else {
      result.push(seg);
    }
  }

  // Append any remaining tool calls that weren't paired with text-based
  // tool segments. This covers both structured-only agents (pi/omp) where
  // no text markers exist, and mixed/edge cases where there are more
  // tool calls than text-based segments (e.g. false-positive text matches).
  if (tcIdx < toolCalls.length) {
    while (tcIdx < toolCalls.length) {
      const tc = toolCalls[tcIdx]!;
      tcIdx++;
      let content = "";
      if (tc.category === "Bash" && tc.input_json) {
        try {
          const input = JSON.parse(tc.input_json);
          const fullCmd = input.command ?? input.cmd;
          if (fullCmd) {
            content = `$ ${fullCmd}`;
          }
        } catch {
          /* leave empty, ToolBlock will use fallbackContent */
        }
      } else if (tc.category === "Read" && tc.input_json) {
        try {
          const input = JSON.parse(tc.input_json);
          const filePath = input.path ?? input.file_path;
          if (filePath) {
            content = String(filePath);
          } else if (input.pattern) {
            // Pi "find" tool mapped to Read — show the glob pattern
            content = String(input.pattern);
          }
        } catch {
          /* leave empty */
        }
      }
      result.push({
        type: "tool",
        content,
        label: TOOL_ALIASES[tc.tool_name] ?? tc.tool_name,
        toolCall: tc,
      });
    }
  }

  return result;
}

/**
 * Pure-function equivalent of the component-level visibility check.
 * Returns true when at least one segment of the message would be
 * rendered given the supplied visibility predicate.
 *
 * `isVisible` is called with a BlockType string -- the component
 * passes `ui.isBlockVisible`, but tests can supply any predicate.
 */
export function hasVisibleSegments(
  msg: Message,
  isVisible: (
    type: "user" | "assistant" | "thinking" | "tool" | "code",
  ) => boolean,
): boolean {
  const role: "user" | "assistant" =
    msg.role === "user" ? "user" : "assistant";
  const segs = enrichSegments(
    parseContent(msg.content, msg.has_tool_use),
    msg.tool_calls,
  );
  // Empty messages (e.g. initial assistant streaming state) should
  // remain visible when their role is not filtered out.
  if (segs.length === 0) return isVisible(role);
  return segs.some((s) => {
    if (s.type === "text") return isVisible(role);
    return isVisible(s.type);
  });
}
