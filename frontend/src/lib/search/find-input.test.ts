// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { ignoreShortcut, protectFindInput } from "./find-input.js";

let node: HTMLDivElement;
let input: HTMLInputElement;
let state: { composing: boolean };
let action: ReturnType<typeof protectFindInput>;

beforeEach(() => {
  node = document.createElement("div");
  input = document.createElement("input");
  node.append(input);
  document.body.append(node);
  state = { composing: false };
  action = protectFindInput(node, state);
});
afterEach(() => {
  action.destroy();
  node.remove();
});

describe("find input composition", () => {
  it.each(["Enter", "Escape", "F3", "ArrowDown"])("keeps %s in an active composition", (key) => {
    const targetHandler = vi.fn();
    input.addEventListener("keydown", targetHandler);
    input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true }));
    const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
    input.dispatchEvent(event);
    expect(state.composing).toBe(true);
    expect(targetHandler).not.toHaveBeenCalled();
    expect(event.defaultPrevented).toBe(false);
    input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true }));
    expect(state.composing).toBe(false);
    input.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));
    expect(targetHandler).toHaveBeenCalledTimes(1);
  });

  it.each([{ isComposing: true }, { keyCode: 229 }])(
    "recognizes candidate-confirmation event %j",
    (options) => {
      const targetHandler = vi.fn();
      input.addEventListener("keydown", targetHandler);
      const event = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, ...options });
      input.dispatchEvent(event);
      expect(targetHandler).not.toHaveBeenCalled();
      expect(ignoreShortcut(event)).toBe(true);
    },
  );

  it("leaves already-consumed shortcuts with the control that handled them", () => {
    const event = new KeyboardEvent("keydown", { key: "ArrowDown", cancelable: true });
    event.preventDefault();
    expect(ignoreShortcut(event)).toBe(true);
    expect(ignoreShortcut(new KeyboardEvent("keydown", { key: "ArrowDown" }))).toBe(false);
  });

  it("releases the composition state and all listeners on destruction", () => {
    input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true }));
    action.destroy();
    expect(state.composing).toBe(false);
    input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true }));
    expect(state.composing).toBe(false);
    const targetHandler = vi.fn();
    input.addEventListener("keydown", targetHandler);
    input.dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true, isComposing: true }),
    );
    expect(targetHandler).toHaveBeenCalledTimes(1);
  });
});
