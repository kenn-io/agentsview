import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ServiceFileReferenceOccurrence } from "../../api/generated/index.js";
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
beforeEach(() => { setLocale("en"); lookup.mockReset(); fileReferences.entries.clear(); });
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
