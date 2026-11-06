import type {
  FrictionP0Alert,
  FrictionPatternItem,
  FrictionSession,
  FrictionSignal,
} from "../../api/generated/index.js";
import { normalizeMessagePreview } from "../../utils/messages.js";

// Section order of the rendered Markdown digest (friction.RenderMarkdown).
export const FRICTION_KINDS = [
  "correction",
  "error",
  "workaround",
  "deferral",
  "pattern",
  "frustration",
  "interruption",
] as const;
export type FrictionKind = (typeof FRICTION_KINDS)[number];

function byteCompare(a: string, b: string): number {
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}

// fileableKind mirrors the server: frustration quotes the user and
// interruptions carry no text, so neither is filed to Kata.
export function fileableKind(kind: string): boolean {
  return ["correction", "error", "workaround", "deferral", "pattern"].includes(kind);
}

// Diagnostic subjects carry a producer-chosen identity, not a
// session id, so the page never deep-links them.
export function isDiagnosticSubject(target: { subject_kind?: string | null }): boolean {
  return target.subject_kind === "diagnostic";
}

export interface SignalSection {
  kind: FrictionKind;
  signals: FrictionSignal[];
}

export function groupSignals(signals: readonly FrictionSignal[]): SignalSection[] {
  const byKind = new Map<string, FrictionSignal[]>(FRICTION_KINDS.map((kind) => [kind, []]));
  for (const signal of signals) {
    byKind.get(signal.kind)?.push(signal);
  }
  return FRICTION_KINDS.map((kind) => ({ kind, signals: byKind.get(kind)! }));
}

export interface InterruptionRow {
  subjectId: string;
  count: number;
  first: FrictionSignal;
}

// The Markdown digest renders interruptions as one bullet per session in run
// order (`interruptions={n}`). The page mirrors that shape.
export function interruptionRows(signals: readonly FrictionSignal[]): InterruptionRow[] {
  const rows = new Map<string, InterruptionRow>();
  for (const signal of signals) {
    if (signal.kind !== "interruption") continue;
    const row = rows.get(signal.subject_id);
    if (row) {
      row.count += 1;
    } else {
      rows.set(signal.subject_id, { subjectId: signal.subject_id, count: 1, first: signal });
    }
  }
  return [...rows.values()];
}

export function sessionLinkParams(target: {
  message_ordinal?: number | null;
}): Record<string, string> {
  return target.message_ordinal == null ? {} : { msg: String(target.message_ordinal) };
}

// Producer dimensions that say who or where a finding came from. The
// session's agent is shown with the session, so it is not repeated here.
// Values arrive display-sanitized from the server.
export function signalDims(signal: Pick<FrictionSignal, "seat" | "machine">): string[] {
  const dims: string[] = [];
  if (signal.seat) dims.push(signal.seat);
  if (signal.machine) dims.push(signal.machine);
  return dims;
}

export type FrictionSessions = Record<string, FrictionSession>;

const SESSION_LABEL_CHARS = 90;

// sessionLabel names a session the way the sidebar does: its custom name,
// else the opening message, else its project. The raw ID is the last resort,
// for sessions no longer in the archive.
export function sessionLabel(id: string, sessions: FrictionSessions | null | undefined): string {
  const info = sessions?.[id];
  if (!info) return id;
  if (info.display_name) return info.display_name;
  const preview = normalizeMessagePreview(info.first_message).replace(/\s+/g, " ").trim();
  if (preview) {
    return preview.length > SESSION_LABEL_CHARS
      ? `${preview.slice(0, SESSION_LABEL_CHARS).trimEnd()}…`
      : preview;
  }
  return info.project || id;
}

export interface TextSegment {
  text: string;
  code: boolean;
}

// codeSegments splits detector text on backticks so `tool` spans render as
// code. An unmatched backtick stays literal.
export function codeSegments(text: string): TextSegment[] {
  const parts = text.split("`");
  if (parts.length % 2 === 0) return [{ text, code: false }];
  return parts
    .map((part, i) => ({ text: part, code: i % 2 === 1 }))
    .filter((segment) => segment.text !== "");
}

// Producers prefix diagnostic text with the identity the row already shows.
export function withoutSubjectPrefix(text: string, subjectId: string): string {
  const prefix = `${subjectId}: `;
  return text.startsWith(prefix) ? text.slice(prefix.length) : text;
}

export interface HeadlinePattern extends FrictionPatternItem {
  isNew: boolean;
  anchor: FrictionSignal | null;
}

export function headlinePatterns(
  date: string,
  signals: readonly FrictionSignal[],
  patterns: readonly FrictionPatternItem[],
): { fresh: HeadlinePattern[]; recurring: HeadlinePattern[] } {
  const firstByFingerprint = new Map<string, FrictionSignal>();
  for (const signal of signals) {
    if (!firstByFingerprint.has(signal.fingerprint)) {
      firstByFingerprint.set(signal.fingerprint, signal);
    }
  }
  const rows: HeadlinePattern[] = patterns
    .filter((p) => firstByFingerprint.has(p.fingerprint))
    .map((p) => ({
      ...p,
      isNew: p.first_seen_date === date,
      anchor: firstByFingerprint.get(p.fingerprint) ?? null,
    }));
  rows.sort(
    (a, b) =>
      b.occurrence_count - a.occurrence_count ||
      byteCompare(b.last_seen_date, a.last_seen_date) ||
      byteCompare(a.fingerprint, b.fingerprint),
  );
  return {
    fresh: rows.filter((r) => r.isNew),
    recurring: rows.filter((r) => !r.isNew),
  };
}

export function sortedP0(alerts: readonly FrictionP0Alert[] | null | undefined): FrictionP0Alert[] {
  return [...(alerts ?? [])].sort((a, b) => byteCompare(a.tool, b.tool));
}
