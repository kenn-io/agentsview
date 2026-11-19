import { describe, expect, it } from "vite-plus/test";
import type { FrictionPatternItem, FrictionSignal } from "../../api/generated/index.js";
import {
  FRICTION_KINDS,
  codeSegments,
  groupSignals,
  headlinePatterns,
  interruptionRows,
  sessionLabel,
  sessionLinkParams,
  signalDims,
  sortedP0,
  withoutSubjectPrefix,
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
    expect(sections.map((s) => s.kind)).toEqual(
      FRICTION_KINDS.filter((kind) => kind !== "diagnostic"),
    );
    expect(sections[0]!.signals.map((s) => s.subject_id)).toEqual(["c2", "c1"]);
    expect(sections[1]!.signals.map((s) => s.subject_id)).toEqual(["e1"]);
    expect(sections[2]!.signals).toEqual([]);
    expect(sections[4]!.signals.map((s) => s.subject_id)).toEqual(["p1"]);
    expect(sections[5]!.signals.map((s) => s.subject_id)).toEqual(["f1"]);
    expect(sections[6]!.signals.map((s) => s.subject_id)).toEqual(["i1"]);
    const diagnostics = groupSignals([
      sig({
        kind: "diagnostic",
        subject_kind: "diagnostic",
        label: "worker",
        text: "launch failed",
      }),
    ]);
    expect(diagnostics.at(-1)?.kind).toBe("diagnostic");
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

describe("sessionLinkParams", () => {
  it.each([
    ["ordinal present", 4, { msg: "4" }],
    ["ordinal zero", 0, { msg: "0" }],
    ["ordinal null (session-level pattern)", null, {}],
  ])("%s", (_name, ordinal, expected) => {
    expect(sessionLinkParams({ message_ordinal: ordinal })).toEqual(expected);
  });
});

describe("signalDims", () => {
  it.each([
    ["no dims", {}, []],
    [
      "seat and machine, without the agent shown with the session",
      { seat: "seat-02", agent: "claude", machine: "laptop" },
      ["seat-02", "laptop"],
    ],
  ])("%s", (_name, dims, expected) => {
    expect(signalDims(sig(dims))).toEqual(expected);
  });
});

describe("sessionLabel", () => {
  const session = (o: Record<string, string>) => ({ agent: "claude", project: "proj", ...o });
  it.each([
    [
      "custom name wins",
      { s: session({ display_name: "Login fix", first_message: "hi" }) },
      "Login fix",
    ],
    [
      "opening message, whitespace collapsed",
      { s: session({ first_message: "Fix the\n  flaky test" }) },
      "Fix the flaky test",
    ],
    [
      "long opening message is shortened",
      { s: session({ first_message: "x".repeat(200) }) },
      `${"x".repeat(90)}…`,
    ],
    ["project when there is no message", { s: session({}) }, "proj"],
    ["raw id for a session missing from the archive", {}, "s"],
  ])("%s", (_name, sessions, expected) => {
    expect(sessionLabel("s", sessions)).toBe(expected);
  });
});

describe("codeSegments", () => {
  it.each([
    ["plain text", "x3 identical", [{ text: "x3 identical", code: false }]],
    [
      "backticked spans become code",
      "`Bash` x4 identical arguments",
      [
        { text: "Bash", code: true },
        { text: " x4 identical arguments", code: false },
      ],
    ],
    ["an unmatched backtick stays literal", "a ` b", [{ text: "a ` b", code: false }]],
  ])("%s", (_name, text, expected) => {
    expect(codeSegments(text)).toEqual(expected);
  });
});

describe("withoutSubjectPrefix", () => {
  it.each([
    ["drops the repeated identity", "run-7:ci: nightly failed", "nightly failed"],
    ["keeps other text", "nightly failed", "nightly failed"],
  ])("%s", (_name, text, expected) => {
    expect(withoutSubjectPrefix(text, "run-7:ci")).toBe(expected);
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
  it("sorts alerts by tool", () => {
    expect(
      sortedP0([
        { tool: "read", subject_ids: ["s1", "s2", "s3"] },
        { tool: "bash", subject_ids: ["s1", "s2", "s3"] },
      ]).map((a) => a.tool),
    ).toEqual(["bash", "read"]);
  });
});
