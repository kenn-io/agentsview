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
    summary: {
      date: DATE,
      schema_version: 1,
      sessions_scanned: 3,
      corrections: 1,
      errors: 2,
      workarounds: 0,
      deferrals: 0,
      patterns: 1,
      frustrations: 1,
      interruptions: 2,
      p0_alerts: { bash: ["s-a", "s-b", "s-c"] },
    },
    signals,
    p0_alerts: [{ tool: "bash", subject_ids: ["s-a", "s-b", "s-c"] }],
    sessions: {
      "test-session-medium-8": {
        agent: "claude",
        project: "proj-a",
        first_message: "Fix the login\n  redirect loop",
      },
      "s-a": { agent: "codex", project: "proj-b", display_name: "Release prep" },
    },
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
// A producer-written diagnostic: its subject is an identity,
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

function version(overrides: Partial<VersionInfo> = {}) {
  return {
    api_version: 1,
    build_date: "",
    commit: "unknown",
    data_version: 1,
    insight_generation_available: false,
    friction_available: true,
    friction_build_allowed: true,
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
      if (friction.errors.dates === null) friction.listLoads += 1;
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
    friction.digest.patterns = [
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
    const el = document.querySelector<HTMLElement>(`section#friction-section-${kind}`);
    expect(el, `section ${kind}`).not.toBeNull();
    return el!;
  }

  function sectionIds(): string[] {
    return Array.from(document.querySelectorAll("section[id^='friction-section-']")).map(
      (el) => el.id,
    );
  }

  it("leads with counts, then what needs attention, then the findings", async () => {
    await render();
    expect(document.querySelector("h1")?.textContent).toBe("Friction Log — 2026-09-21");
    const tiles = Array.from(document.querySelectorAll(".summary-strip .tile")).map((t) =>
      t.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(tiles).toEqual([
      "1 Corrections",
      "2 Errors",
      "0 Workarounds",
      "0 Deferrals",
      "1 Patterns",
      "1 Frustration",
      "2 Interruptions",
    ]);
    const headline = document.querySelector(".friction-headline")!;
    expect(headline.textContent).toContain("Tools failing across sessions");
    expect(headline.textContent).toContain("failed in 3 sessions");
    expect(headline.textContent).toContain("New patterns");
    expect(headline.textContent).toContain("Recurring patterns");
    expect(headline.textContent).toContain("7 occurrences");
    const firstSection = document.querySelector("section[id^='friction-section-']");
    expect(
      headline.compareDocumentPosition(firstSection!) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("names P0 sessions and falls back to the raw id for sessions not in the archive", async () => {
    await render();
    const links = Array.from(
      document.querySelectorAll<HTMLAnchorElement>(".alert-sessions a.session-link"),
    ).map((a) => a.textContent?.trim());
    expect(links).toEqual(["Release prep", "s-b", "s-c"]);
  });

  it("lists only kinds with findings, in digest order", async () => {
    await render();
    expect(sectionIds()).toEqual([
      "friction-section-correction",
      "friction-section-error",
      "friction-section-pattern",
      "friction-section-frustration",
      "friction-section-interruption",
    ]);
    expect(section("error").querySelectorAll("li")).toHaveLength(2);
    expect(section("frustration").textContent).toContain("this is still broken");
  });

  it("shows one empty state when nothing was flagged", async () => {
    friction.digest = { ...makeDigest([]), p0_alerts: [] };
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("No friction found");
    expect(text).toContain("Nothing was flagged in the 3 sessions scanned.");
    expect(sectionIds()).toEqual([]);
    expect(document.querySelector(".summary-strip")).toBeNull();
  });

  it("names a finding's session by its opening message and project", async () => {
    await render();
    const row = section("correction").querySelector("li")!;
    expect(row.querySelector("a.session-link")?.textContent?.trim()).toBe(
      "Fix the login redirect loop",
    );
    const meta = Array.from(row.querySelectorAll(".meta-part")).map((el) => el.textContent);
    expect(meta).toEqual(["proj-a", "claude", "Message 3", "seat-02"]);
  });

  it("collapses interruptions to one counted row per session linked to the first", async () => {
    await render();
    const rows = section("interruption").querySelectorAll("li");
    expect(rows).toHaveLength(1);
    expect(rows[0]!.textContent).toContain("2 interruptions");
    expect(rows[0]!.querySelector("a.session-link")?.getAttribute("href")).toBe(
      "/sessions/test-session-medium-8?msg=2",
    );
  });

  it("links a finding to its message and navigates in-app", async () => {
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    const scroll = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    await render();
    const link = section("correction").querySelector<HTMLAnchorElement>("a.session-link")!;
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
    const link = section("correction").querySelector<HTMLAnchorElement>("a.session-link")!;
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

  it("names patterns in plain language and renders backticked tools as code", async () => {
    await render();
    const row = section("pattern").querySelector("li")!;
    expect(row.textContent).toContain("Retry loop");
    expect(row.textContent).not.toContain("retry_loop");
    expect(row.textContent).not.toContain("`");
    expect(row.querySelector(".finding code")?.textContent).toBe("bash");
    expect(row.querySelector("a.session-link")?.getAttribute("href")).toBe(
      "/sessions/test-session-medium-8",
    );
  });

  it("renders diagnostic subjects once, as plain text without a session link", async () => {
    await render();
    const rows = Array.from(section("error").querySelectorAll("li"));
    const diagnostic = rows.find((r) => r.textContent?.includes("nightly check failed"))!;
    expect(diagnostic.querySelector("a")).toBeNull();
    expect(diagnostic.querySelector("code.identity")?.textContent).toBe("nightly-42:ci_nightly");
    expect(diagnostic.textContent?.split("nightly-42:ci_nightly")).toHaveLength(2);
  });

  it("links headline patterns to their first occurrence in this digest", async () => {
    await render();
    const headline = document.querySelector(".friction-headline")!;
    const recurring = Array.from(headline.querySelectorAll(".headline-row")).find((row) =>
      row.textContent?.includes("command not found"),
    )!;
    expect(recurring.textContent).not.toContain("[friction/");
    expect(recurring.querySelector("a.session-link")?.getAttribute("href")).toBe(
      "/sessions/test-session-medium-8?msg=5",
    );
  });

  describe("Kata issues", () => {
    const issue = {
      qualified_id: "agentsview#f001",
      web_url: "https://kata.example.test/issues/f001",
      issue_uid: "01J0ABCDEF0000000000000001",
      source: "created",
      linked_at: "2026-09-22T08:00:00Z",
    };

    function headlineRow(text: string): HTMLElement {
      const row = Array.from(
        document.querySelectorAll<HTMLElement>(".friction-headline .headline-row"),
      ).find((r) => r.textContent?.includes(text));
      expect(row, text).toBeDefined();
      return row!;
    }

    function buttons(row: HTMLElement): string[] {
      return Array.from(row.querySelectorAll("button")).map((b) => b.textContent?.trim() ?? "");
    }

    it("files a pattern once Kata is ready and this page may write", async () => {
      vi.spyOn(friction, "checkKata").mockImplementation(async () => {
        friction.kataReady = true;
      });
      const file = vi.spyOn(friction, "fileIssue").mockResolvedValue();
      await render();
      await tick();
      const row = headlineRow("command not found");
      expect(buttons(row)).toEqual(["File to Kata", "Link issue"]);
      Array.from(row.querySelectorAll("button"))
        .find((b) => b.textContent?.includes("File to Kata"))!
        .click();
      expect(file).toHaveBeenCalledWith(toolError.fingerprint);
    });

    it("offers no actions when Kata is not ready", async () => {
      vi.spyOn(friction, "checkKata").mockImplementation(async () => {
        friction.kataReady = false;
      });
      await render();
      await tick();
      expect(buttons(headlineRow("command not found"))).toEqual([]);
    });

    it("shows a linked issue on its pattern and findings without actions for a read-only page", async () => {
      sync.serverVersion = version({ friction_build_allowed: false });
      const check = vi.spyOn(friction, "checkKata");
      friction.digest!.patterns = friction.digest!.patterns!.map((p) =>
        p.fingerprint === toolError.fingerprint ? { ...p, issue } : p,
      );
      await render();
      const row = headlineRow("command not found");
      const link = row.querySelector<HTMLAnchorElement>("a.issue-link")!;
      expect(link.textContent?.trim()).toBe("agentsview#f001");
      expect(link.getAttribute("href")).toBe(issue.web_url);
      expect(buttons(row)).toEqual([]);
      expect(check, "a page that cannot write never asks Kata").not.toHaveBeenCalled();
      const finding = Array.from(section("error").querySelectorAll("li")).find((li) =>
        li.textContent?.includes("command not found"),
      )!;
      expect(finding.querySelector(".row-issue")?.textContent?.trim()).toBe("agentsview#f001");
    });
  });

  it("renders hostile transcript text as text", async () => {
    await render();
    expect(document.querySelector(".friction-page img")).toBeNull();
    expect(section("correction").textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("offers Build now only when the server accepts builds from this page", async () => {
    // A page proxied without auth reads digests but cannot start a build.
    sync.serverVersion = version({ friction_available: true, friction_build_allowed: false });
    await render();
    expect(document.body.textContent).not.toContain("Build now");
    await unmount(component!);
    component = undefined;

    sync.serverVersion = version();
    const build = vi.spyOn(friction, "buildNow").mockResolvedValue();
    await render();
    const button = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "Build now",
    )!;
    button.click();
    expect(build).toHaveBeenCalledTimes(1);
  });

  it("offers Build now on a read-only server that has a friction runner", async () => {
    // pg serve reports read_only for sessions but still builds digests.
    sync.serverVersion = version({ friction_available: true, read_only: true });
    await render();
    expect(document.body.textContent).toContain("Build now");
  });

  it("moves to the older digest and records it in the URL", async () => {
    const select = vi.spyOn(friction, "selectDate").mockImplementation(async (date) => {
      friction.selectedDate = date;
    });
    await render();
    const previous = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Previous digest"]',
    )!;
    const next = document.querySelector<HTMLButtonElement>('button[aria-label="Next digest"]')!;
    expect(next.disabled).toBe(true);
    previous.click();
    await tick();
    expect(router.params["date"]).toBe(OLDER);
    expect(select).toHaveBeenCalledWith(OLDER);
  });

  it("follows a date change in the URL while open", async () => {
    const select = vi.spyOn(friction, "selectDate").mockResolvedValue();
    await render();
    router.params = { date: OLDER };
    await tick();
    expect(select).toHaveBeenCalledWith(OLDER);
  });

  it("opens the URL date over a date kept from an earlier visit", async () => {
    friction.selectedDate = DATE;
    router.params = { date: OLDER };
    await render();
    await tick();
    expect(loadCalls).toEqual([OLDER]);
    expect(router.params["date"]).toBe(OLDER);
  });

  it("records the digest the page settled on in the URL", async () => {
    router.params = {};
    await render();
    await tick();
    expect(router.params["date"]).toBe(DATE);
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

  it("says where to build missing days when this page cannot build", async () => {
    sync.serverVersion = version({ friction_build_allowed: false });
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("built automatically");
    expect(text).toContain("require_auth");
    expect(text).not.toContain("Build now");
  });

  it("explains that the Friction Log is off when the server does not build digests", async () => {
    sync.serverVersion = version({ friction_available: false, friction_build_allowed: false });
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    await render();
    const text = document.body.textContent ?? "";
    expect(text).toContain("Friction Log is off on this server");
    expect(text).toContain("[friction] enabled = true");
    expect(text).not.toContain("No friction digests yet");
  });

  it("still shows stored digests when the server no longer builds them", async () => {
    sync.serverVersion = version({ friction_available: false, friction_build_allowed: false });
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
    // The date kept from an earlier visit must not replace the latest digest.
    expect(router.params["date"]).toBeUndefined();
    retry.click();
    expect(loadCalls).toEqual([null, null]);
  });

  it("retries the bookmarked date when the first load fails", async () => {
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    friction.errors.dates = "network error";
    router.params = { date: OLDER };
    await render();
    const retry = Array.from(document.querySelectorAll('[role="alert"] button')).find(
      (b) => b.textContent?.trim() === "Retry",
    ) as HTMLButtonElement;
    retry.click();
    expect(loadCalls).toEqual([OLDER, OLDER]);
  });

  it("follows the digest a successful build opened after a failed first load", async () => {
    friction.dates = [];
    friction.selectedDate = null;
    friction.digest = null;
    friction.errors.dates = "network error";
    router.params = { date: OLDER };
    await render();
    // Build now reloads through the store and opens the latest digest.
    friction.errors.dates = null;
    friction.selectedDate = DATE;
    friction.listLoads += 1;
    await tick();
    expect(router.params["date"]).toBe(DATE);
    document.querySelector<HTMLButtonElement>('button[aria-label="Refresh Friction Log"]')!.click();
    expect(loadCalls.at(-1)).toBe(DATE);
  });
});
