import { describe, expect, it, vi } from "vite-plus/test";
import {
  getSessionListElement,
  navigateRegisteredSessionList,
  registerSessionList,
  resolveArrowTarget,
} from "./arrow-target.js";

function makeList() {
  const list = document.createElement("div");
  list.className = "session-list-scroll";
  document.body.appendChild(list);
  return list;
}

describe("resolveArrowTarget", () => {
  it("uses focused containment before any recorded interaction", () => {
    const list = makeList();
    const row = document.createElement("button");
    list.appendChild(row);

    expect(resolveArrowTarget(row, list)).toBe("sessionList");
    list.remove();
  });

  it("uses the last deliberate interaction instead of passive pointer position", () => {
    const list = makeList();
    const row = document.createElement("button");
    list.appendChild(row);

    expect(resolveArrowTarget(row, list, "message")).toBe("message");
    expect(resolveArrowTarget(document.body, list, "sessionList")).toBe(
      "sessionList",
    );
    list.remove();
  });

  it("vetoes native editing and modal focus", () => {
    const list = makeList();
    const input = document.createElement("input");
    list.appendChild(input);
    expect(resolveArrowTarget(input, list, "sessionList")).toBe("none");

    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    const button = document.createElement("button");
    dialog.appendChild(button);
    list.appendChild(dialog);
    expect(resolveArrowTarget(button, list, "sessionList")).toBe("none");
    list.remove();
  });

  it("falls back after the live ref disconnects", () => {
    const list = makeList();
    list.remove();
    expect(resolveArrowTarget(document.body, list, "sessionList")).toBe("message");
  });

  it("keeps native and dialog vetoes when the ref is disconnected", () => {
    const list = makeList();
    const input = document.createElement("input");
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    const button = document.createElement("button");
    dialog.appendChild(button);
    list.remove();

    expect(resolveArrowTarget(input, list, "sessionList")).toBe("none");
    expect(resolveArrowTarget(button, list, "sessionList")).toBe("none");
  });

  it("uses registration as the sole source of the session-list element", () => {
    const list = makeList();
    const navigate = vi.fn();
    const detach = registerSessionList(list, navigate);

    expect(getSessionListElement()).toBe(list);
    expect(navigateRegisteredSessionList(1)).toBe(true);
    expect(navigate).toHaveBeenCalledWith(1);
    detach();
    expect(getSessionListElement()).toBeNull();
    expect(navigateRegisteredSessionList(1)).toBe(false);
    expect(resolveArrowTarget(document.body, null, "sessionList")).toBe("message");
    list.remove();
  });

  it("rejects a registered list only while its sidebar is hidden", () => {
    const sidebar = document.createElement("aside");
    const list = document.createElement("div");
    sidebar.appendChild(list);
    document.body.appendChild(sidebar);
    const navigate = vi.fn();
    const detach = registerSessionList(list, navigate);

    sidebar.style.display = "none";
    expect(resolveArrowTarget(document.body, list, "sessionList")).toBe(
      "message",
    );
    expect(navigateRegisteredSessionList(1)).toBe(false);
    expect(navigate).not.toHaveBeenCalled();

    sidebar.style.display = "flex";
    expect(resolveArrowTarget(document.body, list, "sessionList")).toBe(
      "sessionList",
    );
    expect(navigateRegisteredSessionList(1)).toBe(true);
    expect(navigate).toHaveBeenCalledWith(1);

    detach();
    sidebar.remove();
  });
});
