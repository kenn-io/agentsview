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

describe("ToolSequencesPanel", () => {
  it("distinguishes measured zero from missing timing and shows bounded evidence", async () => {
    const data = makeData({
      total_tool_calls: 12,
      total_sequence_calls: 12,
      omitted_calls: 4,
      sequences: [
        makeSequence({
          identical: true,
          tool_changed: true,
          total_calls: 12,
          omitted_calls: 2,
          calls: [
            makeCall({
              ordinal: 4,
              outcome: "unknown",
              result_preview: "[image]",
              result_bytes: 10,
              result_omitted_bytes: 3,
              result_content_unknown: true,
              input_omitted_bytes: 2,
            }),
            makeCall({
              ordinal: 5,
              tool_name: "Read",
              duration_ms: 0,
              repeat: "identical",
              tool_changed: true,
            }),
          ],
        }),
      ],
    });
    const jump = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    const component = mount(ToolSequencesPanel, {
      target: document.body,
      props: { data, sessionId: "session-a", loading: false, failed: false },
    });

    const details = document.querySelector<HTMLDetailsElement>("details.sequence");
    expect(details).not.toBeNull();
    details!.open = true;
    await tick();
    const text = document.body.textContent ?? "";
    expect(text).toContain("A later call returned content.");
    expect(text).toContain("Not measured");
    expect(text).toContain("0ms");
    expect(text).toContain("Unknown outcome");
    expect(text).toContain("This result contains non-text evidence");
    expect(text).toContain("2 calls omitted; some tool names may be hidden");
    expect(text).toContain("4 calls omitted across the displayed sequences");
    expect(text).toContain("2 bytes omitted from the input preview");
    expect(text).toContain("3 bytes omitted from the result preview");
    expect(document.querySelectorAll(".call-fact")).toHaveLength(2);

    const callButton = document.querySelector<HTMLButtonElement>(
      '.sequence-call button[title="Open call 5 for Read in the transcript"]',
    );
    expect(callButton).not.toBeNull();
    callButton!.click();
    expect(jump).toHaveBeenCalledWith(5, "session-a");
    jump.mockRestore();
    unmount(component);
  });

  it("keeps no calls distinct from calls without an error or empty result", async () => {
    const noCalls = mount(ToolSequencesPanel, {
      target: document.body,
      props: {
        data: makeData({
          total_tool_calls: 0,
          total_sequences: 0,
          total_sequence_calls: 0,
          sequences: [],
        }),
        sessionId: "session-a",
        loading: false,
        failed: false,
      },
    });
    await tick();
    expect(document.body.textContent).toContain("No tool calls recorded.");
    unmount(noCalls);

    document.body.innerHTML = "";
    const callsWithoutSequence = mount(ToolSequencesPanel, {
      target: document.body,
      props: {
        data: makeData({
          total_tool_calls: 2,
          total_sequences: 0,
          total_sequence_calls: 0,
          sequences: [],
        }),
        sessionId: "session-a",
        loading: false,
        failed: false,
      },
    });
    await tick();
    expect(document.body.textContent).toContain("No error or empty-result sequences recorded.");
    unmount(callsWithoutSequence);
  });

  it("keeps unknown explanations aligned with the displayed calls", async () => {
    const component = mount(ToolSequencesPanel, {
      target: document.body,
      props: {
        data: makeData({
          sequences: [
            makeSequence({
              ending: "unknown",
              calls: [
                makeCall({
                  outcome: "errored",
                  result_content_unknown: true,
                  result_preview: "[image]",
                }),
                makeCall({
                  ordinal: 5,
                  tool_name: "Read",
                  outcome: "content",
                  result_content_unknown: false,
                  result_preview: "Found the config",
                }),
              ],
            }),
          ],
        }),
        sessionId: "session-a",
        loading: false,
        failed: false,
      },
    });
    document.querySelector<HTMLDetailsElement>("details.sequence")!.open = true;
    await tick();

    const calls = document.querySelectorAll(".sequence-call");
    expect(calls[0]!.textContent).toContain("Error");
    expect(calls[0]!.querySelector(".unknown-note")).toBeNull();
    expect(calls[1]!.textContent).toContain("Content");
    expect(document.body.textContent).toContain(
      "The trace does not show how this sequence ended.",
    );

    unmount(component);
  });

  it("renders loading and request errors separately", async () => {
    const component = mount(ToolSequencesPanel, {
      target: document.body,
      props: { data: null, sessionId: "session-a", loading: true, failed: false },
    });
    await tick();
    expect(document.body.textContent).toContain("Loading tool sequences");
    unmount(component);

    document.body.innerHTML = "";
    const failed = mount(ToolSequencesPanel, {
      target: document.body,
      props: { data: null, sessionId: "session-a", loading: false, failed: true },
    });
    await tick();
    expect(document.querySelector('[role="alert"]')?.textContent).toContain(
      "Couldn't load tool sequences",
    );
    unmount(failed);
  });
});
