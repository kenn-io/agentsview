import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { fileReferences } from "../../stores/file-references.svelte.js";
import { trackDocbank } from "./docbankTestHelpers.js";
import DocbankStatus from "./DocbankStatus.svelte";
const lookup = vi.hoisted(() => vi.fn());
vi.mock("../../api/generated/sessions/sessions.js", () => ({
  getApiV1SessionsByIdFileReferences: lookup,
}));
let component: ReturnType<typeof mount>;
afterEach(async () => {
  if (component) await unmount(component);
  document.body.innerHTML = "";
});
it("shows initial loading, failure with Retry, source failure, and a quiet ready state", async () => {
  await trackDocbank({ integration: "enabled", occurrences: [] });
  await fileReferences.track(null, null);
  let reject!: (reason: Error) => void;
  lookup.mockImplementationOnce(
    () =>
      new Promise((_, fail) => {
        reject = fail;
      }),
  );
  const pending = fileReferences.track("session-1", "new");
  component = mount(DocbankStatus, { target: document.body });
  await tick();
  expect(document.body.textContent).toContain("Checking Docbank");
  reject(new Error("offline"));
  await pending;
  await tick();
  expect(document.body.textContent).toContain("Docbank unavailable");
  lookup.mockResolvedValueOnce({ integration: "enabled", source: "unavailable", occurrences: [] });
  document.querySelector<HTMLButtonElement>("button")!.click();
  await fileReferences.track("session-1", "new");
  await tick();
  expect(document.body.textContent).toContain("Source transcript unavailable");
  expect(document.querySelector("button")).toBeNull();
  await trackDocbank({ integration: "enabled", source: "available", occurrences: [] });
  await tick();
  expect(document.body.textContent).toBe("");
});
it("hides disabled integration", async () => {
  await trackDocbank({ integration: "disabled", occurrences: [] });
  component = mount(DocbankStatus, { target: document.body });
  await tick();
  expect(document.body.textContent).toBe("");
});
