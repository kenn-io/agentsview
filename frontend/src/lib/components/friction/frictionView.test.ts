import { describe, expect, it } from "vite-plus/test";
import type {
  FrictionPatternItem,
  FrictionPersonaSummary,
  FrictionSignal,
} from "../../api/generated/index.js";
import {
  FRICTION_KINDS,
  findingSummary,
  formatUSD,
  groupSignals,
  headlinePatterns,
  interruptionRows,
  isDiagnosticSubject,
  personaRows,
  sessionLinkParams,
  signalDims,
  sortedCostEntries,
  sortedP0,
} from "./frictionView.js";

function sig(overrides: Partial<FrictionSignal> = {}): FrictionSignal {
  return {
    kind: "correction",
    detector: "correction.coding",
    subject_id: "session-a",
    subject_kind: "session",
    title: "[friction/correction] session-a: no, use the other flag",
    fingerprint: `fl1:${"a".repeat(64)}`,
    text: "no, use the other flag",
    tool_name: "",
    label: "",
    evidence: "",
    message_ordinal: 4,
    call_index: null,
    occurred_at: null,
    seat: "",
    agent: "",
    machine: "",
    persona: "",
    channel: "",
    session_url: "",
    ...overrides,
  } as FrictionSignal;
}

function pattern(overrides: Partial<FrictionPatternItem> = {}): FrictionPatternItem {
  return {
    fingerprint: `fl1:${"a".repeat(64)}`,
    kind: "correction",
    title: "[friction/correction] session-a: no, use the other flag",
    first_seen_date: "2026-09-21",
    last_seen_date: "2026-09-21",
    occurrence_count: 1,
    session_count: 1,
    last_subject_id: "session-a",
    last_ordinal: 4,
    ...overrides,
  } as FrictionPatternItem;
}

describe("groupSignals", () => {
  it("returns all seven kinds in digest section order, keeping run order inside a kind", () => {
    const sections = groupSignals([
      sig({ kind: "interruption", subject_id: "i1" }),
      sig({ kind: "pattern", subject_id: "p1" }),
      sig({ kind: "correction", subject_id: "c2" }),
      sig({ kind: "frustration", subject_id: "f1" }),
      sig({ kind: "correction", subject_id: "c1" }),
      sig({ kind: "error", subject_id: "e1" }),
    ]);
    expect(sections.map((s) => s.kind)).toEqual([
      "correction",
      "error",
      "workaround",
      "deferral",
      "pattern",
      "frustration",
      "interruption",
    ]);
    expect(sections.map((s) => s.kind)).toEqual([...FRICTION_KINDS]);
    expect(sections[0]!.signals.map((s) => s.subject_id)).toEqual(["c2", "c1"]);
    expect(sections[1]!.signals.map((s) => s.subject_id)).toEqual(["e1"]);
    expect(sections[2]!.signals).toEqual([]);
    expect(sections[4]!.signals.map((s) => s.subject_id)).toEqual(["p1"]);
    expect(sections[5]!.signals.map((s) => s.subject_id)).toEqual(["f1"]);
    expect(sections[6]!.signals.map((s) => s.subject_id)).toEqual(["i1"]);
  });

  it("drops kinds the page does not know instead of throwing", () => {
    const sections = groupSignals([sig({ kind: "future_kind" })]);
    expect(sections.every((s) => s.signals.length === 0)).toBe(true);
  });
});

describe("interruptionRows", () => {
  it("collapses interruptions to one row per session in run order, anchored on the first", () => {
    const rows = interruptionRows([
      sig({ kind: "interruption", subject_id: "s2", message_ordinal: 7, text: "" }),
      sig({ kind: "correction", subject_id: "s1" }),
      sig({ kind: "interruption", subject_id: "s1", message_ordinal: 3, text: "" }),
      sig({ kind: "interruption", subject_id: "s2", message_ordinal: 11, text: "" }),
    ]);
    expect(rows.map((r) => [r.subjectId, r.count, r.first.message_ordinal])).toEqual([
      ["s2", 2, 7],
      ["s1", 1, 3],
    ]);
    expect(interruptionRows([])).toEqual([]);
  });
});

describe("isDiagnosticSubject", () => {
  it.each([
    ["diagnostic subject", "diagnostic", true],
    ["session subject", "session", false],
    ["missing subject kind (older server)", undefined, false],
    ["null subject kind", null, false],
  ])("%s", (_name, subjectKind, expected) => {
    expect(isDiagnosticSubject({ subject_kind: subjectKind })).toBe(expected);
  });
});

describe("sessionLinkParams", () => {
  it.each([
    ["ordinal present", 4, { msg: "4" }],
    ["ordinal zero", 0, { msg: "0" }],
    ["ordinal null (session-level pattern)", null, {}],
    ["ordinal missing", undefined, {}],
  ])("%s", (_name, ordinal, expected) => {
    expect(sessionLinkParams({ message_ordinal: ordinal })).toEqual(expected);
  });
});

describe("signalDims", () => {
  it.each([
    ["no dims", {}, []],
    ["persona with channel", { persona: "helper", channel: "general" }, ["helper@general"]],
    ["persona without channel", { persona: "helper" }, ["helper"]],
    ["channel without persona is ignored", { channel: "general" }, []],
    [
      "all dims in dims_prefix order",
      {
        persona: "helper",
        channel: "general",
        seat: "seat-02",
        agent: "claude",
        machine: "laptop",
      },
      ["helper@general", "seat:seat-02", "agent:claude", "machine:laptop"],
    ],
  ])("%s", (_name, dims, expected) => {
    expect(signalDims(sig(dims))).toEqual(expected);
  });
});

describe("findingSummary", () => {
  it.each([
    ["correction", sig({ kind: "correction", text: "no, stop" }), "no, stop"],
    ["error", sig({ kind: "error", tool_name: "bash", text: "exit 1" }), "bash: exit 1"],
    [
      "workaround",
      sig({ kind: "workaround", label: "for now", text: "hardcode it for now" }),
      "for now: hardcode it for now",
    ],
    ["deferral", sig({ kind: "deferral", label: "next session", text: "" }), "next session"],
    [
      "pattern",
      sig({
        kind: "pattern",
        label: "retry_loop",
        evidence: "`bash` x3 identical arguments 01:35-01:54",
      }),
      "retry_loop: `bash` x3 identical arguments 01:35-01:54",
    ],
    [
      "frustration",
      sig({ kind: "frustration", text: "this is still broken" }),
      "this is still broken",
    ],
    ["interruption", sig({ kind: "interruption", text: "" }), ""],
  ])("%s", (_name, signal, expected) => {
    expect(findingSummary(signal)).toBe(expected);
  });
});

describe("headlinePatterns", () => {
  it("keeps only the digest's fingerprints, splits new from recurring, and ranks by occurrences", () => {
    const fpNew = `fl1:${"1".repeat(64)}`;
    const fpOld = `fl1:${"2".repeat(64)}`;
    const fpBig = `fl1:${"3".repeat(64)}`;
    const fpElsewhere = `fl1:${"4".repeat(64)}`;
    const result = headlinePatterns(
      "2026-09-21",
      [
        sig({ fingerprint: fpNew, subject_id: "s-new", message_ordinal: 2 }),
        sig({ fingerprint: fpOld, subject_id: "s-old-1", message_ordinal: 5 }),
        sig({ fingerprint: fpBig, subject_id: "s-big", message_ordinal: null }),
        sig({ fingerprint: fpOld, subject_id: "s-old-2", message_ordinal: 9 }),
      ],
      [
        pattern({ fingerprint: fpOld, first_seen_date: "2026-09-10", occurrence_count: 3 }),
        pattern({ fingerprint: fpNew, first_seen_date: "2026-09-21", occurrence_count: 1 }),
        pattern({ fingerprint: fpBig, first_seen_date: "2026-08-01", occurrence_count: 9 }),
        pattern({ fingerprint: fpElsewhere, first_seen_date: "2026-09-21", occurrence_count: 50 }),
      ],
    );
    expect(result.fresh.map((p) => p.fingerprint)).toEqual([fpNew]);
    expect(result.recurring.map((p) => p.fingerprint)).toEqual([fpBig, fpOld]);
    expect(result.fresh[0]!.isNew).toBe(true);
    expect(result.recurring[0]!.isNew).toBe(false);
    // The headline links to the first occurrence in this digest, not to the
    // pattern's last subject, which may belong to a later digest.
    expect(result.recurring[1]!.anchor?.subject_id).toBe("s-old-1");
    expect(result.recurring[1]!.anchor?.message_ordinal).toBe(5);
  });

  it("breaks occurrence ties by latest last_seen_date then fingerprint", () => {
    const a = pattern({
      fingerprint: "fl1:a",
      first_seen_date: "2026-09-01",
      last_seen_date: "2026-09-20",
      occurrence_count: 2,
    });
    const b = pattern({
      fingerprint: "fl1:b",
      first_seen_date: "2026-09-01",
      last_seen_date: "2026-09-21",
      occurrence_count: 2,
    });
    const c = pattern({
      fingerprint: "fl1:c",
      first_seen_date: "2026-09-01",
      last_seen_date: "2026-09-21",
      occurrence_count: 2,
    });
    const result = headlinePatterns(
      "2026-09-21",
      [sig({ fingerprint: "fl1:a" }), sig({ fingerprint: "fl1:b" }), sig({ fingerprint: "fl1:c" })],
      [a, c, b],
    );
    expect(result.recurring.map((p) => p.fingerprint)).toEqual(["fl1:b", "fl1:c", "fl1:a"]);
  });
});

describe("sortedP0", () => {
  it("sorts alerts by tool and tolerates null", () => {
    expect(sortedP0(null)).toEqual([]);
    expect(
      sortedP0([
        { tool: "read", subject_ids: ["s1", "s2", "s3"] },
        { tool: "bash", subject_ids: ["s1", "s2", "s3"] },
      ]).map((a) => a.tool),
    ).toEqual(["bash", "read"]);
  });
});

describe("personaRows and sortedCostEntries", () => {
  const counts: FrictionPersonaSummary = {
    persona: "helper",
    channel: null,
    sessions: 1,
    corrections: 0,
    errors: 0,
    workarounds: 0,
    deferrals: 0,
    patterns: 0,
    input_tokens: 0,
    output_tokens: 0,
    cost_usd: null,
  } as FrictionPersonaSummary;

  it("sorts persona keys by byte order like jilog's BTreeMap", () => {
    const rows = personaRows({ "helper@general": counts, Zeta: counts, "helper (2)": counts });
    expect(rows.map((r) => r.key)).toEqual(["Zeta", "helper (2)", "helper@general"]);
    expect(personaRows(undefined)).toEqual([]);
  });

  it("sorts cost keys ascending", () => {
    expect(sortedCostEntries({ subagent: "3.00", "(root)": "1.20" })).toEqual([
      ["(root)", "1.20"],
      ["subagent", "3.00"],
    ]);
    expect(sortedCostEntries(null)).toEqual([]);
  });
});

describe("formatUSD", () => {
  // Ported from jilog digest.rs format_usd_pads_cents_but_keeps_subcent_precision.
  it.each([
    ["4.2", "$4.20"],
    ["7", "$7.00"],
    ["0.0003", "$0.0003"],
    ["4.20", "$4.20"],
    ["332.138392", "$332.138392"],
    ["1300.250000", "$1300.250000"],
  ])("%s -> %s", (input, expected) => {
    expect(formatUSD(input)).toBe(expected);
  });

  it.each([[null], [undefined], [""]])("returns null for %s", (input) => {
    expect(formatUSD(input)).toBeNull();
  });
});
