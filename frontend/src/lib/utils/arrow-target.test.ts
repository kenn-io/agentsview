import { describe, expect, it, vi } from "vite-plus/test";
import { resolveArrowTarget } from "./arrow-target.js";

function makeList() {
  const list = document.createElement("div");
  list.className = "session-list-scroll";
  document.body.appendChild(list);
  return list;
}

describe("resolveArrowTarget", () => {
  it("uses focused containment before hover", () => {
    const list = makeList();
    const row = document.createElement("button");
    list.appendChild(row);

    expect(resolveArrowTarget(row, list, false)).toBe("sessionList");
    list.remove();
  });

  it("uses live hover only for fine pointers", () => {
    const list = makeList();
    vi.spyOn(list, "matches").mockImplementation((selector) => selector === ":hover");

    expect(resolveArrowTarget(document.body, list, true)).toBe("sessionList");
    expect(resolveArrowTarget(document.body, list, false)).toBe("message");
    list.remove();
  });

  it("vetoes native editing and modal focus", () => {
    const list = makeList();
    const input = document.createElement("input");
    list.appendChild(input);
    expect(resolveArrowTarget(input, list, true)).toBe("none");

    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    const button = document.createElement("button");
    dialog.appendChild(button);
    list.appendChild(dialog);
    expect(resolveArrowTarget(button, list, true)).toBe("none");
    list.remove();
  });

  it("falls back after the live ref disconnects", () => {
    const list = makeList();
    list.remove();
    expect(resolveArrowTarget(document.body, list, true)).toBe("message");
  });
});
