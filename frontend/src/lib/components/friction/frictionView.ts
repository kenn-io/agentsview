import type {
  FrictionP0Alert,
  FrictionPatternItem,
  FrictionPersonaSummary,
  FrictionSignal,
} from "../../api/generated/index.js";

// Section order of jilog's render_digest (digest.rs:723-1117), then the two
// agentsview kinds spec §6.8 and §9.1 append (D36).
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

// Diagnostic subjects (spec §11.4) carry a producer-chosen identity, not a
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

// Spec §9.1 renders interruptions as one bullet per session in run order
// (`interruptions={n}`). The page mirrors that shape.
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

// Mirrors jilog dims_prefix (digest.rs:1064-1085): persona first, then seat,
// agent, machine. Values arrive display-sanitized from the server.
export function signalDims(
  signal: Pick<FrictionSignal, "persona" | "channel" | "seat" | "agent" | "machine">,
): string[] {
  const dims: string[] = [];
  if (signal.persona) {
    dims.push(signal.channel ? `${signal.persona}@${signal.channel}` : signal.persona);
  }
  if (signal.seat) dims.push(`seat:${signal.seat}`);
  if (signal.agent) dims.push(`agent:${signal.agent}`);
  if (signal.machine) dims.push(`machine:${signal.machine}`);
  return dims;
}

export function findingSummary(f: {
  kind: string;
  label: string;
  tool_name: string;
  text: string;
  evidence: string;
}): string {
  switch (f.kind) {
    case "error":
      return `${f.tool_name}: ${f.text}`;
    case "workaround":
      return `${f.label}: ${f.text}`;
    case "deferral":
      return f.label;
    case "pattern":
      return `${f.label}: ${f.evidence}`;
    case "interruption":
      return "";
    default:
      // correction and frustration carry their context in text.
      return f.text;
  }
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

export function personaRows(
  personas: Record<string, FrictionPersonaSummary> | null | undefined,
): { key: string; value: FrictionPersonaSummary }[] {
  return Object.entries(personas ?? {})
    .sort(([a], [b]) => byteCompare(a, b))
    .map(([key, value]) => ({ key, value }));
}

export function sortedCostEntries(
  costs: Record<string, string> | null | undefined,
): [string, string][] {
  return Object.entries(costs ?? {}).sort(([a], [b]) => byteCompare(a, b));
}

// Port of jilog format_usd (digest.rs:1120-1127) over the raw decimal strings
// the summary carries: pad to at least two decimals, never round.
export function formatUSD(value: string | null | undefined): string | null {
  if (value == null || value === "") return null;
  const dot = value.indexOf(".");
  if (dot === -1) return `$${value}.00`;
  const scale = value.length - dot - 1;
  return scale < 2 ? `$${value}${"0".repeat(2 - scale)}` : `$${value}`;
}
