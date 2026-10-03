// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { flushSync, mount, tick, unmount } from "svelte";
import type { DbInsight, SessionToolSequencesResponse } from "../../api/generated/index.js";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import ToolEffectivenessReport from "./ToolEffectivenessReport.svelte";

const { getToolSequences, getSession, getTiming } = vi.hoisted(() => ({
  getToolSequences: vi.fn(),
  getSession: vi.fn(),
  getTiming: vi.fn(),
}));

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SessionsService: {
      getApiV1SessionsById: getSession,
      getApiV1SessionsByIdToolSequences: getToolSequences,
      getApiV1SessionsByIdTiming: getTiming,
    },
  };
});

function makeFacts(totalToolCalls: number): SessionToolSequencesResponse {
  return {
    session_id: "s1",
    total_tool_calls: totalToolCalls,
    total_sequences: 1,
    omitted_sequences: 0,
    total_sequence_calls: 1,
    omitted_calls: 0,
    sequences: [
      {
        ending: "recovered",
        identical: true,
        near_identical: false,
        tool_changed: true,
        total_calls: 1,
        omitted_calls: 0,
        calls: [
          {
            ordinal: 1,
            call_index: 0,
            tool_use_id: "g1",
            tool_name: "Grep",
            outcome: "empty",
            repeat: "none",
            tool_changed: false,
            input_preview: "{}",
            input_bytes: 2,
            input_omitted_bytes: 0,
            result_preview: "No matches found",
            result_bytes: 16,
            result_omitted_bytes: 0,
            result_content_unknown: false,
          },
        ],
      },
    ],
  };
}

function makeInsight(overrides: Partial<DbInsight> = {}): DbInsight {
  return {
    id: 7,
    type: "tool_effectiveness",
    date_from: "2026-04-26",
    date_to: "2026-04-26",
    project: "proj",
    agent: "claude",
    model: null,
    prompt: null,
    content: "## Model assessment\n\n- saved markdown text",
    created_at: "2026-04-26T10:00:00Z",
    schema_version: "tool_effectiveness.v1",
    structured_json: JSON.stringify({
      session_id: "s1",
      call_count: 3,
      conclusions: [
        {
          assessment: "did_not_help",
          text: "Repeated the empty search",
          ordinals: [1, 2],
          calls: [{ ordinal: 2, call_index: 0 }],
        },
        { assessment: "helped", text: "Read found the config", ordinals: [3], calls: [] },
      ],
      omissions: [
        {
          reason: "budget",
          ordinal: 3,
          call_index: 0,
          tool_name: "Read",
          field: "result",
          kept_bytes: 100,
          original_bytes: 4000,
        },
        {
          reason: "unretained",
          ordinal: 4,
          call_index: 1,
          tool_name: "Read",
          field: "result",
          original_bytes: 17,
        },
        { reason: "previews", count: 2 },
      ],
      transcript_revision: "rev-1",
      cited_calls: [
        {
          ordinal: 2,
          call_index: 0,
          tool_name: "Grep",
          input_preview: '{"pattern":"loadConfig","path":"/src"}',
          outcome: "empty",
          result_bytes: 0,
          message_calls: 1,
        },
        {
          ordinal: 3,
          call_index: 0,
          tool_name: "Read",
          input_preview: '{"file_path":"config.ts"}',
          outcome: "content",
          result_bytes: 4000,
          message_calls: 1,
        },
      ],
    }),
    ...overrides,
  };
}

function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    id: "s1",
    transcript_revision: "rev-1",
    display_name: "Fix config loading",
    agent: "claude",
    project: "proj",
    message_count: 12,
    ...overrides,
  };
}

function citationButtons(): HTMLButtonElement[] {
  return [...document.querySelectorAll<HTMLButtonElement>(".citations button.citation")];
}

async function settle() {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

beforeEach(() => {
  getToolSequences.mockReset().mockResolvedValue(makeFacts(3));
  getTiming.mockReset().mockResolvedValue(null);
  getSession.mockReset().mockResolvedValue(makeSession());
});

afterEach(() => {
  document.body.innerHTML = "";
});

describe("ToolEffectivenessReport", () => {
  it("lists conclusions with their cited calls above the sequences and omissions", async () => {
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    expect(getToolSequences).toHaveBeenCalledWith({ id: "s1" }, expect.anything());
    expect(document.querySelector(".report-title h3")?.textContent).toBe("Fix config loading");
    const summary = document.querySelector("[data-testid=tool-effectiveness-summary]")?.textContent ?? "";
    expect(summary).toContain("2 conclusions");
    expect(summary).toContain("1 helped");
    expect(summary).toContain("1 did not help");
    expect(summary).toContain("0 unclear");
    expect(summary).toContain("2 of 3 calls cited");

    const conclusions = document.querySelector(".conclusions");
    const panel = document.querySelector(".tool-sequences-panel.embedded");
    expect(panel).not.toBeNull();
    expect(
      conclusions!.compareDocumentPosition(panel!) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(document.querySelector("[data-testid=tool-effectiveness-session-changed]")).toBeNull();

    expect(citationButtons().map((b) => b.textContent?.replace(/\s+/g, " ").trim())).toEqual([
      "Message 1 ↗",
      "Grep loadConfig · Message 2 ↗",
      "Read config.ts · Message 3 ↗",
    ]);

    document.querySelectorAll<HTMLButtonElement>(".citation-toggle").forEach((toggle) => toggle.click());
    flushSync();
    const text = document.body.textContent ?? "";
    expect(text).toContain("Did not help");
    expect(text).toContain("Repeated the empty search");
    expect(text).toContain("model saw 100 bytes");
    expect(text).toContain("Message 3, call 0 (Read): Result cut to 100 of 4,000 bytes");
    expect(text).toContain("Message 4, call 1 (Read): result not retained (17 bytes originally)");
    expect(text).toContain("2 messages shown as 800-character previews");
    expect(text).not.toContain("saved markdown text");
    unmount(component);
  });

  it("labels calls by message and index when call details are missing", async () => {
    const insight = makeInsight();
    const raw = JSON.parse(insight.structured_json!);
    raw.cited_calls = [{ ordinal: 2 }];
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: { ...insight, structured_json: JSON.stringify(raw) } },
    });
    await settle();

    expect(citationButtons().map((b) => b.textContent?.replace(/\s+/g, " ").trim())).toEqual([
      "Message 1 ↗",
      "Message 2, call 0 ↗",
      "Message 3 ↗",
    ]);
    unmount(component);
  });

  it("labels a bare message citation by message when that message holds several calls", async () => {
    const insight = makeInsight();
    const raw = JSON.parse(insight.structured_json!);
    raw.cited_calls[1].message_calls = 2;
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: { ...insight, structured_json: JSON.stringify(raw) } },
    });
    await settle();

    expect(citationButtons().map((b) => b.textContent?.replace(/\s+/g, " ").trim())[2]).toBe("Message 3 ↗");
    unmount(component);
  });

  it("opens a conclusion's evidence on the clicked citation and toggles it closed", async () => {
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    const first = document.querySelector<HTMLLIElement>(".conclusion")!;
    expect(first.classList.contains("open")).toBe(false);
    citationButtons()[1]!.click();
    flushSync();
    expect(first.classList.contains("open")).toBe(true);
    expect(citationButtons()[1]!.classList.contains("active")).toBe(true);
    expect(citationButtons()[1]!.getAttribute("aria-pressed")).toBe("true");
    expect(citationButtons()[0]!.getAttribute("aria-pressed")).toBe("false");
    const evidenceId = first.querySelector(".evidence")!.id;
    expect(citationButtons()[1]!.getAttribute("aria-controls")).toBe(evidenceId);
    const highlighted = first.querySelector(".call.highlighted");
    expect(highlighted?.textContent).toContain("Grep");
    expect(first.querySelectorAll(".call")).toHaveLength(2);

    citationButtons()[1]!.click();
    flushSync();
    expect(first.classList.contains("open")).toBe(false);

    const toggle = first.querySelector<HTMLButtonElement>(".citation-toggle")!;
    expect(toggle.textContent).toContain("2 citations");
    toggle.click();
    flushSync();
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(first.querySelector(".call.highlighted")).toBeNull();
    unmount(component);
  });

  it("navigates to the cited message from the evidence row", async () => {
    const scroll = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    const navigate = vi.spyOn(router, "navigateToSession").mockImplementation(() => {});
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    document.querySelectorAll<HTMLButtonElement>(".citation-toggle").forEach((toggle) => toggle.click());
    flushSync();
    const jumps = [...document.querySelectorAll<HTMLAnchorElement>(".conclusions .jump")];
    const link = jumps.find((a) => a.textContent?.trim() === "Message 3 ↗")!;
    expect(link.getAttribute("aria-label")).toBe("Message 3: open the Read call in the transcript");
    expect(link.getAttribute("href")).toContain("msg=3");
    const event = new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
    link.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(scroll).toHaveBeenCalledWith(3, "s1");
    expect(navigate).toHaveBeenCalledWith("s1", { msg: "3" });

    scroll.mockRestore();
    navigate.mockRestore();
    unmount(component);
  });

  it("retries the sequence read after it fails", async () => {
    getToolSequences.mockRejectedValueOnce(new Error("offline"));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    const alert = document.querySelector(".tool-sequences-panel [role=alert]")!;
    expect(alert.textContent).toContain("Couldn't load tool sequences.");
    alert.querySelector("button")!.click();
    await settle();
    expect(getToolSequences).toHaveBeenCalledTimes(2);
    expect(document.querySelector(".tool-sequences-panel [role=alert]")).toBeNull();
    expect(document.querySelectorAll(".tool-sequences-panel .sequence-row").length).toBeGreaterThan(0);
    unmount(component);
  });

  it("starts fresh when another report for the same session is shown", async () => {
    const props = $state({ insight: makeInsight() });
    const component = mount(ToolEffectivenessReport, { target: document.body, props });
    await settle();
    citationButtons()[1]!.click();
    flushSync();
    expect(document.querySelector(".conclusion.open")).not.toBeNull();

    props.insight = makeInsight({ id: 8 });
    await settle();
    expect(getToolSequences).toHaveBeenCalledTimes(2);
    expect(document.querySelector(".conclusion.open")).toBeNull();
    unmount(component);
  });

  it("uses singular wording for single counts", async () => {
    const insight = makeInsight();
    const raw = JSON.parse(insight.structured_json!);
    raw.call_count = 1;
    raw.omissions = [{ reason: "unretained", ordinal: 4, call_index: 1, tool_name: "Read", original_bytes: 1 }];
    getToolSequences.mockResolvedValue(makeFacts(3));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: { ...insight, structured_json: JSON.stringify(raw) } },
    });
    await settle();

    const text = document.body.textContent ?? "";
    expect(text).toContain("of 1 call cited");
    expect(text).toContain("the model judged 1 call and the session now has 3");
    expect(text).toContain("(1 byte originally)");
    unmount(component);
  });

  it("tells apart identical calls that share a message", async () => {
    const insight = makeInsight();
    const raw = JSON.parse(insight.structured_json!);
    const call = { ordinal: 2, tool_name: "Grep", input_preview: "", outcome: "empty", result_bytes: 0, message_calls: 2 };
    raw.cited_calls = [{ ...call, call_index: 0 }, { ...call, call_index: 1 }];
    raw.conclusions = [{ assessment: "did_not_help", text: "Searched twice", ordinals: [2], calls: [{ ordinal: 2, call_index: 1 }] }];
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: { ...insight, structured_json: JSON.stringify(raw) } },
    });
    await settle();

    const [button] = citationButtons();
    expect(button!.textContent).toContain("Message 2, call 1");
    expect(button!.textContent).toContain("No input recorded.");
    expect(button!.title).toContain("Message 2, call 1");
    button!.click();
    flushSync();
    expect(document.querySelector(".conclusion .jump")!.textContent).toContain("Message 2, call 1");
    expect(document.querySelector(".conclusion .jump")!.getAttribute("aria-label")).toBe(
      "Message 2, call 1: open the Grep call in the transcript",
    );
    unmount(component);
  });

  it("reads the session revision after the sequences", async () => {
    let resolveFacts!: (value: SessionToolSequencesResponse) => void;
    getToolSequences.mockReturnValue(new Promise((resolve) => (resolveFacts = resolve)));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();
    expect(getSession).not.toHaveBeenCalled();

    resolveFacts(makeFacts(3));
    await settle();
    await settle();
    expect(getSession).toHaveBeenCalledOnce();
    unmount(component);
  });

  it("notes when the session has more calls than the model judged", async () => {
    getToolSequences.mockResolvedValue(makeFacts(5));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    expect(
      document.querySelector("[data-testid=tool-effectiveness-session-changed]")?.textContent,
    ).toContain("the model judged 3 calls and the session now has 5");
    unmount(component);
  });

  it("notes when the transcript changed but the call count did not", async () => {
    getSession.mockResolvedValue(makeSession({ transcript_revision: "rev-2" }));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    expect(getSession).toHaveBeenCalledWith({ id: "s1" }, expect.anything());
    expect(
      document.querySelector("[data-testid=tool-effectiveness-session-changed]")?.textContent,
    ).toContain("The session changed after this report was generated");
    unmount(component);
  });

  it("notes when the session's termination status changed", async () => {
    const insight = makeInsight();
    const raw = JSON.parse(insight.structured_json!);
    raw.termination_status = "awaiting_user";
    getSession.mockResolvedValue(makeSession({ transcript_revision: "rev-1", termination_status: "clean" }));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: { ...insight, structured_json: JSON.stringify(raw) } },
    });
    await settle();
    await settle();

    expect(
      document.querySelector("[data-testid=tool-effectiveness-session-changed]")?.textContent,
    ).toContain("The session changed after this report was generated");
    unmount(component);
  });

  it("keeps quiet when the current revision cannot be read", async () => {
    getSession.mockRejectedValue(new Error("offline"));
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight() },
    });
    await settle();

    expect(document.querySelector("[data-testid=tool-effectiveness-session-changed]")).toBeNull();
    unmount(component);
  });

  it("falls back to the saved Markdown when details are unreadable", async () => {
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight({ structured_json: "{not json" }) },
    });
    flushSync();
    await settle();

    expect(document.querySelector("[data-testid=tool-effectiveness-unavailable]")).not.toBeNull();
    expect(document.body.textContent).toContain("saved markdown text");
    expect(document.querySelector(".tool-sequences-panel")).toBeNull();
    expect(getToolSequences).not.toHaveBeenCalled();
    unmount(component);
  });

  it("treats a wrong schema version as unreadable", async () => {
    const component = mount(ToolEffectivenessReport, {
      target: document.body,
      props: { insight: makeInsight({ schema_version: "tool_effectiveness.v0" }) },
    });
    await settle();

    expect(document.querySelector("[data-testid=tool-effectiveness-unavailable]")).not.toBeNull();
    unmount(component);
  });
});
