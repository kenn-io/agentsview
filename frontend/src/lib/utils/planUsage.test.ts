import { beforeEach, describe, expect, it } from "vite-plus/test";
import type { DbPlanUsageView } from "../api/generated/index.js";
import { setLocale } from "../i18n/index.js";
import { formatTimestamp } from "./format.js";
import {
  describeCaptureCoverage,
  describePlanUsage,
  formatPlanCredits,
  formatPlanNumber,
} from "./planUsage.js";

const AS_OF = "2026-03-02T09:15:00Z";
const OUTCOME_AT = "2026-03-03T10:30:00Z";

function plan(overrides: Partial<DbPlanUsageView> = {}): DbPlanUsageView {
  return { final: false, groups: [], ...overrides };
}

const figures = {
  data_as_of: AS_OF,
  data_status: "available",
  weekly_percent: 2.6076414506625096,
  credits: "0E-10",
};

beforeEach(() => {
  setLocale("en");
});

describe("formatPlanNumber", () => {
  it("rounds to two decimals and drops trailing zeros", () => {
    expect(formatPlanNumber(2.6076414506625096)).toBe("2.61");
    expect(formatPlanNumber(12.5)).toBe("12.5");
    expect(formatPlanNumber(3)).toBe("3");
  });

  it("shows a nonzero share that rounds to zero as under 0.01", () => {
    expect(formatPlanNumber(0.001963)).toBe("<0.01");
    expect(formatPlanNumber(0)).toBe("0");
  });
});

describe("formatPlanCredits", () => {
  it("reads exponent-form decimal strings", () => {
    expect(formatPlanCredits("0E-10")).toBe("0");
    expect(formatPlanCredits("1.25E+1")).toBe("12.5");
  });

  it("shows a string that is not a number as sent", () => {
    expect(formatPlanCredits("n/a")).toBe("n/a");
  });
});

describe("describePlanUsage", () => {
  it("shows nothing for a session from another agent without plan usage", () => {
    expect(describePlanUsage("claude", null)).toBeNull();
  });

  it("says not checked yet for a codex-cloud thread with no row", () => {
    expect(describePlanUsage("codex-cloud", undefined)?.headline).toBe(
      "Plan usage: not checked yet",
    );
  });

  it.each([
    ["a row with nothing recorded yet", plan(), "Plan usage: not checked yet"],
    [
      "a pending unavailable row",
      plan({ data_as_of: AS_OF, data_status: "unavailable" }),
      `Plan usage: not reported yet, as of ${formatTimestamp(AS_OF)}`,
    ],
    [
      "a pending root the response left out",
      plan({ data_as_of: AS_OF, outcome_kind: "left_out", outcome_at: OUTCOME_AT }),
      `Plan usage: not reported yet, as of ${formatTimestamp(OUTCOME_AT)}`,
    ],
    [
      "a settled unavailable row",
      plan({ data_as_of: AS_OF, data_status: "unavailable", final: true }),
      "Not reported by OpenAI",
    ],
    [
      "a root that settled with no row",
      plan({ outcome_kind: "settled_no_row", outcome_at: OUTCOME_AT }),
      "Not reported by OpenAI",
    ],
    [
      "too many sub-agent chats",
      plan({ outcome_kind: "too_many_descendants", outcome_at: OUTCOME_AT }),
      "Too many sub-agent chats for OpenAI's usage report",
    ],
    [
      "a root that could not be checked",
      plan({ outcome_kind: "could_not_check", outcome_at: OUTCOME_AT }),
      "AgentsView could not check plan usage",
    ],
    [
      "an account without usage",
      plan({ outcome_kind: "account_unavailable", outcome_at: OUTCOME_AT }),
      "Usage isn't available for this account",
    ],
  ])("states %s without figures", (_name, view, headline) => {
    const display = describePlanUsage("codex-cloud", view);
    expect(display?.headline).toBe(headline);
    expect(display?.hasFigures).toBe(false);
    expect(display?.totals).toEqual([]);
  });

  it.each([
    ["an available row", "available", false, "Plan usage, as of"],
    ["a pending partial row", "partial", false, "Plan usage: still updating, as of"],
    ["a settled partial row", "partial", true, "Partial plan usage, as of"],
  ])("labels %s with its figures", (_name, status, final, prefix) => {
    const display = describePlanUsage(
      "codex-cloud",
      plan({ ...figures, data_status: status, final }),
    );
    expect(display?.headline).toBe(`${prefix} ${formatTimestamp(AS_OF)}`);
    expect(display?.totals).toEqual(["2.61% of the weekly Codex limit", "0 credits"]);
    expect(display?.trigger).toBe("Plan usage: 2.61% weekly");
  });

  it("lists the five-hour share and each group", () => {
    const display = describePlanUsage(
      "codex-cloud",
      plan({
        ...figures,
        five_hour_percent: 0.001963,
        credits: "1",
        groups: [
          {
            model: "model-a",
            reasoning_effort: "high",
            speed: "standard",
            product_experience: "",
            weekly_percent: 2.5,
            five_hour_percent: null,
            credits: "0.004",
          },
        ],
      }),
    );
    expect(display?.totals).toEqual([
      "2.61% of the weekly Codex limit",
      "<0.01% of the five-hour Codex limit",
      "1 credit",
    ]);
    expect(display?.groups).toEqual([
      {
        key: "0",
        label: "model-a · high · standard",
        weekly: "2.5%",
        fiveHour: "",
        credits: "<0.01",
      },
    ]);
  });

  it("keeps the row and adds a stale outcome recorded after it", () => {
    const display = describePlanUsage(
      "codex-cloud",
      plan({ ...figures, outcome_kind: "could_not_check", outcome_at: OUTCOME_AT }),
    );
    expect(display?.headline).toBe(`Plan usage, as of ${formatTimestamp(AS_OF)}`);
    expect(display?.staleNote).toBe(
      `Since then: AgentsView could not check plan usage (${formatTimestamp(OUTCOME_AT)}). ` +
        "These figures may be out of date.",
    );
    expect(display?.hasFigures).toBe(true);
  });

  it("does not promise a report for a final row the report later left out", () => {
    const display = describePlanUsage(
      "codex-cloud",
      plan({ ...figures, final: true, outcome_kind: "left_out", outcome_at: OUTCOME_AT }),
    );
    expect(display?.staleNote).toBe(
      `Since then: OpenAI's usage report left this chat out (${formatTimestamp(OUTCOME_AT)}). ` +
        "These figures may be out of date.",
    );
  });

  it("replaces the figures with the usage root for a counted-under thread", () => {
    const display = describePlanUsage(
      "codex-cloud",
      plan({
        ...figures,
        outcome_kind: "counted_under",
        outcome_at: OUTCOME_AT,
        counted_under: "codex-cloud:root-thread",
      }),
    );
    expect(display?.countedUnder).toBe("codex-cloud:root-thread");
    expect(display?.totals).toEqual([]);
    expect(display?.hasFigures).toBe(false);
  });
});

describe("describeCaptureCoverage", () => {
  const coverage = { last_capture_at: AS_OF, uncaptured_turns: 0 };
  const captured = `Tokens captured live, as of ${formatTimestamp(AS_OF)}`;

  it("shows nothing when live capture never saw the session", () => {
    expect(describeCaptureCoverage(null, plan(figures))).toEqual([]);
  });

  it("states the last capture when every turn was captured", () => {
    expect(describeCaptureCoverage(coverage, null)).toEqual([captured]);
  });

  it("counts missed turns and names the time plan usage counts up to, only with figures", () => {
    expect(describeCaptureCoverage({ ...coverage, uncaptured_turns: 3 }, plan(figures))).toEqual([
      captured,
      `3 turns not captured; plan usage counts the ones that ran before ${formatTimestamp(AS_OF)}`,
    ]);
    expect(
      describeCaptureCoverage(
        { ...coverage, uncaptured_turns: 1 },
        plan({ data_as_of: AS_OF, data_status: "unavailable" }),
      ),
    ).toEqual([captured, "1 turn not captured"]);
  });

  it("does not claim plan usage covers missed turns for a partial row", () => {
    expect(
      describeCaptureCoverage(
        { ...coverage, uncaptured_turns: 2 },
        plan({ ...figures, data_status: "partial", final: true }),
      ),
    ).toEqual([captured, "2 turns not captured"]);
  });

  it("does not claim plan usage covers missed turns after a newer failure", () => {
    for (const outcome_kind of [
      "could_not_check",
      "left_out",
      "account_unavailable",
      "too_many_descendants",
      "settled_no_row",
    ]) {
      expect(
        describeCaptureCoverage(
          { ...coverage, uncaptured_turns: 2 },
          plan({ ...figures, outcome_kind, outcome_at: OUTCOME_AT }),
        ),
      ).toEqual([captured, "2 turns not captured"]);
    }
  });
});
