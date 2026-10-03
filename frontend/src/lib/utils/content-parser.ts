import type { DbMessage as Message, DbToolCall as ToolCall } from "../api/generated/index.js";
import { LRUCache } from "./cache.js";
import { isSystemBoundaryMessage } from "./messages.js";
import { resolveToolInputText } from "../search/tool-input.js";

export type SegmentType = "text" | "thinking" | "tool" | "tool_result" | "code" | "skill";

export interface ContentSegment {
  type: SegmentType;
  content: string;
  /** Tool name or code language, when applicable */
  label?: string;
  /** Structured tool call data from the API, when available */
  toolCall?: ToolCall;
  /** Stable structured-call index, independent of the surrounding text. */
  callIndex?: number;
  /** Original native text run, owned by its first display segment for copy. */
  copyContent?: string;
}

/**
 * Marked thinking blocks use explicit [/Thinking] delimiters.
 * Tried first; captures everything between markers.
 */
const THINKING_MARKED_RE = /\[Thinking\]\n?([\s\S]*?)\n?\[\/Thinking\]/g;

/**
 * Legacy thinking blocks without end markers.
 * Used as fallback for old data that predates [/Thinking].
 */
const THINKING_LEGACY_RE = /\[Thinking\]\n?([\s\S]*?)(?=\n\[|\n\n|$)/g;

/**
 * Skill blocks use [Skill: name]...[/Skill] delimiters,
 * same pattern as thinking blocks.
 */
const SKILL_RE = /\[Skill: (.+?)\]\n?([\s\S]*?)\n?\[\/Skill\]/g;

const TOOL_NAMES =
  "Tool|Read|Write|Edit|Patch|Bash|Glob|Grep|Other|TaskCreate|TaskUpdate|TaskGet|TaskList|Task|Agent|Skill|" +
  "SendMessage|Question|Todo List|Entering Plan Mode|" +
  "Exiting Plan Mode|exec_command|shell_command|" +
  "write_stdin|apply_patch|ApplyPatch|shell|parallel|view_image|" +
  "request_user_input|update_plan";

const TOOL_ALIASES: Record<string, string> = {
  Agent: "Task",
  exec_command: "Bash",
  shell_command: "Bash",
  write_stdin: "Bash",
  shell: "Bash",
  apply_patch: "Edit",
  ApplyPatch: "Edit",
  Patch: "Edit",
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

const TOOL_RE = new RegExp(`\\[(${TOOL_NAMES})([^\\]]*)\\]([\\s\\S]*?)(?=\\n\\[|\\n\\n|$)`, "g");

/** Returns true if text[from..to) contains a backtick run of
 *  exactly `len` characters. Used to detect a closing inline
 *  code delimiter on the same line as the opener. */
function hasRunBefore(text: string, from: number, to: number, len: number): boolean {
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
 * separately by codeBlockMatches.
 */
function scanInlineCodeSpans(text: string): Array<[number, number]> {
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
    if (runLen >= 3 && (openStart === 0 || text[openStart - 1] === "\n")) {
      const nl = text.indexOf("\n", i);
      if (nl >= 0 && !hasRunBefore(text, i, nl, runLen)) {
        continue;
      }
    }

    // Scan for a closing run of exactly the same length.
    for (let j = i; j < text.length; j++) {
      if (text[j] !== "`") continue;
      const closeStart = j;
      while (j < text.length && text[j] === "`") j++;
      if (j - closeStart === runLen) {
        spans.push([openStart, j]);
        i = j;
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

const segmentCache = new LRUCache<string, ContentSegment[]>(500);
const messageCache = new LRUCache<string, { identity: symbol; segments: ContentSegment[] }>(500);
const identityCache = new LRUCache<string, { fields: unknown[]; identity: symbol }>(500);

/**
 * Compare immutable strings directly and snapshot nested scalar fields so
 * in-place edits invalidate caches without serializing entire transcripts.
 */
export function messageContentIdentity(message: Message): symbol {
  const layout = message.content_layout;
  const fields: unknown[] = [
    message.content,
    message.thinking_text,
    message.tool_result_text,
    message.has_tool_use,
    message.has_thinking,
    message.role,
    message.is_system,
    message.source_subtype,
    message.is_compact_boundary,
    message.ordinal,
    layout == null,
    layout?.version,
    layout?.blocks?.length,
    message.tool_calls?.length,
  ];
  for (const block of layout?.blocks ?? []) {
    fields.push(block.kind, block.start, block.end, block.call_index, block.category);
  }
  for (const call of message.tool_calls ?? []) {
    fields.push(
      call.tool_name,
      call.category,
      call.rendering,
      call.input_json,
      call.result_content,
      call.result_content_length,
      call.skill_name,
      call.subagent_session_id,
      call.tool_use_id,
      call.result_events?.length,
    );
    for (const event of call.result_events ?? []) {
      fields.push(
        event.agent_id,
        event.content,
        event.content_length,
        event.event_index,
        event.source,
        event.status,
        event.subagent_session_id,
        event.timestamp,
        event.tool_use_id,
      );
    }
  }
  const key = `${message.session_id}:${message.id}`;
  const cached = identityCache.get(key);
  if (
    cached?.fields.length === fields.length &&
    fields.every((field, index) => Object.is(field, cached.fields[index]))
  )
    return cached.identity;
  const identity = Symbol("message content");
  identityCache.set(key, { fields, identity });
  return identity;
}

/** Map only valid UTF-8 boundaries to JavaScript's UTF-16 string positions. */
function utf8Slice(source: string): (start: number, end: number) => string {
  const boundaries = new Map<number, number>([[0, 0]]);
  let bytes = 0;
  let units = 0;
  for (const character of source) {
    const point = character.codePointAt(0)!;
    bytes += point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
    units += character.length;
    boundaries.set(bytes, units);
  }
  return (start, end) => {
    if (
      !Number.isInteger(start) ||
      !Number.isInteger(end) ||
      start > end ||
      !boundaries.has(start) ||
      !boundaries.has(end)
    ) {
      throw new Error("Invalid UTF-8 content layout range");
    }
    return source.slice(boundaries.get(start)!, boundaries.get(end)!);
  };
}

/** One transcript adapter: native owners use saved order; NULL stays legacy. */
export function messageSegments(message: Message): ContentSegment[] {
  const identity = messageContentIdentity(message);
  const key = `${message.session_id}:${message.id}`;
  const cached = messageCache.get(key);
  if (cached?.identity === identity) return cached.segments;

  let segments: ContentSegment[];
  const layout = message.content_layout;
  if (layout == null) {
    segments = enrichSegments(
      parseContent(message.content, message.has_tool_use),
      message.tool_calls,
    );
  } else {
    if (layout.version !== 1 || !Array.isArray(layout.blocks)) {
      throw new Error("Unsupported content layout version");
    }
    const text = utf8Slice(message.content);
    const thinking = utf8Slice(message.thinking_text);
    const output = utf8Slice(message.tool_result_text);
    const attached = new Set<number>();
    segments = [];
    for (let index = 0; index < layout.blocks.length; index++) {
      const block = layout.blocks[index]!;
      switch (block.kind) {
        case "text": {
          let end = block.end;
          while (layout.blocks[index + 1]?.kind === "text") {
            const next = layout.blocks[++index]!;
            text(next.start, next.end); // Validate each original range too.
            end = next.end;
          }
          const content = text(block.start, end);
          const parsed = buildSegments(content, resolveOverlaps(codeBlockMatches(content)));
          if (parsed.length === 0) parsed.push({ type: "text", content });
          parsed.forEach((segment, part) => {
            segments.push({ ...segment, copyContent: part === 0 ? content : "" });
          });
          break;
        }
        case "thinking":
          segments.push({ type: "thinking", content: thinking(block.start, block.end) });
          break;
        case "tool_result":
          segments.push({ type: "tool_result", content: output(block.start, block.end) });
          break;
        case "tool_call": {
          const call = message.tool_calls?.[block.call_index];
          if (!Number.isInteger(block.call_index) || !call || attached.has(block.call_index)) {
            throw new Error("Invalid content layout tool call");
          }
          attached.add(block.call_index);
          segments.push({
            type: "tool",
            content: call.rendering || resolveToolInputText(call, ""),
            label: TOOL_ALIASES[call.tool_name] ?? (call.category || call.tool_name),
            toolCall: call,
            callIndex: block.call_index,
          });
          break;
        }
        default:
          throw new Error("Unsupported content layout block kind");
      }
    }
  }
  messageCache.set(key, { identity, segments });
  return segments;
}

/** Clear both content-parser caches. Call on session switch
 *  to prevent cross-session memory accumulation. */
export function clearContentCaches(): void {
  segmentCache.clear();
  messageCache.clear();
  identityCache.clear();
}

/** Returns true if the message contains only tool calls (no text) */
export function isToolOnly(msg: Message): boolean {
  if (msg.role !== "assistant") return false;
  if (msg.content_layout == null && !msg.has_tool_use) return false;
  const segments = messageSegments(msg);
  // A standalone output is rendered as its own message, not a zero-call group.
  if (msg.content_layout != null && !segments.some((segment) => segment.type === "tool")) {
    return false;
  }
  return segments.every(
    (segment) =>
      segment.type === "tool" ||
      segment.type === "thinking" ||
      segment.type === "tool_result" ||
      (segment.type === "text" && segment.content.trim() === ""),
  );
}

/** Returns true if pos falls inside any inline code span. */
function insideInlineCode(pos: number, spans: Array<[number, number]>): boolean {
  return spans.some(([s, e]) => pos > s && pos < e);
}

function atFenceLineStart(text: string, pos: number): boolean {
  const lineStart = text.lastIndexOf("\n", pos - 1) + 1;
  return /^[ \t]{0,3}$/.test(text.slice(lineStart, pos));
}

function countBackticks(text: string, pos: number): number {
  let end = pos;
  while (end < text.length && text[end] === "`") end++;
  return end - pos;
}

function closingFence(
  text: string,
  contentStart: number,
  fenceLen: number,
): { contentEnd: number; end: number } | undefined {
  let pos = contentStart;
  while (pos < text.length) {
    const tickStart = text.indexOf("`", pos);
    if (tickStart < 0) return undefined;
    const lineStart = text.lastIndexOf("\n", tickStart - 1) + 1;
    const nextLineStart = text.indexOf("\n", tickStart);
    const lineEnd = nextLineStart >= 0 ? nextLineStart : text.length;

    const tickCount = countBackticks(text, tickStart);
    const rest = text.slice(tickStart + tickCount, lineEnd);
    if (tickCount >= fenceLen && atFenceLineStart(text, tickStart) && /^[ \t]*$/.test(rest)) {
      return { contentEnd: lineStart, end: lineEnd };
    }

    pos = tickStart + tickCount;
  }
  return undefined;
}

function codeBlockMatches(text: string): Match[] {
  const matches: Match[] = [];
  let pos = 0;

  while (pos < text.length) {
    const start = text.indexOf("```", pos);
    if (start < 0) break;

    if (!atFenceLineStart(text, start)) {
      pos = start + 1;
      continue;
    }

    const fenceLen = countBackticks(text, start);
    const infoStart = start + fenceLen;
    const lineEnd = text.indexOf("\n", infoStart);
    if (lineEnd < 0) break;

    const info = text.slice(infoStart, lineEnd);
    if (info.includes("`")) {
      pos = infoStart;
      continue;
    }

    const contentStart = lineEnd + 1;
    const close = closingFence(text, contentStart, fenceLen);
    if (close === undefined) {
      pos = infoStart;
      continue;
    }

    matches.push({
      start,
      end: close.end,
      segment: {
        type: "code",
        content: text.slice(contentStart, close.contentEnd),
        label: info.trim() || undefined,
      },
    });
    pos = close.end;
  }

  return matches;
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
    const overlaps = matches.some((o) => start >= o.start && start < o.end);
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

  // Skill blocks
  for (const m of text.matchAll(SKILL_RE)) {
    const start = m.index!;
    const end = start + m[0].length;
    if (insideInlineCode(start, codeSpans)) continue;
    const overlaps = matches.some((o) => start >= o.start && start < o.end);
    if (overlaps) continue;
    matches.push({
      start,
      end,
      segment: {
        type: "skill",
        content: (m[2] ?? "").trim(),
        label: m[1] ?? "",
      },
    });
  }

  if (parseTools) {
    for (const m of text.matchAll(TOOL_RE)) {
      if (insideInlineCode(m.index!, codeSpans)) continue;
      const toolName = m[1] ?? "";
      const toolArgs = (m[2] ?? "").trim();
      const displayName = TOOL_ALIASES[toolName] ?? toolName;
      const label = toolArgs ? `${displayName} ${toolArgs}` : displayName;
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

  for (const m of codeBlockMatches(text)) {
    const idx = m.start;
    const insideOther = matches.some((o) => idx >= o.start && idx < o.end);
    if (insideOther) continue;

    matches.push(m);
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

function buildSegments(text: string, matches: Match[]): ContentSegment[] {
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

function mergeThinking(segments: ContentSegment[]): ContentSegment[] {
  const result: ContentSegment[] = [];
  for (const seg of segments) {
    const prev = result[result.length - 1];
    if (seg.type === "thinking" && prev?.type === "thinking") {
      prev.content += "\n\n" + seg.content;
    } else {
      result.push({ ...seg });
    }
  }
  return result;
}

/** Parse message content into typed segments.
 *  Pass messageId and contentLength to enable LRU caching
 *  keyed by ID instead of by full text (avoids storing huge
 *  strings as Map keys). contentLength ensures the cache
 *  invalidates when message content grows during streaming. */
export function parseContent(
  text: string,
  hasToolUse = true,
  messageId?: number,
  contentLength?: number,
): ContentSegment[] {
  if (!text) return [];

  const cacheKey =
    messageId !== undefined
      ? `${hasToolUse ? "t" : "n"}:${messageId}:${contentLength ?? text.length}`
      : undefined;

  if (cacheKey) {
    const cached = segmentCache.get(cacheKey);
    if (cached) return cached;
  }

  const matches = extractMatches(text, hasToolUse);

  if (matches.length === 0) {
    const onlyText: ContentSegment[] = [{ type: "text", content: text.trimEnd() }];
    if (cacheKey) segmentCache.set(cacheKey, onlyText);
    return onlyText;
  }

  const deduped = resolveOverlaps(matches);
  const segments = mergeThinking(buildSegments(text, deduped));

  if (cacheKey) segmentCache.set(cacheKey, segments);
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
              if (!next.content.trim() || fullCmd.includes(next.content.trim())) {
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
  isVisible: (type: "user" | "assistant" | "thinking" | "tool" | "code" | "system") => boolean,
): boolean {
  // A boundary card shows a label and a one-line preview, never the parsed
  // segments of its body, so its own block type owns its visibility. Parsing
  // the body here would let an unrelated toggle hide the card, and would tie
  // every boundary card to the "user" role it carries for analytics.
  if (isSystemBoundaryMessage(msg)) return isVisible("system");
  const role: "user" | "assistant" = msg.role === "user" ? "user" : "assistant";
  const segs = messageSegments(msg);
  // Empty messages (e.g. initial assistant streaming state) should
  // remain visible when their role is not filtered out.
  if (segs.length === 0) return msg.content_layout == null && isVisible(role);
  return segs.some((s) => {
    if (s.type === "text") return isVisible(role);
    if (s.type === "skill") return isVisible(role);
    if (s.type === "tool_result") return isVisible("tool");
    return isVisible(s.type);
  });
}
