// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { createDetailsReveal, rangeIsDisclosed } from "./details-reveal.js";

const cleanups: (() => void)[] = [];
function setup(html: string) {
  const root = document.createElement("div");
  root.innerHTML = html;
  document.body.append(root);
  const controller = createDetailsReveal(root);
  cleanups.push(() => controller.destroy());
  const rangeFor = (selector: string) => {
    const range = document.createRange();
    range.selectNodeContents(root.querySelector(selector)!);
    return range;
  };
  return { root, controller, rangeFor };
}
afterEach(() => {
  cleanups.splice(0).forEach((cleanup) => cleanup());
  document.body.replaceChildren();
});

describe("native search disclosures", () => {
  it("temporarily opens and restores a closed matching details", () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>Label</summary><p>needle</p></details>",
    );
    const range = rangeFor("p");
    expect(rangeIsDisclosed(root, range)).toBe(false);
    controller.update(range);
    expect(rangeIsDisclosed(root, range)).toBe(true);
    controller.destroy();
    expect(root.querySelector("details")!.open).toBe(false);
  });
  it("reveals nested ancestors while leaving unrelated details closed", () => {
    const { root, controller, rangeFor } = setup(
      "<details id='outer'><summary>Outer</summary><details id='inner'><summary>Inner</summary><p>needle</p></details></details><details id='other'><summary>Other</summary>needle</details>",
    );
    controller.update(rangeFor("p"));
    expect(root.querySelector<HTMLDetailsElement>("#outer")!.open).toBe(true);
    expect(root.querySelector<HTMLDetailsElement>("#inner")!.open).toBe(true);
    expect(root.querySelector<HTMLDetailsElement>("#other")!.open).toBe(false);
  });
  it("does not expand a disclosure for its visible summary", () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>needle</summary><p>body</p></details>",
    );
    const range = rangeFor("summary");
    controller.update(range);
    expect(root.querySelector("details")!.open).toBe(false);
    expect(rangeIsDisclosed(root, range)).toBe(true);
  });
  it("opens an outer disclosure for an inner summary without opening the inner body", () => {
    const { root, controller, rangeFor } = setup(
      "<details id='outer'><summary>Outer</summary><details id='inner'><summary id='hit'>needle</summary><p>body</p></details></details>",
    );
    controller.update(rangeFor("#hit"));
    expect(root.querySelector<HTMLDetailsElement>("#outer")!.open).toBe(true);
    expect(root.querySelector<HTMLDetailsElement>("#inner")!.open).toBe(false);
  });
  it("retains previously open details when search ends", () => {
    const { root, controller, rangeFor } = setup(
      "<details open><summary>Label</summary><p>needle</p></details>",
    );
    controller.update(rangeFor("p"));
    controller.destroy();
    expect(root.querySelector("details")!.open).toBe(true);
  });
  it("respects a manual close across repeated painting", () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>Label</summary><p>needle</p></details>",
    );
    const range = rangeFor("p");
    controller.update(range);
    root.querySelector("details")!.open = false;
    controller.update(range);
    controller.update(range);
    expect(root.querySelector("details")!.open).toBe(false);
    expect(rangeIsDisclosed(root, range)).toBe(false);
  });
  it("preserves a user's close and reopen on cleanup", async () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>Label</summary><p>needle</p></details>",
    );
    const range = rangeFor("p");
    controller.update(range);
    root.querySelector("summary")!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    const details = root.querySelector("details")!;
    details.open = false;
    await Promise.resolve();
    details.open = true;
    controller.update(range);
    controller.destroy();
    expect(details.open).toBe(true);
  });
  it("restores the previous automatic disclosure when the target changes", () => {
    const { root, controller, rangeFor } = setup(
      "<details id='first'><summary>First</summary><p id='a'>needle</p></details><details id='second'><summary>Second</summary><p id='b'>needle</p></details>",
    );
    controller.update(rangeFor("#a"));
    controller.update(rangeFor("#b"));
    expect(root.querySelector<HTMLDetailsElement>("#first")!.open).toBe(false);
    expect(root.querySelector<HTMLDetailsElement>("#second")!.open).toBe(true);
  });
  it("preserves open named siblings and restores group membership", () => {
    const { root, controller, rangeFor } = setup(
      "<details name='group' open id='user'><summary>User</summary>body</details><details name='group' id='auto'><summary>Auto</summary><p>needle</p></details>",
    );
    controller.update(rangeFor("p"));
    expect(root.querySelector<HTMLDetailsElement>("#user")!.open).toBe(true);
    expect(root.querySelector<HTMLDetailsElement>("#auto")!.open).toBe(true);
    controller.destroy();
    expect(root.querySelector<HTMLDetailsElement>("#user")!.open).toBe(true);
    expect(root.querySelector<HTMLDetailsElement>("#auto")!.open).toBe(false);
    expect(root.querySelector("#auto")!.getAttribute("name")).toBe("group");
  });
  it("handles matches spanning two native disclosure bodies", () => {
    const { root, controller } = setup(
      "<details><summary>A</summary><p id='a'>first</p></details><details><summary>B</summary><p id='b'>second</p></details>",
    );
    const range = document.createRange();
    range.setStart(root.querySelector("#a")!.firstChild!, 0);
    range.setEnd(root.querySelector("#b")!.firstChild!, 6);
    controller.update(range);
    expect(Array.from(root.querySelectorAll("details"), (element) => element.open)).toEqual([
      true,
      true,
    ]);
  });
  it("rejects a foreign or stale range", () => {
    const { root, controller } = setup("<details><summary>Label</summary><p>needle</p></details>");
    const other = document.createElement("p");
    other.textContent = "needle";
    document.body.append(other);
    const range = document.createRange();
    range.selectNodeContents(other);
    controller.update(range);
    expect(root.querySelector("details")!.open).toBe(false);
    expect(rangeIsDisclosed(root, range)).toBe(false);
  });
  it("does not declare a CSS-hidden target visible", () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>Label</summary><p style='display:none'>needle</p></details>",
    );
    const range = rangeFor("p");
    controller.update(range);
    expect(rangeIsDisclosed(root, range)).toBe(false);
  });
  it("accepts a visible child that overrides inherited hidden visibility", () => {
    const { root, rangeFor } = setup(
      "<div style='visibility:hidden'><p style='visibility:visible'>needle</p></div>",
    );
    expect(rangeIsDisclosed(root, rangeFor("p"))).toBe(true);
  });
  it("reveals the same match again after the next navigation creates a new owner", () => {
    const { root, controller, rangeFor } = setup(
      "<details><summary>Label</summary><p>needle</p></details>",
    );
    const range = rangeFor("p");
    controller.update(range);
    root.querySelector("details")!.open = false;
    controller.update(range);
    controller.destroy();
    const next = createDetailsReveal(root);
    cleanups.push(() => next.destroy());
    next.update(range);
    expect(rangeIsDisclosed(root, range)).toBe(true);
  });
});
