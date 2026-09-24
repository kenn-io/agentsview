// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type {
  FrictionDigestResponse,
  FrictionSignal,
  VersionInfo,
} from "../../api/generated/index.js";

vi.mock("../../api/client.js", () => ({
  downloadFrictionDigestMarkdown: vi.fn().mockResolvedValue(undefined),
}));

import { friction } from "../../stores/friction.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sync } from "../../stores/sync.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import FrictionPage from "./FrictionPage.svelte";

const DATE = "2026-09-21";
const OLDER = "2026-09-20";

function sig(overrides: Partial<FrictionSignal>): FrictionSignal {
  return {
    kind: "correction",
    detector: "correction.coding",
    subject_id: "test-session-medium-8",
    subject_kind: "session",
    title: "t",
    fingerprint: `fl1:${"a".repeat(64)}`,
    text: "",
    tool_name: "",
    label: "",
    evidence: "",
    message_ordinal: null,
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

function makeDigest(signals: FrictionSignal[]): FrictionDigestResponse {
  return {
    date: DATE,
    timezone: "UTC",
    rules_version: "friction-v1",
    built_at: "2026-09-22T00:05:00Z",
    revision: 2,
    sessions_scanned: 3,
    markdown_sha256: "0".repeat(64),
    web_url: "",
    summary: {
      schema_version: 3,
      sessions_scanned: 3,
      tracker_failures: 0,
      corrections: 1,
      errors: 2,
      workarounds: 0,
      deferrals: 0,
      patterns: 1,
      frustrations: 1,
      interruptions: 2,
      p0_alerts: { bash: ["s-a", "s-b", "s-c"] },
      personas: {
        "helper@general": {
          persona: "helper",
          channel: "general",
          sessions: 2,
          corrections: 1,
          errors: 0,
          workarounds: 0,
          deferrals: 0,
          patterns: 1,
          input_tokens: 5000,
          output_tokens: 250,
          cost_usd: "0.5",
        },
      },
      spend: {
        total_usd: "4.2",
        sessions_with_stats: 3,
        sessions_with_cost: 2,
        input_tokens: 1000,
        output_tokens: 50,
        role_costs_usd: { "(root)": "1.20", subagent: "3.00" },
        model_costs_usd: { "claude-opus-5": "4.20" },
      },
      digest_path: `friction:${DATE}`,
      created_issues: [],
    },
    signals,
    p0_alerts: [{ tool: "bash", subject_ids: ["s-a", "s-b", "s-c"] }],
  } as unknown as FrictionDigestResponse;
}

const correction = sig({
  kind: "correction",
  text: "no, use the other flag <img src=x onerror=alert(1)>",
  message_ordinal: 3,
  persona: "helper",
  channel: "general",
  seat: "seat-02",
});
// A producer-written diagnostic (spec §11.4): its subject is an identity,
// not a session id.
const diagnosticError = sig({
  kind: "error",
  detector: "error",
  subject_id: "nightly-42:ci_nightly",
  subject_kind: "diagnostic",
  tool_name: "ci_nightly",
  text: "nightly-42:ci_nightly: nightly check failed",
  fingerprint: `fl1:${"b".repeat(64)}`,
});
const toolError = sig({
  kind: "error",
  detector: "error",
  tool_name: "bash",
  text: "command not found",
  message_ordinal: 5,
  fingerprint: `fl1:${"c".repeat(64)}`,
});
const patternSignal = sig({
  kind: "pattern",
  detector: "pattern.retry_loop",
  label: "retry_loop",
  evidence: "`bash` x3 identical arguments 01:35-01:54",
  fingerprint: `fl1:${"d".repeat(64)}`,
});
const frustrationSignal = sig({
  kind: "frustration",
  detector: "frustration",
  text: "this is still broken",
  message_ordinal: 6,
  fingerprint: `fl1:${"e".repeat(64)}`,
});
const interruptionA = sig({
  kind: "interruption",
  detector: "interruption",
  message_ordinal: 2,
  fingerprint: `fl1:${"f".repeat(64)}`,
});
const interruptionB = sig({
  kind: "interruption",
  detector: "interruption",
  message_ordinal: 7,
  fingerprint: `fl1:${"f".repeat(64)}`,
});

function version(overrides: Partial<VersionInfo & { friction_available: boolean }> = {}) {
  return {
    api_version: 1,
    build_date: "",
    commit: "unknown",
    data_version: 1,
    insight_generation_available: false,
    friction_available: true,
    read_only: false,
    version: "dev",
    ...overrides,
  } as VersionInfo;
}

describe("FrictionPage", () => {
  let component: ReturnType<typeof mount> | undefined;
  let loadCalls: Array<string | null | undefined>;

  beforeEach(() => {
    friction.reset();
    router.route = "friction";
    router.params = {};
    sync.serverVersion = version();
    loadCalls = [];
    vi.spyOn(friction, "load").mockImplementation(async (date) => {
      loadCalls.push(date);
    });
    friction.dates = [
      {
        date: DATE,
        timezone: "UTC",
        rules_version: "friction-v1",
        built_at: "2026-09-22T00:05:00Z",
        revision: 2,
        sessions_scanned: 3,
      },
      {
        date: OLDER,
        timezone: "UTC",
        rules_version: "friction-v1",
        built_at: "2026-09-21T00:05:00Z",
        revision: 1,
        sessions_scanned: 1,
      },
    ] as never;
    friction.selectedDate = DATE;
    friction.digest = makeDigest([
      correction,
      diagnosticError,
      toolError,
      patternSignal,
      frustrationSignal,
      interruptionA,
      interruptionB,
    ]);
    friction.patterns = [
      {
        fingerprint: patternSignal.fingerprint,
        kind: "pattern",
        title: "[friction/pattern] test-session-medium-8: retry loop",
        first_seen_date: DATE,
        last_seen_date: DATE,
        occurrence_count: 1,
        session_count: 1,
        last_subject_id: "test-session-medium-8",
        last_ordinal: null,
      },
      {
        fingerprint: toolError.fingerprint,
        kind: "error",
        title: "[friction/error] bash: command not found",
        first_seen_date: "2026-09-01",
        last_seen_date: DATE,
        occurrence_count: 7,
        session_count: 4,
        last_subject_id: "test-session-medium-8",
        last_ordinal: 5,
      },
    ] as never;
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    router.route = "sessions";
    router.params = {};
    sync.serverVersion = null;
    friction.reset();
    vi.restoreAllMocks();
  });

  async function render() {
    component = mount(FrictionPage, { target: document.body });
    await tick();
  }

  function section(kind: string): HTMLElement {
    const el = document.querySelector<HTMLElement>(
      `section[aria-labelledby="friction-section-${kind}"]`,
    );
    expect(el, `section ${kind}`).not.toBeNull();
    return el!;
  }

  it("loads the requested date from the URL on mount", async () => {
    router.params = { date: OLDER };
    await render();
    expect(loadCalls).toContain(OLDER);
  });

  it("links to the published Friction Log guide", async () => {
    await render();
    expect(document.querySelector<HTMLAnchorElement>(".friction-help-link")?.href).toBe(
      "https://agentsview.io/docs/friction-log/",
    );
  });

  it("renders summary first: heading, P0 alerts, then ranked patterns", async () => {
    await render();
    const text = document.body.textContent ?? "";
    expect(document.querySelector("h1")?.textContent).toBe("Friction Log — 2026-09-21");
    expect(text).toContain("bash failed in 3 distinct sessions");
    expect(text).toContain("s-a, s-b, s-c");
    const headline = document.querySelector(".friction-headline")!;
    expect(headline.textContent).toContain("New patterns");
    expect(headline.textContent).toContain("Recurring patterns");
    expect(headline.textContent).toContain("7 occurrences");
    const firstSection = document.querySelector("section[aria-labelledby^='friction-section-']");
    expect(
      headline.compareDocumentPosition(firstSection!) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("renders all seven kinds in digest order with placeholders when empty", async () => {
    await render();
    const order = Array.from(
      document.querySelectorAll("section[aria-labelledby^='friction-section-']"),
    ).map((el) => el.getAttribute("aria-labelledby"));
    expect(order).toEqual([
      "friction-section-correction",
      "friction-section-error",
      "friction-section-workaround",
      "friction-section-deferral",
      "friction-section-pattern",
      "friction-section-frustration",
      "friction-section-interruption",
    ]);
    expect(section("workaround").textContent).toContain("No workarounds detected.");
    expect(section("deferral").textContent).toContain("No deferrals detected.");
    expect(section("error").querySelectorAll("li")).toHaveLength(2);
    expect(section("frustration").textContent).toContain("this is still broken");
  });

  it("shows the frustration placeholder when a digest has none", async () => {
    friction.digest = makeDigest([correction]);
    await render();
    expect(section("frustration").textContent).toContain("No frustration detected.");
    expect(section("interruption").textContent).toContain("No interruptions detected.");
  });

  it("collapses interruptions to one counted row per session linked to the first", async () => {
    await render();
    const rows = section("interruption").querySelectorAll("li");
    expect(rows).toHaveLength(1);
    expect(rows[0]!.textContent).toContain("2 interruptions");
    expect(rows[0]!.querySelector("a.subject-link")?.getAttribute("href")).toBe(
      "/sessions/test-session-medium-8?msg=2",
    );
  });

  it("links a finding to its message and navigates in-app", async () => {
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    const scroll = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    await render();
    const link = section("correction").querySelector<HTMLAnchorElement>("a.subject-link")!;
    expect(link.getAttribute("href")).toBe("/sessions/test-session-medium-8?msg=3");
    const event = new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
    link.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(navigate).toHaveBeenCalledWith("test-session-medium-8", { msg: "3" });
    expect(scroll).toHaveBeenCalledWith(3, "test-session-medium-8");
  });

  it("leaves modifier clicks to the browser", async () => {
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    await render();
    const link = section("correction").querySelector<HTMLAnchorElement>("a.subject-link")!;
    let prevented: boolean | null = null;
    const observe = (e: Event) => {
      prevented = e.defaultPrevented;
      e.preventDefault();
    };
    document.addEventListener("click", observe);
    link.dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true, button: 0, ctrlKey: true }),
    );
    document.removeEventListener("click", observe);
    expect(prevented).toBe(false);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("links pattern rows to the session without a message target", async () => {
    await render();
    const link = section("pattern").querySelector<HTMLAnchorElement>("a.subject-link")!;
    expect(link.getAttribute("href")).toBe("/sessions/test-session-medium-8");
  });

  it("renders diagnostic subjects as plain text without a session link", async () => {
    await render();
    const rows = Array.from(section("error").querySelectorAll("li"));
    const diagnostic = rows.find((r) => r.textContent?.includes("nightly-42:ci_nightly"))!;
    expect(diagnostic.querySelector("a")).toBeNull();
    expect(diagnostic.querySelector("code.subject")?.textContent).toBe("nightly-42:ci_nightly");
  });

  it("links headline patterns to their first occurrence in this digest", async () => {
    await render();
    const headline = document.querySelector(".friction-headline")!;
    const recurring = Array.from(
      headline.querySelectorAll<HTMLAnchorElement>("a.pattern-title"),
    ).find((a) => a.textContent?.includes("command not found"))!;
    expect(recurring.getAttribute("href")).toBe("/sessions/test-session-medium-8?msg=5");
  });

  it("renders hostile transcript text as text", async () => {
    await render();
    expect(document.querySelector(".friction-page img")).toBeNull();
    expect(section("correction").textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("shows dims in jilog order", async () => {
    await render();
    const dims = Array.from(section("correction").querySelectorAll(".dim")).map(
      (d) => d.textContent,
    );
    expect(dims).toEqual(["helper@general", "seat:seat-02"]);
  });

  it("shows personas and spend with padded cents", async () => {
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("helper@general");
    expect(text).toContain("$0.50");
    expect(text).toContain("$4.20");
    expect(text).toContain("2 of 3 sessions with usage data");
    expect(text).toContain("(root)");
  });

  it("offers Build now only when the server can build digests", async () => {
    sync.serverVersion = version({ friction_available: false });
    await render();
    expect(document.body.textContent).not.toContain("Build now");
    await unmount(component!);
    component = undefined;

    sync.serverVersion = version({ friction_available: true });
    const build = vi.spyOn(friction, "buildNow").mockResolvedValue();
    await render();
    const button = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Build now",
    )!;
    button.click();
    expect(build).toHaveBeenCalledTimes(1);
  });

  it("hides Build now on a read-only server", async () => {
    sync.serverVersion = version({ friction_available: true, read_only: true });
    await render();
    expect(document.body.textContent).not.toContain("Build now");
  });

  it("moves to the older digest and records it in the URL", async () => {
    const select = vi.spyOn(friction, "selectDate").mockResolvedValue();
    const replace = vi.spyOn(router, "replaceParams").mockImplementation(() => {});
    await render();
    const previous = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Previous digest"]',
    )!;
    const next = document.querySelector<HTMLButtonElement>('button[aria-label="Next digest"]')!;
    expect(next.disabled).toBe(true);
    previous.click();
    expect(replace).toHaveBeenCalledWith({ date: OLDER });
    expect(select).toHaveBeenCalledWith(OLDER);
  });

  it("says digests build automatically when the server builds them but none exist yet", async () => {
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("No friction digests yet");
    expect(text).toContain("built automatically");
    expect(text).toContain("Build now");
  });

  it("explains that the Friction Log is off when the server does not build digests", async () => {
    sync.serverVersion = version({ friction_available: false });
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("Friction Log is off on this server");
    expect(text).toContain("[friction] enabled = false");
    expect(text).not.toContain("No friction digests yet");
  });

  it("still shows stored digests when the server no longer builds them", async () => {
    sync.serverVersion = version({ friction_available: false });
    await render();
    expect(document.querySelector("h1")?.textContent).toBe("Friction Log — 2026-09-21");
    expect(document.body.textContent).not.toContain("Build now");
  });

  it("shows a retryable error when the list fails", async () => {
    friction.errors.dates = "not implemented for read-only store";
    await render();
    const alert = document.querySelector('[role="alert"]')!;
    expect(alert.textContent).toContain("Could not load the Friction Log");
    expect(alert.textContent).toContain("not implemented for read-only store");
    const retry = Array.from(alert.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Retry",
    )!;
    retry.click();
    expect(loadCalls.at(-1)).toBe(DATE);
  });

  it("states that findings are heuristics", async () => {
    await render();
    expect(document.body.textContent).toContain("not ground truth");
  });
});
