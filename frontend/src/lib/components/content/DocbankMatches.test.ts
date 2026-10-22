import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ServiceFileReferenceOccurrence } from "../../api/generated/index.js";
import { messages } from "../../stores/messages.svelte.js";
import { fileReferences } from "../../stores/file-references.svelte.js";
import { setLocale } from "../../i18n/index.js";
import DocbankMatches from "./DocbankMatches.svelte";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../../api/generated/sessions/sessions.js", () => ({ getApiV1SessionsByIdFileReferences: lookup }));
const components: ReturnType<typeof mount>[] = [];
const occurrence: ServiceFileReferenceOccurrence = {
  message_ordinal: 2, tool_use_id: "read-1", tool_name: "Read", event_index: null,
  image_index: 0, media_type: "image/png", size: 3, sha256: "a".repeat(64),
  state: "found", matches: [{ state: "current", name: "a.png", path: "/images/a.png", open_url: "https://example.com/#node=1&version=abc" }], match_total: 12,
};
async function render(items: ServiceFileReferenceOccurrence[], integration: "enabled" | "disabled" = "enabled") {
  lookup.mockResolvedValueOnce({ integration, occurrences: items });
  components.push(mount(DocbankMatches, { target: document.body, props: { sessionId: "session-a", ordinal: 2, toolUseId: "read-1" } }));
  await tick();
  await fileReferences.load("session-a");
  await tick();
}
beforeEach(() => { setLocale("en"); lookup.mockReset(); fileReferences.entries.clear(); fileReferences.enabled = true; messages.clear(); });
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  document.body.innerHTML = "";
});

describe("Docbank matches", () => {
  it("shows loading and shares a request across two blocks", async () => {
    let resolve!: (value: unknown) => void;
    lookup.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
    for (let i = 0; i < 2; i++) components.push(mount(DocbankMatches, { target: document.body, props: { sessionId: "session-a", ordinal: 2, toolUseId: "read-1" } }));
    await tick();
    expect(document.body.textContent).toContain("Checking Docbank");
    expect(lookup).toHaveBeenCalledTimes(1);
    resolve({ integration: "enabled", occurrences: [occurrence] });
    await fileReferences.load("session-a");
    await tick();
    expect(document.querySelectorAll("a")).toHaveLength(2);
  });
  it("links exact versions and shows remaining matches", async () => {
    await render([occurrence]);
    const link = document.querySelector("a")!;
    expect(link.textContent).toContain("Current · /images/a.png");
    expect(link.href).toBe("https://example.com/#node=1&version=abc");
    expect(link.rel).toBe("noreferrer");
    expect(document.body.textContent).toContain("+11 more in Docbank");
  });
  it("shows prior and trash without links when web_url is unset", async () => {
    await render([{ ...occurrence, matches: [{ state: "prior", name: "a.png", path: "/prior/a.png" }, { state: "trash", name: "deleted.png", path: "" }], match_total: 2 }]);
    expect(document.querySelector("a")).toBeNull();
    expect(document.body.textContent).toContain("Prior version · /prior/a.png");
    expect(document.body.textContent).toContain("Trash · deleted.png");
  });
  it("shows not found", async () => {
    await render([{ ...occurrence, state: "not_found", matches: [], match_total: 0 }]);
    expect(document.body.textContent).toContain("Not in Docbank");
  });
  it("retries unavailable evidence", async () => {
    await render([{ ...occurrence, state: "unavailable", matches: [], match_total: 0 }]);
    expect(document.body.textContent).toContain("Docbank unavailable");
    lookup.mockResolvedValueOnce({ integration: "enabled", occurrences: [{ ...occurrence, state: "not_found", matches: [], match_total: 0 }] });
    document.querySelector<HTMLButtonElement>("button")!.click();
    await fileReferences.load("session-a");
    await tick();
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain("Not in Docbank");
  });
  it("hides disabled integration", async () => {
    await render([], "disabled");
    expect(document.body.textContent?.trim()).toBe("");
  });
});

it("omits unknown image size without a trailing separator", async () => {
  await render([{ ...occurrence, size: null }]);
  expect(document.querySelector(".docbank-matches .muted")?.textContent).toBe("Image 1 · image/png");
});

it.each(["found", "not_found", "unavailable"] as const)("places the %s state below its image lead", async (state) => {
  await render([{ ...occurrence, state }]);
  const lead = document.querySelector(".docbank-matches .muted")!;
  expect(lead.nextElementSibling?.textContent).toContain(state === "found" ? "Current" : state === "not_found" ? "Not in Docbank" : "Docbank unavailable");
});

it("keeps source discovery quiet for blocks without known images", async () => {
  let resolve!: (value: unknown) => void;
  lookup.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
  for (let ordinal = 0; ordinal < 3; ordinal++) {
    components.push(mount(DocbankMatches, { target: document.body, props: { sessionId: "session-a", ordinal, source: true, hasImage: false, sessionStatus: ordinal === 0 } }));
  }
  await tick();
  const loadingText = document.body.textContent?.trim();
  resolve({ integration: "enabled", source: "available", occurrences: [{ ...occurrence, tool_use_id: null }] });
  await fileReferences.load("session-a");
  await tick();
  expect(loadingText).toBe("");
  expect(document.querySelectorAll(".docbank-matches")).toHaveLength(1);
  expect(document.body.textContent).toContain("Current · /images/a.png");
});

it("keeps failures and Retry beside later images while empty rows stay quiet", async () => {
  lookup.mockRejectedValueOnce(new Error("offline"));
  for (const [ordinal, hasImage] of [
    [4, true],
    [5, false],
    [6, true],
  ] as const) {
    components.push(
      mount(DocbankMatches, {
        target: document.body,
        props: { sessionId: "session-a", ordinal, source: true, hasImage },
      }),
    );
  }
  await tick();
  await fileReferences.load("session-a");
  await tick();
  expect(document.querySelectorAll(".docbank-matches")).toHaveLength(2);
  expect(document.querySelectorAll("button")).toHaveLength(2);
  lookup.mockResolvedValueOnce({ integration: "enabled", source: "available", occurrences: [] });
  document.querySelector<HTMLButtonElement>("button")!.click();
  await fileReferences.load("session-a");
  await tick();
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(document.body.textContent?.trim()).toBe("");
});

it("makes no lookup request when metadata disables Docbank", async () => {
  lookup.mockResolvedValueOnce({ integration: "disabled", occurrences: [] });
  fileReferences.enabled = false;
  components.push(
    mount(DocbankMatches, {
      target: document.body,
      props: { sessionId: "session-a", ordinal: 4, source: true, hasImage: true },
    }),
  );
  await tick();
  expect(lookup).not.toHaveBeenCalled();
  expect(document.body.textContent?.trim()).toBe("");
});

it("reports a missing source transcript without Retry", async () => {
  lookup.mockResolvedValueOnce({ integration: "enabled", source: "unavailable", occurrences: [] });
  components.push(mount(DocbankMatches, { target: document.body, props: { sessionId: "session-a", ordinal: 0, source: true, hasImage: false, sessionStatus: true } }));
  await tick();
  await fileReferences.load("session-a");
  await tick();
  expect(document.body.textContent?.trim()).toBe("Source transcript unavailable");
  expect(document.querySelector("button")).toBeNull();
});

it.each(["revision", "metadata token"])("refetches when the transcript %s changes", async (signal) => {
  messages.sessionId = "session-a";
  messages.loadedRevision = signal === "revision" ? "revision-1" : "";
  messages.activeSessionToken = "revision-1";
  lookup.mockResolvedValueOnce({ integration: "enabled", source: "available", occurrences: [] });
  components.push(
    mount(DocbankMatches, {
      target: document.body,
      props: { sessionId: "session-a", ordinal: 2, source: true, hasImage: false },
    }),
  );
  await tick();
  await fileReferences.load("session-a", "revision-1");
  lookup.mockResolvedValueOnce({
    integration: "enabled",
    source: "available",
    occurrences: [{ ...occurrence, tool_use_id: null }],
  });
  if (signal === "revision") messages.loadedRevision = "revision-2";
  else messages.activeSessionToken = "revision-2";
  await tick();
  await fileReferences.load("session-a", "revision-2");
  await tick();
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(document.body.textContent).toContain("Current · /images/a.png");
});

it("shows Retry beside a retained image when its source is unavailable", async () => {
  lookup.mockResolvedValueOnce({ integration: "enabled", source: "unavailable", occurrences: [] });
  components.push(
    mount(DocbankMatches, {
      target: document.body,
      props: { sessionId: "session-a", ordinal: 4, source: true, hasImage: true },
    }),
  );
  await tick();
  await fileReferences.load("session-a");
  await tick();
  expect(document.body.textContent).toContain("Docbank unavailable");
  expect(document.querySelector("button")?.textContent).toContain("Retry");
});

it("keeps a discovered source image actionable after a failed refresh", async () => {
 lookup.mockResolvedValueOnce({ integration: "enabled", source: "available", occurrences: [{ ...occurrence, tool_use_id: null }] });
 components.push(mount(DocbankMatches, { target: document.body, props: { sessionId: "session-a", ordinal: 2, source: true, hasImage: false } }));
 await tick();
 await fileReferences.load("session-a");
 await tick();
 expect(document.body.textContent).toContain("Current · /images/a.png");
 lookup.mockRejectedValueOnce(new Error("offline"));
 await fileReferences.refresh("session-a");
 await tick();
 expect(document.body.textContent).toContain("Docbank unavailable");
 expect(document.body.textContent).not.toContain("Current");
 expect(document.querySelector("button")?.textContent).toContain("Retry");
});
