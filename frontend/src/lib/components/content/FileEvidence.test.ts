// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, unmount, tick } from "svelte";
import FileEvidence from "./FileEvidence.svelte";
import { AssetsService } from "../../api/generated/index.js";
import { SessionFileReferencesStore } from "../../stores/file-references.svelte.js";
import {
  localEvidence,
  fileOccurrence,
  fileReferencesResult,
  fileMatch,
} from "../../test/file-evidence.js";
const local = localEvidence();
let component: ReturnType<typeof mount>;
afterEach(async () => {
  vi.restoreAllMocks();
  if (component) await unmount(component);
  document.body.replaceChildren();
});
it("keeps local rows mounted through a hung lookup and safely renders remote metadata and exact links", async () => {
  let finish!: (value: import("../../api/generated/index.js").ServiceFileReferencesResult) => void;
  const store = new SessionFileReferencesStore(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  store.setSession("s1", "b1");
  component = mount(FileEvidence, { target: document.body, props: { records: [local], store } });
  const loading = store.loadWindow(0, 0);
  await tick();
  const anchor = document.querySelector('[data-occurrence="o1"]');
  expect(anchor?.textContent).toContain("report.txt");
  expect(document.querySelector("button")).toBeNull();
  finish(
    fileReferencesResult({
      occurrences: [
        fileOccurrence({
          state: "found",
          next_reference_offset: 100,
          matches: [
            fileMatch({
              node_id: 2,
              name: "<img src=x onerror=alert(1)>",
              state: "prior",
              open_url: "https://files.example.test/#node=2&version=v1",
            }),
          ],
        }),
      ],
    }),
  );
  await loading;
  await tick();
  expect(document.querySelector('[data-occurrence="o1"]')).toBe(anchor);
  expect(document.querySelector("img")).toBeNull();
  expect(document.body.textContent).toContain("<img src=x onerror=alert(1)>");
  const link = document.querySelector("a")!;
  expect(link.href).toBe("https://files.example.test/#node=2&version=v1");
  expect(link.rel).toContain("noreferrer");
  expect(document.body.textContent).toContain("Prior version");
  expect(document.body.textContent).toContain("More matches");
});
it("has no Docbank chrome when disabled and shows ineligible proof only when enabled", async () => {
  const store = new SessionFileReferencesStore();
  store.integration = "disabled";
  const records = [{ ...local, ineligible_reason: "truncated" }];
  component = mount(FileEvidence, { target: document.body, props: { records, store } });
  await tick();
  expect(document.body.textContent).toContain("report.txt");
  expect(document.body.textContent).not.toContain("Docbank");
  store.integration = "enabled";
  await tick();
  expect(document.body.textContent).toContain("Exact bytes unavailable");
});

it.each(["image/png", "image/svg+xml"])(
  "only renders allowlisted retained assets: %s",
  async (media_type) => {
    const fetch = vi
      .spyOn(AssetsService, "getApiV1AssetsByFilename")
      .mockReturnValue(new Promise(() => {}));
    const store = new SessionFileReferencesStore();
    store.integration = "disabled";
    component = mount(FileEvidence, {
      target: document.body,
      props: { records: [localEvidence({ media_type, asset_ref: "asset://abc.png" })], store },
    });
    await tick();
    expect(document.querySelector("img")?.alt ?? null).toBe(
      media_type === "image/png" ? "Transcript image: report.txt" : null,
    );
    expect(fetch).toHaveBeenCalledTimes(media_type === "image/png" ? 1 : 0);
    if (media_type === "image/png") expect(fetch).toHaveBeenCalledWith({ filename: "abc.png" });
  },
);

it("collapses additional matches and opens them when More matches is requested", async () => {
  const match = fileMatch();
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(
      fileReferencesResult({
        occurrences: [
          fileOccurrence({
            state: "found",
            matches: [match, { ...match, node_id: 2, name: "second" }],
            next_reference_offset: 100,
          }),
        ],
      }),
    )
    .mockResolvedValue(
      fileReferencesResult({
        occurrences: [
          fileOccurrence({ state: "found", matches: [{ ...match, node_id: 3, name: "third" }] }),
        ],
      }),
    );
  const store = new SessionFileReferencesStore(fetch);
  store.setSession("s1", "b1");
  await store.loadWindow(0, 0);
  component = mount(FileEvidence, { target: document.body, props: { records: [local], store } });
  await tick();
  expect(document.querySelector("details")?.open).toBe(false);
  expect(document.querySelector("summary")?.textContent).toBe("Docbank matches");
  document.querySelector<HTMLButtonElement>("button")!.click();
  await Promise.resolve();
  await Promise.resolve();
  await tick();
  expect(document.querySelector("details")?.open).toBe(true);
  expect(document.body.textContent).toContain("third");
});

it("offers a retry control when an additional matches page is unavailable", async () => {
  const match = fileMatch();
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(
      fileReferencesResult({
        occurrences: [
          fileOccurrence({ state: "found", matches: [match], next_reference_offset: 100 }),
        ],
      }),
    )
    .mockResolvedValueOnce(
      fileReferencesResult({ occurrences: [fileOccurrence({ state: "deferred" })] }),
    );
  const store = new SessionFileReferencesStore(fetch);
  store.setSession("s1", "b1");
  await store.loadWindow(0, 0);
  component = mount(FileEvidence, { target: document.body, props: { records: [local], store } });
  await tick();
  document.querySelector<HTMLButtonElement>("button")!.click();
  await Promise.resolve();
  await Promise.resolve();
  await tick();
  expect(document.querySelector<HTMLButtonElement>("button")?.textContent?.trim()).toBe(
    "Retry matches",
  );
  expect(store.occurrences.get("o1")?.next_reference_offset).toBe(100);
});
