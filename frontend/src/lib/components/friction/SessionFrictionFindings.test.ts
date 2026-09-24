// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";

const mocks = vi.hoisted(() => ({ getApiV1FrictionFindings: vi.fn() }));

vi.mock("../../api/generated/index.js", () => ({
  FrictionService: { getApiV1FrictionFindings: mocks.getApiV1FrictionFindings },
}));

import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import SessionFrictionFindings from "./SessionFrictionFindings.svelte";

function finding(overrides: Record<string, unknown>) {
  return {
    session_id: "test-session-medium-8",
    kind: "correction",
    detector: "correction.coding",
    message_ordinal: 3,
    call_index: null,
    tool_name: "",
    label: "",
    text: "no, use the other flag",
    evidence: "",
    title: "[friction/correction] test-session-medium-8: no, use the other flag",
    fingerprint: `fl1:${"a".repeat(64)}`,
    occurred_at: null,
    seq: 0,
    rules_version: "friction-v1",
    ...overrides,
  };
}

async function flush() {
  await Promise.resolve();
  await tick();
  await Promise.resolve();
  await tick();
}

describe("SessionFrictionFindings", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    mocks.getApiV1FrictionFindings.mockReset();
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("lists findings for the session and jumps to their message", async () => {
    mocks.getApiV1FrictionFindings.mockResolvedValue({
      findings: [
        finding({}),
        finding({
          kind: "pattern",
          detector: "pattern.retry_loop",
          label: "retry_loop",
          evidence: "`bash` x3 identical arguments 01:35-01:54",
          message_ordinal: null,
          seq: 1,
        }),
        finding({
          kind: "frustration",
          detector: "frustration",
          text: "this is still broken",
          message_ordinal: 6,
          seq: 2,
        }),
        finding({
          kind: "interruption",
          detector: "interruption",
          text: "",
          message_ordinal: 7,
          seq: 3,
        }),
      ],
      next_cursor: "",
    });
    const scroll = vi.spyOn(ui, "scrollToOrdinal").mockImplementation(() => {});
    component = mount(SessionFrictionFindings, {
      target: document.body,
      props: { sessionId: "test-session-medium-8" },
    });
    await flush();

    expect(mocks.getApiV1FrictionFindings).toHaveBeenCalledWith(
      { session_id: "test-session-medium-8", limit: 500 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    const rows = document.querySelectorAll(".finding-row");
    expect(rows).toHaveLength(4);
    expect(rows[0]!.querySelector(".kind")?.textContent).toBe("Correction");
    expect(rows[0]!.textContent).toContain("correction.coding");
    expect(rows[0]!.textContent).toContain("no, use the other flag");
    expect(rows[1]!.querySelector("button")).toBeNull();
    expect(rows[2]!.querySelector(".kind")?.textContent).toBe("Frustration");
    expect(rows[2]!.textContent).toContain("this is still broken");
    expect(rows[3]!.querySelector(".kind")?.textContent).toBe("Interruption");
    expect(rows[3]!.querySelector("button")?.textContent?.trim()).toBe("Message 7");

    const jump = rows[0]!.querySelector<HTMLButtonElement>("button")!;
    expect(jump.textContent?.trim()).toBe("Message 3");
    jump.click();
    expect(scroll).toHaveBeenCalledWith(3, "test-session-medium-8");
  });

  it("says so when the session has no findings", async () => {
    mocks.getApiV1FrictionFindings.mockResolvedValue({ findings: [], next_cursor: "" });
    component = mount(SessionFrictionFindings, {
      target: document.body,
      props: { sessionId: "s1" },
    });
    await flush();
    expect(document.body.textContent).toContain("No friction findings in this session.");
  });

  it("stays quiet when findings are unavailable (read-only backend)", async () => {
    mocks.getApiV1FrictionFindings.mockRejectedValue(
      Object.assign(new Error("read only"), { status: 501 }),
    );
    component = mount(SessionFrictionFindings, {
      target: document.body,
      props: { sessionId: "s1" },
    });
    await flush();
    expect(document.body.textContent).toContain("Friction findings are unavailable.");
  });
});
