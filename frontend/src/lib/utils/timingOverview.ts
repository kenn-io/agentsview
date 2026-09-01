import type { Message, ToolCall } from "../api/types.js";
import type { CallTiming, SessionTiming, TurnTiming } from "../api/types/timing.js";

export type TimingOverviewLane = "input" | "model" | "tools";

export interface TimingOverviewSpan {
  key: string;
  lane: TimingOverviewLane;
  startMs: number;
  endMs: number;
  recordedStartMs: number;
  recordedEndMs: number;
  ordinal: number;
  label: string;
  searchText: string;
  category: string | null;
  approximate: boolean;
  running: boolean;
  errored: boolean;
}

export interface TimingOverviewModel {
  startMs: number;
  endMs: number;
  spans: TimingOverviewSpan[];
}

export interface TimingOverviewRange {
  startMs: number;
  endMs: number;
}

export interface TimingOverviewOptions {
  sessionStartedAt?: string | null;
  sessionEndedAt?: string | null;
  hasEarlierMessages?: boolean;
  nowMs?: number;
}

interface ExecutionRange {
  startMs: number;
  endMs: number;
  errored: boolean;
}

function timestampMs(value: string | null | undefined): number | null {
  if (!value) return null;
  const valueMs = new Date(value).getTime();
  return Number.isFinite(valueMs) ? valueMs : null;
}

function executionRange(call: ToolCall | undefined): ExecutionRange | null {
  const events = call?.result_events ?? [];
  let startedAt: number | null = null;
  let completedAt: number | null = null;
  let errored = false;

  for (const event of events) {
    if (event.source !== "tool_execution") continue;
    const at = timestampMs(event.timestamp);
    if (at === null) continue;
    if (event.status === "started") {
      startedAt = startedAt === null ? at : Math.min(startedAt, at);
      continue;
    }
    if (event.status === "completed" || event.status === "errored") {
      if (completedAt === null || at >= completedAt) {
        completedAt = at;
        errored = event.status === "errored";
      }
    }
  }

  if (startedAt === null || completedAt === null || completedAt < startedAt) {
    return null;
  }
  return { startMs: startedAt, endMs: completedAt, errored };
}

function sourceToolCall(message: Message | undefined, timing: CallTiming): ToolCall | undefined {
  const calls = message?.tool_calls ?? [];
  if (timing.tool_use_id) {
    const exact = calls.find((call) => call.tool_use_id === timing.tool_use_id);
    if (exact) return exact;
  }
  return calls.length === 1 ? calls[0] : undefined;
}

function addMessageSpans(
  spans: TimingOverviewSpan[],
  messages: readonly Message[],
): Map<number, Message> {
  const messageByOrdinal = new Map<number, Message>();
  let previousTimestampMs: number | null = null;
  let orderedMessages = messages;
  for (let index = 1; index < messages.length; index++) {
    if (messages[index - 1]!.ordinal <= messages[index]!.ordinal) continue;
    orderedMessages = [...messages].sort((a, b) => a.ordinal - b.ordinal);
    break;
  }

  for (const message of orderedMessages) {
    if (message.is_system) continue;
    messageByOrdinal.set(message.ordinal, message);
    const at = timestampMs(message.timestamp);
    if (at === null) continue;

    if (message.role === "user") {
      spans.push({
        key: `input:${message.ordinal}`,
        lane: "input",
        startMs: at,
        endMs: at,
        recordedStartMs: at,
        recordedEndMs: at,
        ordinal: message.ordinal,
        label: "",
        searchText: `${message.content}\n${message.thinking_text}`,
        category: null,
        approximate: false,
        running: false,
        errored: false,
      });
    } else if (message.role === "assistant") {
      const startMs = previousTimestampMs === null ? at : Math.min(previousTimestampMs, at);
      spans.push({
        key: `model:${message.ordinal}`,
        lane: "model",
        startMs,
        endMs: at,
        recordedStartMs: startMs,
        recordedEndMs: at,
        ordinal: message.ordinal,
        label: message.model,
        searchText: `${message.model}\n${message.content}\n${message.thinking_text}`,
        category: null,
        approximate: false,
        running: false,
        errored: false,
      });
    }
    previousTimestampMs = at;
  }

  return messageByOrdinal;
}

function fallbackToolRange(
  turn: TurnTiming,
  call: CallTiming,
  timing: SessionTiming,
): Pick<TimingOverviewSpan, "startMs" | "endMs" | "approximate" | "running"> | null {
  const startMs = timestampMs(turn.started_at);
  if (startMs === null) return null;
  const durationMs = call.duration_ms ?? (turn.calls.length === 1 ? turn.duration_ms : null);
  if (durationMs === null) {
    return {
      startMs,
      endMs: startMs,
      approximate: true,
      running: timing.running && turn.duration_ms === null,
    };
  }
  return {
    startMs,
    endMs: startMs + Math.max(0, durationMs),
    approximate: true,
    running: false,
  };
}

function addToolSpans(
  spans: TimingOverviewSpan[],
  timing: SessionTiming,
  messageByOrdinal: ReadonlyMap<number, Message>,
  restrictToLoadedMessages: boolean,
): void {
  for (const turn of timing.turns) {
    if (restrictToLoadedMessages && !messageByOrdinal.has(turn.ordinal)) continue;
    const message = messageByOrdinal.get(turn.ordinal);
    for (const [index, call] of turn.calls.entries()) {
      const exact = executionRange(sourceToolCall(message, call));
      const fallback = exact === null ? fallbackToolRange(turn, call, timing) : null;
      const range = exact ?? fallback;
      if (range === null) continue;
      spans.push({
        key: `tool:${turn.ordinal}:${call.tool_use_id || index}`,
        lane: "tools",
        startMs: range.startMs,
        endMs: range.endMs,
        recordedStartMs: range.startMs,
        recordedEndMs: range.endMs,
        ordinal: turn.ordinal,
        label: call.tool_name,
        searchText: `${call.tool_name}\n${call.category}\n${call.input_preview}`,
        category: call.category,
        approximate: exact === null,
        running: exact === null && fallback?.running === true,
        errored: exact?.errored ?? false,
      });
    }
  }
}

/**
 * Builds the loaded-session timing projection used by the interactive overview.
 * User messages are point-in-time input markers, assistant messages span the
 * interval since the preceding visible message, and tool calls prefer recorded
 * execution events before falling back to the timing API's turn-level bounds.
 */
export function deriveTimingOverview(
  messages: readonly Message[],
  timing: SessionTiming,
  options: TimingOverviewOptions = {},
): TimingOverviewModel | null {
  const spans: TimingOverviewSpan[] = [];
  const messageByOrdinal = addMessageSpans(spans, messages);
  addToolSpans(
    spans,
    timing,
    messageByOrdinal,
    options.hasEarlierMessages === true && messageByOrdinal.size > 0,
  );
  if (spans.length === 0) return null;

  let startMs = Number.POSITIVE_INFINITY;
  let endMs = Number.NEGATIVE_INFINITY;
  for (const span of spans) {
    startMs = Math.min(startMs, span.startMs);
    endMs = Math.max(endMs, span.endMs);
  }
  const sessionStartMs = timestampMs(options.sessionStartedAt);
  const sessionEndMs = timestampMs(options.sessionEndedAt);

  if (options.hasEarlierMessages !== true && sessionStartMs !== null) {
    startMs = Math.min(startMs, sessionStartMs);
  }
  if (sessionEndMs !== null) {
    endMs = Math.max(endMs, sessionEndMs);
  } else if (timing.running && Number.isFinite(options.nowMs)) {
    endMs = Math.max(endMs, options.nowMs!);
  }
  if (endMs <= startMs) endMs = startMs + 1;

  return {
    startMs,
    endMs,
    spans: spans.sort(
      (a, b) => a.startMs - b.startMs || a.endMs - b.endMs || a.ordinal - b.ordinal,
    ),
  };
}

export type TimingOverviewProjectionMode = "duration" | "sequence";

/** Projects source timestamps into either idle-compressed duration or equal-width sequence space. */
export function projectTimingOverview(
  model: TimingOverviewModel,
  mode: TimingOverviewProjectionMode,
): TimingOverviewModel {
  if (mode === "sequence") {
    return {
      startMs: 0,
      endMs: model.spans.length,
      spans: model.spans.map((span, index) => ({
        ...span,
        startMs: index,
        endMs: index + 1,
      })),
    };
  }

  const removedIdleByKey = new Map<string, number>();
  let removedIdleMs = 0;
  let coveredUntilMs: number | null = null;
  for (const span of model.spans) {
    if (coveredUntilMs !== null && span.startMs > coveredUntilMs) {
      removedIdleMs += span.startMs - coveredUntilMs;
    }
    removedIdleByKey.set(span.key, removedIdleMs);
    coveredUntilMs = coveredUntilMs === null ? span.endMs : Math.max(coveredUntilMs, span.endMs);
  }

  let startMs = Number.POSITIVE_INFINITY;
  let endMs = Number.NEGATIVE_INFINITY;
  const spans = model.spans.map((span) => {
    const offset = removedIdleByKey.get(span.key) ?? 0;
    const projected = {
      ...span,
      startMs: span.startMs - offset,
      endMs: span.endMs - offset,
    };
    startMs = Math.min(startMs, projected.startMs);
    endMs = Math.max(endMs, projected.endMs);
    return projected;
  });

  if (endMs <= startMs) endMs = startMs + 1;
  return { startMs, endMs, spans };
}

export function orderedTimingOverviewRange(leftMs: number, rightMs: number): TimingOverviewRange {
  return leftMs <= rightMs
    ? { startMs: leftMs, endMs: rightMs }
    : { startMs: rightMs, endMs: leftMs };
}

export function timingOverviewSpanIntersects(
  span: TimingOverviewSpan,
  range: TimingOverviewRange,
): boolean {
  return span.startMs <= range.endMs && span.endMs >= range.startMs;
}

export function timingOverviewFocusOrdinals(
  model: TimingOverviewModel,
  range: TimingOverviewRange,
): number[] {
  return [
    ...new Set(
      model.spans
        .filter((span) => timingOverviewSpanIntersects(span, range))
        .map((span) => span.ordinal),
    ),
  ].sort((a, b) => a - b);
}

export function nearestTimingOverviewSpan(
  model: TimingOverviewModel,
  atMs: number,
): TimingOverviewSpan {
  return model.spans.reduce((nearest, span) => {
    const nearestDistance =
      atMs < nearest.startMs
        ? nearest.startMs - atMs
        : atMs > nearest.endMs
          ? atMs - nearest.endMs
          : 0;
    const spanDistance =
      atMs < span.startMs ? span.startMs - atMs : atMs > span.endMs ? atMs - span.endMs : 0;
    return spanDistance < nearestDistance ? span : nearest;
  });
}
