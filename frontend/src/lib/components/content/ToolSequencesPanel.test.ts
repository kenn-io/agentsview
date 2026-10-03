// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type {
  SessionToolSequence,
  SessionToolSequenceCall,
  SessionToolSequencesResponse,
} from "../../api/generated/index.js";
import { setLocale } from "../../i18n/index.js";
import { ui } from "../../stores/ui.svelte.js";
import ToolSequencesPanel from "./ToolSequencesPanel.svelte";

function makeCall(overrides: Partial<SessionToolSequenceCall> = {}): SessionToolSequenceCall {
  return {
    ordinal: 4,
    call_index: 0,
    tool_use_id: "tool-id",
    tool_name: "Grep",
    outcome: "empty",
    repeat: "none",
    tool_changed: false,
    duration_ms: null,
    input_preview: "{}",
    input_bytes: 2,
    input_omitted_bytes: 0,
    result_preview: "No matches found",
    result_bytes: 15,
    result_omitted_bytes: 0,
    result_content_unknown: false,
    awaiting_subagent: false,
    ...overrides,
  };
}

function makeSequence(overrides: Partial<SessionToolSequence> = {}): SessionToolSequence {
  return {
    ending: "recovered",
    identical: false,
    near_identical: false,
    tool_changed: false,
    total_calls: 1,
    omitted_calls: 0,
    calls: [makeCall()],
    ...overrides,
  };
}

function makeData(
  overrides: Partial<SessionToolSequencesResponse> = {},
): SessionToolSequencesResponse {
  return {
    session_id: "session-a",
    total_tool_calls: 1,
    total_sequences: 1,
    omitted_sequences: 0,
    total_sequence_calls: 1,
    omitted_calls: 0,
    sequences: [makeSequence()],
    ...overrides,
  };
}

afterEach(() => {
  setLocale("en");
  document.body.innerHTML = "";
});

function mountPanel(
  data: SessionToolSequencesResponse | null,
  extra: { loading?: boolean; failed?: boolean; unavailable?: boolean; onretry?: () => void } = {},
) {
  return mount(ToolSequencesPanel, {
    target: document.body,
    props: { data, sessionId: "session-a", loading: false, failed: false, ...extra },
  });
}

async function openSequence(index = 0) {
  const row = document.querySelectorAll<HTMLButtonElement>(".sequence-row")[index]!;
  row.click();
  await tick();
  return row;
}

async function openCall(index: number) {
  const row = document.querySelectorAll<HTMLButtonElement>(".call-row")[index]!;
  row.click();
  await tick();
  return row;
}

describe("ToolSequencesPanel", () => {
  it("distinguishes measured zero from missing timing and shows bounded evidence", async () => {
    // The server keeps the first nine calls and the last one of a capped sequence.
    const retained = [
      makeCall({
        ordinal: 4,
        outcome: "errored",
        result_preview: "exit status 1",
        result_bytes: 10,
        result_omitted_bytes: 3,
        input_bytes: 12,
        input_omitted_bytes: 2,
      }),
      ...Array.from({ length: 8 }, (_, index) =>
        makeCall({ ordinal: 5 + index, repeat: index === 0 ? "identical" : "none" }),
      ),
      makeCall({
        ordinal: 20,
        tool_name: "Read",
        outcome: "content",
        duration_ms: 0,
        tool_changed: true,
      }),
    ];
    const data = makeData({
      total_tool_calls: 12,
      total_sequence_calls: 12,
      omitted_calls: 2,
      sequences: [
        makeSequence({
          identical: true,
          tool_changed: true,
          total_calls: 12,
          omitted_calls: 2,
          calls: retained,
        }),
      ],
    });
    const jump = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    const component = mountPanel(data);

    const row = document.querySelector<HTMLButtonElement>(".sequence-row")!;
    expect(row.getAttribute("aria-expanded")).toBe("false");
    expect(document.querySelector(".call")).toBeNull();
    const header = document.querySelector(".panel-head")!.textContent ?? "";
    expect(header).toContain("12 calls in sequences");
    expect(header).toContain("12 tool calls in session");
    expect(row.textContent).toContain("Same input repeated");
    expect(row.textContent).toContain("Tool switched");
    expect(row.textContent).toContain("Messages 4–20");
    const steps = [...row.querySelectorAll(".step")].map((step) =>
      step.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(steps).toEqual(["Grep, Error", "Grep, Empty×8", "+2 more", "Read, Content"]);
    expect(row.hasAttribute("aria-controls")).toBe(false);

    await openSequence();
    expect(row.getAttribute("aria-expanded")).toBe("true");
    expect(row.getAttribute("aria-controls")).toBe(document.querySelector(".calls")!.id);
    let text = document.body.textContent ?? "";
    expect(text).toContain("A later call returned content.");
    expect(text).toContain("12 calls in a row.");
    expect(text).toContain("Not measured");
    expect(text).toContain("0ms");
    const gap = document.querySelector(".calls > .omit")!;
    expect(gap.textContent).toContain(
      "2 more calls between message 12 and message 20 aren't shown.",
    );
    expect(gap.previousElementSibling!.querySelector(".jump")!.textContent).toContain("Message 12");
    expect(gap.nextElementSibling!.querySelector(".jump")!.textContent).toContain("Message 20");
    expect(text).toContain("2 calls in sequences aren't shown in this view.");
    expect([...document.querySelectorAll(".tag")].map((tag) => tag.textContent)).toEqual([
      "same input",
      "tool switched",
    ]);

    await openCall(0);
    text = document.body.textContent ?? "";
    expect(text).toContain("Preview shows 10 of 12 bytes.");
    expect(text).toContain("Full input is in message 4.");
    expect(text).toContain("Preview shows 7 of 10 bytes.");
    expect(text).toContain("Full result is in message 4.");
    expect(text).toContain("tool-id");
    // Narrow panels hide the duration column, so each expanded call repeats its timing.
    const durations = () =>
      [...document.querySelectorAll(".ev-duration")].map((row) =>
        row.textContent?.replace(/\s+/g, " ").trim(),
      );
    expect(durations()).toEqual(["Duration Not measured"]);
    await openCall(9);
    expect(durations()).toEqual(["Duration Not measured", "Duration 0ms"]);
    await openCall(9);

    const link = document.querySelector<HTMLAnchorElement>(
      'a.jump[aria-label="Message 20: open the Read call in the transcript"]',
    );
    expect(link).not.toBeNull();
    expect(link!.textContent).toContain("Message 20");
    expect(link!.getAttribute("href")).toContain("msg=20");
    expect(link!.closest("button")).toBeNull();
    link!.click();
    expect(jump).toHaveBeenCalledWith(20, "session-a");
    expect(document.querySelectorAll(".call-row")[9]!.getAttribute("aria-expanded")).toBe("false");
    jump.mockRestore();
    unmount(component);
  });

  it("collapses back-to-back calls with the same tool and outcome", async () => {
    const component = mountPanel(
      makeData({
        sequences: [
          makeSequence({
            total_calls: 4,
            calls: [
              makeCall({ ordinal: 2 }),
              makeCall({ ordinal: 3, repeat: "near_identical" }),
              makeCall({ ordinal: 4, outcome: "errored" }),
              makeCall({ ordinal: 5, tool_name: "Read", outcome: "content", tool_changed: true }),
            ],
          }),
        ],
      }),
    );
    await tick();

    const steps = [...document.querySelectorAll(".step")].map((step) =>
      step.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(steps).toEqual(["Grep, Empty×2", "Grep, Error", "Read, Content"]);
    await openSequence();
    expect(document.querySelectorAll(".call")).toHaveLength(4);
    const tag = document.querySelectorAll<HTMLElement>(".tag")[0]!;
    expect(tag.textContent).toBe("same input, reformatted");
    expect(tag.title).toBe("Same input with different JSON formatting");
    unmount(component);
  });

  it("keeps no calls distinct from calls without an error or empty result", async () => {
    const noCalls = mountPanel(
      makeData({ total_tool_calls: 0, total_sequences: 0, total_sequence_calls: 0, sequences: [] }),
    );
    await tick();
    expect(document.body.textContent).toContain("No tool calls recorded in this session.");
    expect(document.querySelector(".count")).toBeNull();
    unmount(noCalls);

    document.body.innerHTML = "";
    const callsWithoutSequence = mountPanel(
      makeData({ total_tool_calls: 2, total_sequences: 0, total_sequence_calls: 0, sequences: [] }),
    );
    await tick();
    expect(document.body.textContent).toContain("No tool call returned an error or empty result.");
    expect(document.querySelector(".count")!.textContent).toContain("0");
    expect(document.querySelector(".legend")).toBeNull();
    unmount(callsWithoutSequence);
  });

  it("keeps unknown explanations aligned with the displayed calls", async () => {
    const component = mountPanel(
      makeData({
        sequences: [
          makeSequence({
            ending: "unknown",
            total_calls: 3,
            calls: [
              makeCall({
                outcome: "errored",
                result_content_unknown: true,
                result_preview: "[image]",
              }),
              makeCall({
                ordinal: 4,
                call_index: 1,
                tool_name: "Read",
                outcome: "content",
                result_content_unknown: false,
                result_preview: "Found the config",
              }),
              makeCall({
                ordinal: 4,
                call_index: 2,
                tool_name: "Read",
                outcome: "unknown",
                result_content_unknown: true,
                result_preview: "[image]",
              }),
            ],
          }),
        ],
      }),
    );
    await openSequence();
    await openCall(0);
    await openCall(1);
    await openCall(2);

    const calls = document.querySelectorAll(".call");
    expect(calls[0]!.textContent).toContain("Error");
    expect(calls[0]!.textContent).not.toContain("isn't text");
    expect(calls[1]!.textContent).toContain("Content");
    expect(calls[2]!.textContent).toContain("Unknown");
    expect(calls[2]!.textContent).toContain("This result isn't text, so its outcome is unknown.");
    expect(calls[2]!.querySelector(".dot.hollow")).not.toBeNull();
    const preview = calls[2]!.querySelectorAll("pre")[1]!;
    expect(preview.tabIndex).toBe(0);
    expect(preview.getAttribute("aria-label")).toBe("Result");
    expect(document.body.textContent).toContain("The trace doesn't show how this sequence ended.");
    unmount(component);
  });

  it("describes open sequences without denying later results", async () => {
    const component = mountPanel(
      makeData({
        sequences: [
          makeSequence({
            ending: "open",
            total_calls: 2,
            calls: [
              makeCall({ outcome: "empty", result_preview: "", result_bytes: 0 }),
              makeCall({
                ordinal: 4,
                call_index: 1,
                outcome: "content",
                result_preview: "later result",
              }),
            ],
          }),
        ],
      }),
    );
    await openSequence();
    await openCall(0);
    await openCall(1);

    expect(document.body.textContent).toContain("The tool returned nothing.");
    expect(document.body.textContent).toContain("later result");
    expect(document.body.textContent).toContain("The trace ends before the sequence is resolved.");
    unmount(component);
  });

  it("does not point at a full result when no result text was retained", async () => {
    const component = mountPanel(
      makeData({
        sequences: [
          makeSequence({
            calls: [
              makeCall({ result_preview: "", result_bytes: 4096, result_omitted_bytes: 4096 }),
            ],
          }),
        ],
      }),
    );
    await openSequence();
    await openCall(0);

    const text = document.body.textContent ?? "";
    expect(text).toContain("The retained 4,096 bytes of result text are unavailable here.");
    expect(text).not.toContain("Full result is in message");
    expect(text).not.toContain("Preview shows");
    unmount(component);
  });

  it("keeps the full name of a long tool on its truncated call row", async () => {
    const tool = "mcp__agentsview__search_sessions_by_content";
    const component = mountPanel(makeData({ sequences: [makeSequence({ calls: [makeCall({ tool_name: tool })] })] }));
    await openSequence();

    const name = document.querySelector<HTMLElement>(".call-row .name")!;
    expect(name.textContent).toBe(tool);
    expect(name.title).toBe(tool);
    expect(document.querySelector(".call-row")!.textContent).toContain(tool);
    unmount(component);
  });

  it("says how many sequences are shown when the response is capped", async () => {
    const component = mountPanel(
      makeData({
        total_sequences: 26,
        omitted_sequences: 25,
        total_sequence_calls: 30,
        omitted_calls: 29,
      }),
    );
    await tick();
    expect(document.body.textContent).toContain(
      "Showing 1 of 26 sequences. The rest come later in the session.",
    );
    unmount(component);
  });

  it("renders loading and request errors separately", async () => {
    const component = mountPanel(null, { loading: true });
    await tick();
    expect(document.body.textContent).toContain("Loading tool sequences");
    expect(document.querySelector("section")!.getAttribute("aria-busy")).toBe("true");
    expect(document.querySelectorAll(".skel")).toHaveLength(3);
    unmount(component);

    document.body.innerHTML = "";
    const onretry = vi.fn();
    const failed = mountPanel(null, { failed: true, onretry });
    await tick();
    const alert = document.querySelector('[role="alert"]')!;
    expect(alert.textContent).toContain("Couldn't load tool sequences");
    alert.querySelector("button")!.click();
    expect(onretry).toHaveBeenCalledOnce();
    unmount(failed);

    document.body.innerHTML = "";
    const unavailable = mountPanel(null, { unavailable: true });
    await tick();
    expect(document.body.textContent).toContain(
      "This archive doesn't record transcript versions, so tool sequences aren't available.",
    );
    expect(document.querySelector('[role="alert"]')).toBeNull();
    unmount(unavailable);
  });
});
