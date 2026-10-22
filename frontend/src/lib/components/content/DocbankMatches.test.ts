import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { imageOccurrence, trackDocbank } from "./docbankTestHelpers.js";
import DocbankMatches from "./DocbankMatches.svelte";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../../api/generated/sessions/sessions.js", () => ({
  getApiV1SessionsByIdFileReferences: lookup,
}));
const components: ReturnType<typeof mount>[] = [];
afterEach(async () => {
  for (const component of components.splice(0)) await unmount(component);
  document.body.innerHTML = "";
});

describe("Docbank matches", () => {
  it("links exact versions and shows remaining matches", async () => {
    await trackDocbank({
      integration: "enabled",
      occurrences: [
        {
          ...imageOccurrence,
          state: "found",
          matches: [
            {
              state: "current",
              name: "a.png",
              path: "/images/a.png",
              open_url: "https://example.com/#node=1&version=abc",
            },
          ],
          match_total: 12,
        },
      ],
    });
    components.push(
      mount(DocbankMatches, {
        target: document.body,
        props: { sessionId: "session-1", ordinal: 0 },
      }),
    );
    await tick();
    const link = document.querySelector("a")!;
    expect(link.textContent).toContain("Current · /images/a.png");
    expect(link.href).toBe("https://example.com/#node=1&version=abc");
    expect(link.rel).toBe("noreferrer");
    expect(document.body.textContent).toContain("+11 more in Docbank");
  });
  it.each(["not_found", "unavailable"] as const)(
    "shows %s without a retry button",
    async (state) => {
      await trackDocbank({ integration: "enabled", occurrences: [{ ...imageOccurrence, state }] });
      components.push(
        mount(DocbankMatches, {
          target: document.body,
          props: { sessionId: "session-1", ordinal: 0 },
        }),
      );
      await tick();
      expect(document.body.textContent).toContain(
        state === "not_found" ? "Not in Docbank" : "Docbank unavailable",
      );
      expect(document.querySelector("button")).toBeNull();
    },
  );
  it("renders nothing for empty results or another session", async () => {
    await trackDocbank({ integration: "enabled", occurrences: [imageOccurrence] });
    components.push(
      mount(DocbankMatches, { target: document.body, props: { sessionId: "other", ordinal: 0 } }),
    );
    await tick();
    expect(document.querySelector(".docbank-matches")).toBeNull();
    await trackDocbank({ integration: "enabled", occurrences: [] });
    expect(document.querySelector(".docbank-matches")).toBeNull();
  });
  it("shows prior and trash as text when web_url is absent", async () => {
    await trackDocbank({
      integration: "enabled",
      occurrences: [
        {
          ...imageOccurrence,
          state: "found",
          matches: [
            { state: "prior", name: "a.png", path: "/prior/a.png" },
            { state: "trash", name: "deleted.png", path: "" },
          ],
          match_total: 2,
        },
      ],
    });
    components.push(
      mount(DocbankMatches, {
        target: document.body,
        props: { sessionId: "session-1", ordinal: 0 },
      }),
    );
    await tick();
    expect(document.querySelector("a")).toBeNull();
    expect(document.body.textContent).toContain("Prior version · /prior/a.png");
    expect(document.body.textContent).toContain("Trash · deleted.png");
  });
});
